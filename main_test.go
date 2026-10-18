package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var xtBin, fakeSSH string

// TestMain builds xt once. Remote transfers go through a fake remote shell
// that drops the host argument and runs the command locally.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "xt-test-bin")
	if err != nil {
		panic(err)
	}
	xtBin = filepath.Join(dir, "xt")
	if runtime.GOOS == "windows" {
		xtBin += ".exe"
	}
	build := []string{"build", "-o", xtBin}
	if os.Getenv("XT_E2E_COVER") != "" {
		// Instrumented binary writes coverage to $GOCOVERDIR.
		build = append(build, "-cover", "-coverpkg=./...")
	}
	if out, err := exec.Command("go", append(build, ".")...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	if runtime.GOOS != "windows" {
		fakeSSH = filepath.Join(dir, "fakessh")
		os.WriteFile(fakeSSH, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	out  string
	code int
}

func xt(t *testing.T, args ...string) result {
	t.Helper()
	full := append([]string{"--xt-path", xtBin}, args...)
	if fakeSSH != "" {
		full = append([]string{"-e", fakeSSH}, full...)
	}
	cmd := exec.Command(xtBin, full...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{buf.String(), code}
}

func mustXT(t *testing.T, args ...string) string {
	t.Helper()
	r := xt(t, args...)
	if r.code != 0 {
		t.Fatalf("xt %v: exit %d\n%s", args, r.code, r.out)
	}
	return r.out
}

func needRemote(t *testing.T) {
	if fakeSSH == "" {
		t.Skip("fake remote shell needs a POSIX shell")
	}
}

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// touch moves p's mtime so the quick check sees it as changed.
func touch(t *testing.T, p string, d time.Duration) {
	t.Helper()
	tm := time.Now().Add(d)
	if err := os.Chtimes(p, tm, tm); err != nil {
		t.Fatal(err)
	}
}

func makeTree(t *testing.T, root string) {
	write(t, filepath.Join(root, "a.txt"), []byte("hello\n"))
	write(t, filepath.Join(root, "sub", "b.bin"), random(300_000))
	write(t, filepath.Join(root, "sub", "deep", "c.txt"), []byte("deep\n"))
	write(t, filepath.Join(root, "empty.txt"), nil)
	os.MkdirAll(filepath.Join(root, "emptydir"), 0o755)
	if runtime.GOOS != "windows" {
		os.Symlink("a.txt", filepath.Join(root, "link"))
	}
}

// snapshot describes a tree: type, size, mtime (seconds), contents.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			m[rel] = "link:" + l
		case fi.IsDir():
			m[rel] = "dir"
		default:
			b, _ := os.ReadFile(p)
			m[rel] = fmt.Sprintf("file:%d:%x", fi.ModTime().Unix(), b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	delete(m, ".")
	return m
}

func sameTree(t *testing.T, a, b string) {
	t.Helper()
	sa, sb := snapshot(t, a), snapshot(t, b)
	for k, v := range sa {
		if sb[k] != v {
			t.Errorf("%s differs (src %.40s…, dst %.40s…)", k, v, sb[k])
		}
	}
	for k := range sb {
		if _, ok := sa[k]; !ok {
			t.Errorf("extra entry in destination: %s", k)
		}
	}
}

func stat(t *testing.T, out, name string) int64 {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(name) + `: ([\d,]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no %q in output:\n%s", name, out)
	}
	n, _ := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	return n
}

func TestLocalCopy(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	makeTree(t, src)
	mustXT(t, "-a", src+"/", dst)
	sameTree(t, src, dst)

	out := mustXT(t, "-a", "--stats", src+"/", dst)
	if n := stat(t, out, "Number of regular files transferred"); n != 0 {
		t.Errorf("second run transferred %d files", n)
	}
}

func TestDirWithoutTrailingSlash(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	makeTree(t, src)
	mustXT(t, "-a", src, dst)
	sameTree(t, src, filepath.Join(dst, "src"))
}

func TestSingleFile(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "one.txt")
	write(t, src, []byte("one"))
	mustXT(t, "-a", src, filepath.Join(d, "renamed.txt"))
	if b, _ := os.ReadFile(filepath.Join(d, "renamed.txt")); string(b) != "one" {
		t.Fatalf("got %q", b)
	}
	os.Mkdir(filepath.Join(d, "dir"), 0o755)
	mustXT(t, "-a", src, filepath.Join(d, "dir"))
	if b, _ := os.ReadFile(filepath.Join(d, "dir", "one.txt")); string(b) != "one" {
		t.Fatalf("got %q", b)
	}
}

func TestPushPull(t *testing.T) {
	needRemote(t)
	for _, z := range []string{"-a", "-az"} {
		t.Run(z, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "src")
			makeTree(t, src)
			mustXT(t, z, src+"/", "remote:"+filepath.Join(d, "pushed"))
			sameTree(t, src, filepath.Join(d, "pushed"))
			mustXT(t, z, "remote:"+filepath.Join(d, "pushed")+"/", filepath.Join(d, "pulled"))
			sameTree(t, src, filepath.Join(d, "pulled"))
		})
	}
}

func TestDelta(t *testing.T) {
	needRemote(t)
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	data := random(4 << 20)
	write(t, filepath.Join(src, "big"), data)
	mustXT(t, "-a", src+"/", "remote:"+dst)

	// Change a few bytes in the middle and insert some at the front.
	data[2<<20] ^= 0xff
	data = append([]byte("inserted"), data...)
	write(t, filepath.Join(src, "big"), data)
	touch(t, filepath.Join(src, "big"), time.Hour)

	for _, dir := range []string{"push", "pull"} {
		args := []string{"-a", "--stats", src + "/", "remote:" + dst}
		if dir == "pull" {
			write(t, filepath.Join(dst, "big"), data[8:])
			touch(t, filepath.Join(dst, "big"), -time.Hour)
			args = []string{"-a", "--stats", "remote:" + src + "/", dst}
		}
		out := mustXT(t, args...)
		if lit := stat(t, out, "Literal data"); lit > 64<<10 {
			t.Errorf("%s: literal data %d, want a small delta", dir, lit)
		}
		if sent := stat(t, out, "Total bytes sent") + stat(t, out, "Total bytes received"); sent > 200<<10 {
			t.Errorf("%s: %d bytes on the wire, want a small delta", dir, sent)
		}
		sameTree(t, src, dst)
	}
}

func TestResume(t *testing.T) {
	needRemote(t)
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	data := random(2 << 20)
	write(t, filepath.Join(src, "big"), data)
	partial := filepath.Join(dst, ".big.xt-partial")
	write(t, partial, data[:1500_000])

	out := mustXT(t, "-a", "--stats", "--partial", src+"/", "remote:"+dst)
	if lit := stat(t, out, "Literal data"); lit != int64(len(data)-1500_000) {
		t.Errorf("literal data %d, want only the remainder %d", lit, len(data)-1500_000)
	}
	sameTree(t, src, dst)
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("partial file left behind")
	}

	// A partial file that does not match the source is ignored.
	write(t, filepath.Join(src, "big"), random(1<<20))
	touch(t, filepath.Join(src, "big"), time.Hour)
	write(t, partial, data[:1000])
	mustXT(t, "-a", "--whole-file", "--partial", src+"/", "remote:"+dst)
	sameTree(t, src, dst)
}

func TestInterruptKeepsPartial(t *testing.T) {
	needRemote(t)
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, filepath.Join(src, "big"), random(256<<20))
	os.Mkdir(dst, 0o755)
	partial := filepath.Join(dst, ".big.xt-partial")

	cmd := exec.Command(xtBin, "-e", fakeSSH, "--xt-path", xtBin, "-a", "-P", src+"/", "remote:"+dst)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if st, err := os.Stat(partial); err == nil && st.Size() > 8<<20 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("partial file never grew\n%s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cmd.Process.Signal(os.Interrupt)
	err := cmd.Wait()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Skipf("transfer finished before the interrupt (err %v)", err)
	}
	if ee.ExitCode() != 20 {
		t.Fatalf("exit %d, want 20\n%s", ee.ExitCode(), buf.String())
	}
	st, err := os.Stat(partial)
	if err != nil {
		t.Fatalf("partial file not kept: %v\n%s", err, buf.String())
	}

	out := mustXT(t, "-a", "-P", "--stats", src+"/", "remote:"+dst)
	lit := stat(t, out, "Literal data")
	if lit > 256<<20-st.Size()+1<<20 {
		t.Errorf("resumed run sent %d literal bytes, partial had %d of %d", lit, st.Size(), 256<<20)
	}
	sameTree(t, src, dst)
}

func TestDeleteAndExclude(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	makeTree(t, src)
	write(t, filepath.Join(src, "skip.log"), []byte("log"))
	mustXT(t, "-a", "--exclude", "*.log", src+"/", dst)
	if _, err := os.Stat(filepath.Join(dst, "skip.log")); !os.IsNotExist(err) {
		t.Fatal("excluded file was copied")
	}

	write(t, filepath.Join(dst, "extra.txt"), []byte("x"))
	write(t, filepath.Join(dst, "olddir", "f"), []byte("x"))
	write(t, filepath.Join(dst, "keep.log"), []byte("x"))
	mustXT(t, "-a", "--delete", "--exclude", "*.log", src+"/", dst)
	for _, p := range []string{"extra.txt", "olddir"} {
		if _, err := os.Stat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s not deleted", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.log")); err != nil {
		t.Error("excluded file was deleted without --delete-excluded")
	}
	mustXT(t, "-a", "--delete", "--delete-excluded", "--exclude", "*.log", src+"/", dst)
	if _, err := os.Stat(filepath.Join(dst, "keep.log")); !os.IsNotExist(err) {
		t.Error("--delete-excluded kept an excluded file")
	}
}

func TestDryRun(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	makeTree(t, src)
	out := mustXT(t, "-avn", src+"/", dst)
	if !strings.Contains(out, "sub/b.bin") || !strings.Contains(out, "dry run") {
		t.Errorf("dry run output missing file names:\n%s", out)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("dry run created the destination")
	}
}

func TestUpdateAndExisting(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, filepath.Join(src, "f"), []byte("src"))
	write(t, filepath.Join(src, "new"), []byte("new"))
	write(t, filepath.Join(dst, "f"), []byte("newer on dst"))
	touch(t, filepath.Join(dst, "f"), time.Hour)
	mustXT(t, "-au", src+"/", dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "newer on dst" {
		t.Error("-u overwrote a newer file")
	}
	os.Remove(filepath.Join(dst, "new"))
	mustXT(t, "-a", "--existing", src+"/", dst)
	if _, err := os.Stat(filepath.Join(dst, "new")); !os.IsNotExist(err) {
		t.Error("--existing created a new file")
	}
}

func TestChecksumMode(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, filepath.Join(src, "f"), []byte("aaaa"))
	write(t, filepath.Join(dst, "f"), []byte("bbbb"))
	tm := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(src, "f"), tm, tm)
	os.Chtimes(filepath.Join(dst, "f"), tm, tm)
	mustXT(t, "-a", src+"/", dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "bbbb" {
		t.Fatal("quick check should have skipped the file")
	}
	mustXT(t, "-ac", src+"/", dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "aaaa" {
		t.Fatal("-c did not transfer a changed file")
	}
}

func TestMissingRemoteXT(t *testing.T) {
	needRemote(t)
	d := t.TempDir()
	write(t, filepath.Join(d, "f"), []byte("x"))
	cmd := exec.Command(xtBin, "-e", fakeSSH, "--xt-path", filepath.Join(d, "nope"), filepath.Join(d, "f"), "remote:"+filepath.Join(d, "g"))
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "is xt installed") {
		t.Errorf("unhelpful error:\n%s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"--bogus-flag", "a", "b"}, {"hosta:x", "hostb:y"}, {"-A", "a", "b"},
		{"--", "-oProxyCommand=x:y", "out"}, {"a", "user@-oProxyCommand=x:y"}} {
		cmd := exec.Command(xtBin, args...)
		out, err := cmd.CombinedOutput()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 1 {
			t.Errorf("xt %v: err %v, want exit 1\n%s", args, err, out)
		}
	}
}

// The tests below pin behaviours where xt used to differ from rsync.

func needSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
}

func TestNoSpecialBitsWithoutPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no setuid bits on Windows")
	}
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, filepath.Join(src, "prog"), []byte("x"))
	os.Chmod(filepath.Join(src, "prog"), 0o755|fs.ModeSetuid)
	os.Mkdir(filepath.Join(src, "dir"), 0o755)
	os.Chmod(filepath.Join(src, "dir"), 0o755|fs.ModeSetgid|fs.ModeSticky)
	mustXT(t, "-rt", src+"/", dst)
	for _, p := range []string{"prog", "dir"} {
		st, err := os.Stat(filepath.Join(dst, p))
		if err != nil {
			t.Fatal(err)
		}
		if m := st.Mode() & (fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != 0 {
			t.Errorf("%s kept special bits %v without -p", p, m)
		}
	}
	mustXT(t, "-a", src+"/", filepath.Join(d, "perms"))
	if st, _ := os.Stat(filepath.Join(d, "perms", "prog")); st.Mode()&fs.ModeSetuid == 0 {
		t.Error("-p dropped the setuid bit")
	}
}

func TestRemoveSourceNothingSynced(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "src", "a", "b"), 0o755)
	mustXT(t, "-a", "--remove-source-files", filepath.Join(d, "src")+"/", filepath.Join(d, "dst"))
}

func TestRemoveSourceCopyLinks(t *testing.T) {
	needSymlinks(t)
	d := t.TempDir()
	write(t, filepath.Join(d, "real"), []byte("x"))
	os.Mkdir(filepath.Join(d, "src"), 0o755)
	os.Symlink("../real", filepath.Join(d, "src", "lnk"))
	mustXT(t, "-rL", "--remove-source-files", filepath.Join(d, "src")+"/", filepath.Join(d, "dst"))
	if _, err := os.Lstat(filepath.Join(d, "src", "lnk")); !os.IsNotExist(err) {
		t.Error("source link not removed")
	}
	if _, err := os.Stat(filepath.Join(d, "real")); err != nil {
		t.Error("link target was removed")
	}
}
