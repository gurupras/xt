package xfer

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/xt/internal/delta"
	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
)

// peer runs a Receiver against a scripted sender.
func peer(t *testing.T, dest string, script func(c *proto.Conn) error) error {
	t.Helper()
	sr, sw := io.Pipe()
	rr, rw := io.Pipe()
	sconn := proto.NewConn(rr, sw, sw, 0)
	rconn := proto.NewConn(sr, rw, rw, 0)
	o := &opt.Options{Recursive: true, Times: true, Perms: true}
	ui := &UI{out: io.Discard, errw: io.Discard}
	rcv := &Receiver{Conn: rconn, Opts: o, UI: ui, Dest: dest, PeerOS: runtime.GOOS}
	done := make(chan error, 1)
	go func() {
		err := script(sconn)
		sconn.Close()
		done <- err
	}()
	errc := make(chan error, 1)
	go func() {
		err := rcv.Run()
		rconn.Close()
		errc <- err
	}()
	select {
	case err := <-errc:
		sw.Close()
		rw.Close()
		<-done
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("receiver hung")
		return nil
	}
}

func list(c *proto.Conn, files ...proto.FileEntry) error {
	if err := c.Send(&proto.Msg{Files: files}); err != nil {
		return err
	}
	return c.Send(&proto.Msg{FilesEnd: true})
}

// serveX answers every request with the one-byte file "x".
func serveX(c *proto.Conn) error {
	for {
		m, err := c.Recv()
		if err != nil {
			return err
		}
		if m.Request != nil {
			c.Send(&proto.Msg{Begin: &proto.Begin{Index: m.Request.Index}})
			c.Send(&proto.Msg{Ops: []delta.Op{{Kind: delta.OpLiteral, Data: []byte("x")}}})
			sum := sha256.Sum256([]byte("x"))
			c.Send(&proto.Msg{End: &proto.End{Sum: sum[:]}})
		}
		if m.PhaseEnd {
			c.Send(&proto.Msg{PhaseEnd: true})
		}
		if m.Done != nil {
			return c.Send(&proto.Msg{Done: &proto.Stats{}})
		}
	}
}

func TestRejectsUnsafePaths(t *testing.T) {
	bad := []string{"../evil", "a/../../evil", "/etc/evil", "a//b", ""}
	for _, p := range bad {
		dest := t.TempDir()
		err := peer(t, filepath.Join(dest, "d"), func(c *proto.Conn) error {
			return list(c,
				proto.FileEntry{Path: ".", Type: proto.TypeDir, Mode: 0o755},
				proto.FileEntry{Path: p, Type: proto.TypeFile, Mode: 0o644, Size: 1})
		})
		if err == nil || !strings.Contains(err.Error(), "refusing file list") {
			t.Errorf("path %q: got %v, want refusal", p, err)
		}
	}
}

// Names Windows cannot represent are skipped, not written, and do not abort
// the rest of the transfer.
func TestWindowsBadNamesSkipped(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only")
	}
	dest := t.TempDir()
	d := filepath.Join(dest, "d")
	bad := []string{`a\..\..\evil`, "C:evil", "q?", "CON", "nul.txt", "trail.", "ads:stream"}
	files := []proto.FileEntry{{Path: ".", Type: proto.TypeDir, Mode: 0o755}}
	for _, p := range append(bad, "ok") {
		files = append(files, proto.FileEntry{Path: p, Type: proto.TypeFile, Mode: 0o644, Size: 1})
	}
	if err := peer(t, d, func(c *proto.Conn) error {
		if err := list(c, files...); err != nil {
			return err
		}
		return serveX(c)
	}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(d, "ok")); err != nil || string(b) != "x" {
		t.Fatalf("ok: %q, %v", b, err)
	}
	ents, _ := os.ReadDir(d)
	if len(ents) != 1 {
		t.Errorf("destination has %d entries, want only ok", len(ents))
	}
	if _, err := os.Lstat(filepath.Join(dest, "evil")); err == nil {
		t.Error("backslash path escaped the destination")
	}
}

func TestWinNameErr(t *testing.T) {
	for _, p := range []string{"ok", "a/b.txt", "dir/.hidden", "console", "COM", "LPTX", "a b"} {
		if err := winNameErr(p); err != nil {
			t.Errorf("%q: unexpected %v", p, err)
		}
	}
	for _, p := range []string{`a\b`, "a:b", "x/q?", "<", "pipe|", "tab\t", "trail.", "trail ",
		"CON", "con.txt", "con.d/x", "Nul", "dir/AUX.tar.gz", "COM1", "lpt9.log", "CONIN$"} {
		if err := winNameErr(p); err == nil {
			t.Errorf("%q: accepted", p)
		}
	}
}

// A symlink in the list must not let a later entry write through it.
func TestSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dest := t.TempDir()
	outside := t.TempDir()
	peer(t, filepath.Join(dest, "d"), func(c *proto.Conn) error {
		if err := list(c,
			proto.FileEntry{Path: ".", Type: proto.TypeDir, Mode: 0o755},
			proto.FileEntry{Path: "link", Type: proto.TypeSymlink, Link: outside},
			proto.FileEntry{Path: "link/pwned", Type: proto.TypeFile, Mode: 0o644, Size: 1}); err != nil {
			return err
		}
		return serveX(c)
	})
	if _, err := os.Lstat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("file was written through a symlink")
	}
}

// A corrupted transfer is retried once from scratch.
func TestChecksumMismatchRedo(t *testing.T) {
	dest := t.TempDir()
	content := []byte("the real contents")
	sum := sha256.Sum256(content)
	var requests []*proto.Request
	err := peer(t, dest, func(c *proto.Conn) error {
		if err := list(c,
			proto.FileEntry{Path: ".", Type: proto.TypeDir, Mode: 0o755},
			proto.FileEntry{Path: "f", Type: proto.TypeFile, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return err
		}
		for {
			m, err := c.Recv()
			if err != nil {
				return err
			}
			switch {
			case m.Request != nil:
				requests = append(requests, m.Request)
				data := content
				if len(requests) == 1 {
					data = []byte("corrupted content")
				}
				c.Send(&proto.Msg{Begin: &proto.Begin{Index: m.Request.Index}})
				c.Send(&proto.Msg{Ops: []delta.Op{{Kind: delta.OpLiteral, Data: data}}})
				c.Send(&proto.Msg{End: &proto.End{Sum: sum[:]}})
			case m.PhaseEnd:
				c.Send(&proto.Msg{PhaseEnd: true})
			case m.Done != nil:
				if m.Done.Errors != 0 {
					t.Errorf("receiver reported %d errors", m.Done.Errors)
				}
				return c.Send(&proto.Msg{Done: &proto.Stats{}})
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2 (original and redo)", len(requests))
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "f")); string(b) != string(content) {
		t.Fatalf("got %q", b)
	}
	if _, err := os.Stat(filepath.Join(dest, ".f.xt-partial")); !os.IsNotExist(err) {
		t.Error("partial file left behind")
	}
}
