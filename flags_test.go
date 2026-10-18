package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
)

// End-to-end tests for the rsync flags beyond the basics.

type mode struct {
	name     string
	src, dst func(string) string
}

func local(p string) string  { return p }
func remote(p string) string { return "remote:" + p }

// modes runs fn for a local copy, a push and a pull.
func modes(t *testing.T, fn func(t *testing.T, m mode)) {
	for _, m := range []mode{{"local", local, local}, {"push", local, remote}, {"pull", remote, local}} {
		t.Run(m.name, func(t *testing.T) {
			if m.name != "local" {
				needRemote(t)
			}
			fn(t, m)
		})
	}
}

func needUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestItemize(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "a"), []byte("aaaa"))
		write(t, filepath.Join(src, "sub", "b"), []byte("bbbb"))
		out := mustXT(t, "-ai", m.src(src+"/"), m.dst(dst))
		arrow := ">"
		if m.name == "push" {
			arrow = "<"
		}
		for _, want := range []string{arrow + "f+++++++++ a", "cd+++++++++ sub/", arrow + "f+++++++++ sub/b"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if strings.Contains(out, "\na\n") {
			t.Errorf("plain file name printed alongside -i:\n%s", out)
		}
		if out := mustXT(t, "-ai", m.src(src+"/"), m.dst(dst)); strings.TrimSpace(out) != "" {
			t.Errorf("no-op run printed:\n%s", out)
		}
		write(t, filepath.Join(src, "a"), []byte("AAAA"))
		touch(t, filepath.Join(src, "a"), time.Hour)
		out = mustXT(t, "-ai", m.src(src+"/"), m.dst(dst))
		if !strings.Contains(out, arrow+"f..t...... a") {
			t.Errorf("changed file not itemized as time change:\n%s", out)
		}
		os.Remove(filepath.Join(src, "sub", "b"))
		out = mustXT(t, "-ai", "--delete", m.src(src+"/"), m.dst(dst))
		if !strings.Contains(out, "*deleting   sub/b") {
			t.Errorf("deletion not itemized:\n%s", out)
		}
	})
}

func TestIgnoreTimes(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
	write(t, filepath.Join(src, "f"), []byte("aaaa"))
	mustXT(t, "-a", src+"/", dst)
	fi, _ := os.Stat(filepath.Join(src, "f"))
	write(t, filepath.Join(dst, "f"), []byte("bbbb"))
	os.Chtimes(filepath.Join(dst, "f"), fi.ModTime(), fi.ModTime())
	mustXT(t, "-a", src+"/", dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "bbbb" {
		t.Fatal("quick check should have skipped the file")
	}
	mustXT(t, "-aI", src+"/", dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "aaaa" {
		t.Fatal("-I did not transfer a file matching size and mtime")
	}
}

func TestSizeLimits(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "small"), make([]byte, 10))
		write(t, filepath.Join(src, "mid"), make([]byte, 2000))
		write(t, filepath.Join(src, "big"), make([]byte, 5000))
		mustXT(t, "-a", "--min-size=1K", "--max-size=4K", m.src(src+"/"), m.dst(dst))
		if exists(filepath.Join(dst, "small")) || exists(filepath.Join(dst, "big")) || !exists(filepath.Join(dst, "mid")) {
			t.Errorf("size limits not applied: %v", snapshot(t, dst))
		}
	})
}

func TestRemoveSourceFiles(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "a"), []byte("a"))
		write(t, filepath.Join(src, "sub", "b"), random(100_000))
		want := snapshot(t, src)
		mustXT(t, "-a", "--remove-source-files", "-n", m.src(src+"/"), m.dst(dst))
		if !exists(filepath.Join(src, "a")) {
			t.Fatal("--dry-run removed a source file")
		}
		mustXT(t, "-a", "--remove-source-files", m.src(src+"/"), m.dst(dst))
		if exists(filepath.Join(src, "a")) || exists(filepath.Join(src, "sub", "b")) {
			t.Error("source files were not removed")
		}
		if !exists(filepath.Join(src, "sub")) {
			t.Error("source directory was removed")
		}
		got := snapshot(t, dst)
		for _, k := range []string{"a", filepath.Join("sub", "b")} {
			if got[k] != want[k] {
				t.Errorf("%s not transferred intact", k)
			}
		}
	})
}

func TestBackup(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "f"), []byte("new"))
		write(t, filepath.Join(dst, "f"), []byte("old"))
		touch(t, filepath.Join(src, "f"), time.Hour)
		write(t, filepath.Join(dst, "gone"), []byte("gone"))
		mustXT(t, "-ab", "--delete", m.src(src+"/"), m.dst(dst))
		for p, want := range map[string]string{"f": "new", "f~": "old", "gone~": "gone"} {
			if b, err := os.ReadFile(filepath.Join(dst, p)); err != nil || string(b) != want {
				t.Errorf("%s = %q, %v; want %q", p, b, err, want)
			}
		}
		if exists(filepath.Join(dst, "gone")) {
			t.Error("deleted file still present")
		}
		// A second --delete run must not delete the backups.
		mustXT(t, "-ab", "--delete", m.src(src+"/"), m.dst(dst))
		if !exists(filepath.Join(dst, "f~")) {
			t.Error("--delete removed a backup file")
		}

		write(t, filepath.Join(src, "f"), []byte("newer"))
		touch(t, filepath.Join(src, "f"), 2*time.Hour)
		mustXT(t, "-a", "--delete", "--backup-dir=../bak", "--suffix=.1", m.src(src+"/"), m.dst(dst))
		if b, _ := os.ReadFile(filepath.Join(d, "bak", "f.1")); string(b) != "new" {
			t.Errorf("--backup-dir copy = %q, want \"new\"", b)
		}
	})
}

func TestBwLimit(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "f"), random(300_000))
		start := time.Now()
		mustXT(t, "-a", "--bwlimit=200K", m.src(src+"/"), m.dst(dst))
		if el := time.Since(start); el < time.Second {
			t.Errorf("300 kB at 200 KiB/s took only %v", el)
		}
		sameTree(t, src, dst)
	})
}

// TestTimeoutClient uses a remote shell that completes the handshake and
// then goes silent.
func TestTimeoutClient(t *testing.T) {
	needRemote(t)
	d := t.TempDir()
	hang := filepath.Join(d, "hang")
	os.WriteFile(hang, []byte("#!/bin/sh\nprintf 'XTPROTO "+strconv.Itoa(proto.Version)+" linux 0\\n'\nexec sleep 60\n"), 0o755)
	write(t, filepath.Join(d, "f"), []byte("x"))
	start := time.Now()
	cmd := exec.Command(xtBin, "-e", hang, "--timeout=1", filepath.Join(d, "f"), "remote:"+filepath.Join(d, "g"))
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 30 || !strings.Contains(string(out), "timeout") {
		t.Fatalf("err %v, want exit 30\n%s", err, out)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("timeout took %v", el)
	}
}

// TestTimeoutServer starts a server and then never sends it anything.
func TestTimeoutServer(t *testing.T) {
	needUnix(t)
	cmd := exec.Command(xtBin, "--server")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	br := bufio.NewReader(stdout)
	if _, err := proto.ReadHello(br, nil); err != nil {
		t.Fatal(err)
	}
	proto.WriteHello(stdin, 0)
	conn := proto.NewConn(br, stdin, stdin, 0)
	o, _ := opt.Parse([]string{"-a", "--timeout=1", "x", "y"})
	if err := conn.Send(&proto.Msg{Start: &proto.Start{Opts: *o, Role: "receiver", Paths: []string{t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	go func() {
		// Recv hides keepalives; it returns when the server exits.
		for {
			if _, err := conn.Recv(); err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 30 {
			t.Fatalf("server exit %v, want 30", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not time out")
	}
}

func TestRelative(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		makeTree(t, src)
		mustXT(t, "-aR", m.src(src+"/./sub/deep"), m.dst(dst))
		if b, _ := os.ReadFile(filepath.Join(dst, "sub", "deep", "c.txt")); string(b) != "deep\n" {
			t.Errorf("-R with /./ did not keep the path: %v", snapshot(t, dst))
		}
		if exists(filepath.Join(dst, "sub", "b.bin")) {
			t.Error("-R copied a sibling of the source")
		}
		dst2 := filepath.Join(d, "d2")
		mustXT(t, "-aR", m.src(filepath.Join(src, "a.txt")), m.dst(dst2))
		// The path is kept without its leading slash (or drive on Windows).
		rel := strings.TrimPrefix(src, filepath.VolumeName(src))
		if !exists(filepath.Join(dst2, rel, "a.txt")) {
			t.Errorf("-R with an absolute path: %v", snapshot(t, dst2))
		}
	})
}

func TestFilesFrom(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		makeTree(t, src)
		list := filepath.Join(d, "list")
		os.WriteFile(list, []byte("a.txt\n# not a file\nsub/deep/c.txt\nemptydir\n"), 0o644)
		mustXT(t, "-a", "--files-from="+list, m.src(src), m.dst(dst))
		got := snapshot(t, dst)
		for _, k := range []string{"a.txt", filepath.Join("sub", "deep", "c.txt"), "emptydir"} {
			if _, ok := got[k]; !ok {
				t.Errorf("%s missing: %v", k, got)
			}
		}
		if _, ok := got[filepath.Join("sub", "b.bin")]; ok {
			t.Error("--files-from copied an unlisted file")
		}

		dst0 := filepath.Join(d, "d0")
		os.WriteFile(list, []byte("a.txt\x00sub\x00"), 0o644)
		mustXT(t, "-a", "-r", "-0", "--files-from="+list, m.src(src), m.dst(dst0))
		if !exists(filepath.Join(dst0, "a.txt")) || !exists(filepath.Join(dst0, "sub", "deep", "c.txt")) {
			t.Errorf("--from0 with -r: %v", snapshot(t, dst0))
		}
	})
}

func TestChmod(t *testing.T) {
	needUnix(t)
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "sub", "f"), []byte("x"))
		mustXT(t, "-a", "--chmod=D750,F600", m.src(src+"/"), m.dst(dst))
		if p := perm(t, filepath.Join(dst, "sub")); p != 0o750 {
			t.Errorf("dir perm %o, want 750", p)
		}
		if p := perm(t, filepath.Join(dst, "sub", "f")); p != 0o600 {
			t.Errorf("file perm %o, want 600", p)
		}
	})
}

func TestCVSExclude(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
	write(t, filepath.Join(src, "main.c"), []byte("x"))
	write(t, filepath.Join(src, "main.o"), []byte("x"))
	write(t, filepath.Join(src, "notes~"), []byte("x"))
	write(t, filepath.Join(src, ".git", "HEAD"), []byte("x"))
	write(t, filepath.Join(src, "keep.o"), []byte("x"))
	mustXT(t, "-aC", "--include=keep.o", src+"/", dst)
	got := snapshot(t, dst)
	for _, k := range []string{"main.o", "notes~", ".git"} {
		if _, ok := got[k]; ok {
			t.Errorf("-C copied %s", k)
		}
	}
	if _, ok := got["main.c"]; !ok {
		t.Error("-C excluded main.c")
	}
	if _, ok := got["keep.o"]; !ok {
		t.Error("user --include should win over -C")
	}
}

func TestHardLinks(t *testing.T) {
	needUnix(t)
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "a"), random(50_000))
		os.Link(filepath.Join(src, "a"), filepath.Join(src, "b"))
		os.MkdirAll(filepath.Join(src, "sub"), 0o755)
		os.Link(filepath.Join(src, "a"), filepath.Join(src, "sub", "c"))
		write(t, filepath.Join(src, "solo"), []byte("solo"))
		out := mustXT(t, "-aH", "--stats", m.src(src+"/"), m.dst(dst))
		sameTree(t, src, dst)
		a, _ := os.Stat(filepath.Join(dst, "a"))
		for _, p := range []string{"b", filepath.Join("sub", "c")} {
			fi, _ := os.Stat(filepath.Join(dst, p))
			if !os.SameFile(a, fi) {
				t.Errorf("%s is not a hard link to a", p)
			}
		}
		if n := stat(t, out, "Number of regular files transferred"); n != 2 {
			t.Errorf("transferred %d files, want 2 (a and solo)", n)
		}
		// Without -H the links are separate files.
		dst2 := filepath.Join(d, "d2")
		mustXT(t, "-a", m.src(src+"/"), m.dst(dst2))
		a2, _ := os.Stat(filepath.Join(dst2, "a"))
		b2, _ := os.Stat(filepath.Join(dst2, "b"))
		if os.SameFile(a2, b2) {
			t.Error("hard link preserved without -H")
		}
		// A rerun is a no-op and keeps the links.
		mustXT(t, "-aH", m.src(src+"/"), m.dst(dst))
		b, _ := os.Stat(filepath.Join(dst, "b"))
		a, _ = os.Stat(filepath.Join(dst, "a"))
		if !os.SameFile(a, b) {
			t.Error("rerun broke the hard link")
		}
	})
}

// holesSupported reports whether the filesystem under dir keeps holes.
func holesSupported(t *testing.T, dir string) bool {
	p := filepath.Join(dir, "probe")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{1}, 4<<20)
	f.Close()
	defer os.Remove(p)
	return blocks(t, p) < 1<<20
}

func TestSparse(t *testing.T) {
	needUnix(t)
	if !holesSupported(t, t.TempDir()) {
		t.Skip("the temp filesystem does not support sparse files")
	}
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		data := make([]byte, 4<<20)
		copy(data[1<<20:], random(5000))
		copy(data[len(data)-10:], "tail")
		write(t, filepath.Join(src, "f"), data)
		write(t, filepath.Join(src, "zeros"), make([]byte, 1<<20))
		mustXT(t, "-aS", m.src(src+"/"), m.dst(dst))
		sameTree(t, src, dst)
		if b := blocks(t, filepath.Join(dst, "f")); b > 1<<20 {
			t.Errorf("sparse copy uses %d bytes on disk", b)
		}
		if fi, _ := os.Stat(filepath.Join(dst, "zeros")); fi.Size() != 1<<20 {
			t.Errorf("all-zero file has size %d", fi.Size())
		}
	})
}

func TestPruneEmptyDirs(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
	write(t, filepath.Join(src, "keep", "f"), []byte("x"))
	os.MkdirAll(filepath.Join(src, "empty", "nested"), 0o755)
	write(t, filepath.Join(src, "excl", "f.o"), []byte("x"))
	mustXT(t, "-am", "--exclude=*.o", src+"/", dst)
	got := snapshot(t, dst)
	if _, ok := got[filepath.Join("keep", "f")]; !ok {
		t.Error("-m dropped a non-empty dir")
	}
	for _, k := range []string{"empty", "excl"} {
		if _, ok := got[k]; ok {
			t.Errorf("-m kept %s", k)
		}
	}
}

func TestOmitDirTimes(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "s")
	write(t, filepath.Join(src, "sub", "f"), []byte("x"))
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(src, "sub"), old, old)
	for _, c := range []struct {
		flag string
		kept bool
	}{{"-a", true}, {"-aO", false}} {
		dst := filepath.Join(d, "d"+c.flag)
		mustXT(t, c.flag, src+"/", dst)
		fi, _ := os.Stat(filepath.Join(dst, "sub"))
		if kept := fi.ModTime().Unix() == old.Unix(); kept != c.kept {
			t.Errorf("%s: dir mtime kept = %v", c.flag, kept)
		}
	}
}

func TestExecutability(t *testing.T) {
	needUnix(t)
	d := t.TempDir()
	src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
	write(t, filepath.Join(src, "run"), []byte("#!/bin/sh\n"))
	write(t, filepath.Join(dst, "run"), []byte("old"))
	os.Chmod(filepath.Join(src, "run"), 0o750)
	os.Chmod(filepath.Join(dst, "run"), 0o604)
	mustXT(t, "-rtE", src+"/", dst)
	// -E adds x where the existing file has r; no other bits change.
	if p := perm(t, filepath.Join(dst, "run")); p != 0o705 {
		t.Errorf("perm %o, want 705", p)
	}
	os.Chmod(filepath.Join(src, "run"), 0o640)
	touch(t, filepath.Join(src, "run"), time.Hour)
	mustXT(t, "-rtE", src+"/", dst)
	if p := perm(t, filepath.Join(dst, "run")); p != 0o604 {
		t.Errorf("perm %o after source lost x, want 604", p)
	}
}

func TestListOnly(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		if m.name == "push" {
			t.Skip("listing has no destination")
		}
		d := t.TempDir()
		src := filepath.Join(d, "s")
		makeTree(t, src)
		out := mustXT(t, "-a", "--list-only", m.src(src+"/"))
		for _, want := range []string{" a.txt\n", "sub/deep/c.txt", "          6 ", "drwx"} {
			if !strings.Contains(out, want) {
				t.Errorf("listing lacks %q:\n%s", want, out)
			}
		}
		// A single argument lists one level, like rsync.
		out = mustXT(t, m.src(src+"/"))
		if !strings.Contains(out, " sub\n") || strings.Contains(out, "sub/b.bin") {
			t.Errorf("single-arg listing:\n%s", out)
		}
		out = mustXT(t, "-r", "--list-only", m.src(src+"/"), m.dst(filepath.Join(d, "never")))
		if !strings.Contains(out, "sub/b.bin") || exists(filepath.Join(d, "never")) {
			t.Errorf("--list-only with a dest:\n%s", out)
		}
	})
}

func TestMkpath(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		write(t, filepath.Join(d, "s", "f"), []byte("x"))
		deep := filepath.Join(d, "a", "b", "c")
		if r := xt(t, "-a", m.src(filepath.Join(d, "s")+"/"), m.dst(deep)); r.code == 0 || !strings.Contains(r.out, "--mkpath") {
			t.Errorf("missing parents: exit %d\n%s", r.code, r.out)
		}
		mustXT(t, "-a", "--mkpath", m.src(filepath.Join(d, "s")+"/"), m.dst(deep))
		if !exists(filepath.Join(deep, "f")) {
			t.Error("--mkpath did not create the destination")
		}
		// A single file to a missing nested path.
		mustXT(t, "-a", "--mkpath", m.src(filepath.Join(d, "s", "f")), m.dst(filepath.Join(d, "x", "y", "g")))
		if b, _ := os.ReadFile(filepath.Join(d, "x", "y", "g")); string(b) != "x" {
			t.Error("--mkpath single file")
		}
	})
}

func TestDeleteAfter(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "zz-new"), []byte("x"))
		write(t, filepath.Join(dst, "aa-old"), []byte("x"))
		out := mustXT(t, "-av", "--delete-after", m.src(src+"/"), m.dst(dst))
		del, xfer := strings.Index(out, "deleting aa-old"), strings.Index(out, "zz-new")
		if del < 0 || xfer < 0 || del < xfer {
			t.Errorf("deletion should come after the transfer:\n%s", out)
		}
		if exists(filepath.Join(dst, "aa-old")) {
			t.Error("extraneous file not deleted")
		}
	})
}

func TestMaxDelete(t *testing.T) {
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "keep"), []byte("x"))
		for _, n := range []string{"x1", "x2", "x3"} {
			write(t, filepath.Join(dst, n), []byte("x"))
		}
		r := xt(t, "-a", "--delete", "--max-delete=1", m.src(src+"/"), m.dst(dst))
		if r.code != 25 || !strings.Contains(r.out, "--max-delete") {
			t.Fatalf("exit %d, want 25\n%s", r.code, r.out)
		}
		left := 0
		for _, n := range []string{"x1", "x2", "x3"} {
			if exists(filepath.Join(dst, n)) {
				left++
			}
		}
		if left != 2 {
			t.Errorf("%d extraneous files left, want 2", left)
		}
		if !exists(filepath.Join(dst, "keep")) {
			t.Error("transfer did not happen")
		}
		r = xt(t, "-a", "--delete", "--max-delete=0", m.src(src+"/"), m.dst(dst))
		if r.code != 25 {
			t.Errorf("--max-delete=0: exit %d", r.code)
		}
	})
}

// TestIOErrorBlocksDelete checks that an unreadable source directory does
// not make --delete wipe the matching destination directory.
func TestIOErrorBlocksDelete(t *testing.T) {
	needUnix(t)
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	modes(t, func(t *testing.T, m mode) {
		d := t.TempDir()
		src, dst := filepath.Join(d, "s"), filepath.Join(d, "d")
		write(t, filepath.Join(src, "locked", "f"), []byte("x"))
		write(t, filepath.Join(src, "ok"), []byte("x"))
		mustXT(t, "-a", m.src(src+"/"), m.dst(dst))
		write(t, filepath.Join(dst, "extra"), []byte("x"))
		os.Chmod(filepath.Join(src, "locked"), 0)
		defer os.Chmod(filepath.Join(src, "locked"), 0o755)
		// The directory's mode (000) is copied too.
		defer os.Chmod(filepath.Join(dst, "locked"), 0o755)
		r := xt(t, "-a", "--delete", m.src(src+"/"), m.dst(dst))
		os.Chmod(filepath.Join(dst, "locked"), 0o755)
		if r.code != 23 || !strings.Contains(r.out, "skipping file deletion") {
			t.Errorf("exit %d, want 23 with a deletion warning\n%s", r.code, r.out)
		}
		if !exists(filepath.Join(dst, "locked", "f")) || !exists(filepath.Join(dst, "extra")) {
			t.Error("files were deleted despite the I/O error")
		}
		r = xt(t, "-a", "--delete", "--ignore-errors", m.src(src+"/"), m.dst(dst))
		if r.code != 23 {
			t.Errorf("--ignore-errors: exit %d, want 23\n%s", r.code, r.out)
		}
		if exists(filepath.Join(dst, "extra")) {
			t.Error("--ignore-errors did not delete")
		}
	})
}

func TestDirsContents(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "s")
	makeTree(t, src)
	dst := filepath.Join(d, "d")
	mustXT(t, "-d", "-t", src+"/", dst)
	got := snapshot(t, dst)
	for _, k := range []string{"a.txt", "sub", "emptydir"} {
		if _, ok := got[k]; !ok {
			t.Errorf("-d dir/ missing %s: %v", k, got)
		}
	}
	if _, ok := got[filepath.Join("sub", "b.bin")]; ok {
		t.Error("-d recursed")
	}
	dst2 := filepath.Join(d, "d2")
	mustXT(t, "-d", src, dst2)
	if got := snapshot(t, dst2); len(got) != 1 || got["s"] != "dir" {
		t.Errorf("-d dir: %v", got)
	}
}

func TestIPVersionFlag(t *testing.T) {
	needRemote(t)
	d := t.TempDir()
	argsLog := filepath.Join(d, "args")
	rsh := filepath.Join(d, "ssh")
	os.WriteFile(rsh, []byte("#!/bin/sh\necho \"$@\" > "+argsLog+"\nshift 2\nexec \"$@\"\n"), 0o755)
	write(t, filepath.Join(d, "f"), []byte("x"))
	cmd := exec.Command(xtBin, "-4", "-e", rsh, "--xt-path", xtBin, filepath.Join(d, "f"), "remote:"+filepath.Join(d, "g"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(argsLog); !strings.HasPrefix(string(b), "-4 remote ") {
		t.Errorf("ssh args = %q", b)
	}
}
