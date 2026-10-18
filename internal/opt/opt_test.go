package opt

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseCombined(t *testing.T) {
	o, err := Parse([]string{"-avzupr", "--delete", "--exclude=*.o", "--exclude", "tmp/", "src/", "host:dst"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.Recursive || !o.Links || !o.Perms || !o.Times || !o.Owner || !o.Group || !o.Compress || !o.Update {
		t.Fatalf("flags not set: %+v", o)
	}
	if o.Verbose != 1 || !o.Delete || len(o.Rules) != 2 || o.Rules[1].Pattern != "tmp/" {
		t.Fatalf("bad parse: %+v", o)
	}
	if len(o.Args) != 2 || o.Args[0] != "src/" || o.Args[1] != "host:dst" {
		t.Fatalf("args = %q", o.Args)
	}
}

func TestParseRshAndNegation(t *testing.T) {
	o, err := Parse([]string{"-ave", "ssh -p 2222", "--no-o", "--no-perms", "-P", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Rsh != "ssh -p 2222" || o.Owner || o.Perms || !o.Partial || !o.Progress {
		t.Fatalf("bad parse: %+v", o)
	}
	o, err = Parse([]string{"-essh -i key", "a", "b"})
	if err != nil || o.Rsh != "ssh -i key" {
		t.Fatalf("attached -e: %+v %v", o, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-A", "a", "b"}, {"--inplace", "a", "b"}, {"--nope"}, {"-e"},
		{"--max-size=10Q", "a", "b"}, {"--bwlimit=x", "a", "b"}, {"--chmod=Z+r", "a", "b"},
		{"--info=bogus", "a", "b"}, {"--timeout=-1", "a", "b"}, {"--files-from=host:list", "a", "b"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", args)
		}
	}
}

func TestWholeFile(t *testing.T) {
	o, _ := Parse([]string{"-W", "a", "b"})
	if !o.WholeFile || o.WholeFileMode != 1 {
		t.Fatal("-W not applied")
	}
	o, _ = Parse([]string{"--no-W", "a", "b"})
	if o.WholeFile || o.WholeFileMode != 2 {
		t.Fatal("--no-W not applied")
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		unit int64
		want int64
	}{
		{"100", 1, 100}, {"100", 1024, 102400}, {"1K", 1, 1024}, {"1k", 1, 1024},
		{"1.5M", 1, 1572864}, {"2KB", 1, 2000}, {"2KiB", 1, 2048}, {"1G", 1, 1 << 30},
		{"3MB", 1, 3000000}, {"7B", 1024, 7},
	} {
		got, err := ParseSize(c.in, c.unit)
		if err != nil || got != c.want {
			t.Errorf("ParseSize(%q, %d) = %d, %v; want %d", c.in, c.unit, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "x", "1Q", "1KX", "-5", "1.2.3"} {
		if _, err := ParseSize(bad, 1); err == nil {
			t.Errorf("ParseSize(%q) succeeded", bad)
		}
	}
}

func TestChmod(t *testing.T) {
	for _, c := range []struct {
		spec string
		mode uint32
		dir  bool
		want uint32
	}{
		{"D755,F644", 0700, true, 0755},
		{"D755,F644", 0700, false, 0644},
		{"go-w", 0777, false, 0755},
		{"u+x", 0644, false, 0744},
		{"a+X", 0644, false, 0644},
		{"a+X", 0744, false, 0755},
		{"a+X", 0600, true, 0711},
		{"g=r", 0777, false, 0747},
		{"+r", 0200, false, 0644},
		{"Fo-rwx", 0777, false, 0770},
		{"Fo-rwx", 0777, true, 0777},
		{"u+s", 0755, false, 04755},
	} {
		rules, err := ParseChmod(c.spec)
		if err != nil {
			t.Fatalf("ParseChmod(%q): %v", c.spec, err)
		}
		if got := ApplyChmod(rules, c.mode, c.dir); got != c.want {
			t.Errorf("chmod %q on %o (dir=%v) = %o, want %o", c.spec, c.mode, c.dir, got, c.want)
		}
	}
	for _, bad := range []string{"", "D", "z+r", "u", "u+q", "8", "77777", "a,"} {
		if _, err := ParseChmod(bad); err == nil {
			t.Errorf("ParseChmod(%q) succeeded", bad)
		}
	}
}

func TestNewFlags(t *testing.T) {
	o, err := Parse([]string{"-iiIbHSmOEC0x46", "--max-size=1M", "--min-size=10", "--bwlimit=2M",
		"--timeout=9", "--max-delete=3", "--delete-after", "--backup-dir=old", "--chmod=F644",
		"--chmod=D755", "--remove-source-files", "--mkpath", "--list-only", "--ignore-errors",
		"--info=stats2,name1", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"itemize": o.Itemize == 2, "ignore-times": o.IgnoreTimes, "backup": o.Backup,
		"hard-links": o.HardLinks, "sparse": o.Sparse, "prune": o.PruneEmpty,
		"omit-dir-times": o.OmitDirTimes, "executability": o.Executability, "one-fs": o.OneFS,
		"ipv6 (last wins)": o.IPVersion == '6', "max-size": o.MaxSize == 1<<20, "min-size": o.MinSize == 10,
		"bwlimit": o.BwLimit == 2<<20, "timeout": o.Timeout == 9, "max-delete": o.MaxDelete == 3,
		"delete-after": o.Delete && o.DeleteAfter, "backup-dir": o.BackupDir == "old" && o.Suffix == "",
		"chmod": o.Chmod == "F644,D755", "remove-source": o.RemoveSource, "mkpath": o.Mkpath,
		"list-only": o.ListOnly, "ignore-errors": o.IgnoreErrors, "info": o.Stats && o.Verbose == 1,
		"cvs-exclude": len(o.Rules) > 10,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s not applied: %+v", name, o)
		}
	}
	o, _ = Parse([]string{"-b", "a", "b"})
	if o.Suffix != "~" || o.MaxDelete != -1 || o.BwLimit != 0 {
		t.Errorf("defaults: suffix %q, max-delete %d, bwlimit %d", o.Suffix, o.MaxDelete, o.BwLimit)
	}
	o, _ = Parse([]string{"--bwlimit=100", "a", "b"})
	if o.BwLimit != 100*1024 {
		t.Errorf("--bwlimit=100 = %d, want KiB/s", o.BwLimit)
	}
}

func TestFilesFrom(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "list")
	os.WriteFile(list, []byte("a.txt\n# comment\n\nsub/b.txt\r\n; also comment\n"), 0o644)
	o, err := Parse([]string{"-a", "--files-from=" + list, "src", "dst"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.FileList, []string{"a.txt", "sub/b.txt"}) {
		t.Errorf("FileList = %q", o.FileList)
	}
	if !o.FilesFrom || !o.Relative || !o.Dirs || o.Recursive {
		t.Errorf("files-from implications: %+v", o)
	}
	o, _ = Parse([]string{"-ar", "--files-from=" + list, "src", "dst"})
	if !o.Recursive {
		t.Error("explicit -r was dropped by --files-from")
	}
	os.WriteFile(list, []byte("x y\x00#z\x00\x00w\n"), 0o644)
	o, err = Parse([]string{"-0", "--files-from", list, "src", "dst"})
	if err != nil || !reflect.DeepEqual(o.FileList, []string{"x y", "#z", "w\n"}) {
		t.Errorf("--from0 FileList = %q, %v", o.FileList, err)
	}
	if _, err := Parse([]string{"--files-from=" + filepath.Join(dir, "missing"), "a", "b"}); err == nil {
		t.Error("missing --files-from file accepted")
	}
}

func TestInfo(t *testing.T) {
	o, _ := Parse([]string{"--info=progress2", "a", "b"})
	if o.Progress || o.NoProgress {
		t.Error("progress2 should keep the live display")
	}
	o, _ = Parse([]string{"--info=progress0", "a", "b"})
	if !o.NoProgress {
		t.Error("progress0 not applied")
	}
	o, _ = Parse([]string{"--info=progress", "a", "b"})
	if !o.Progress {
		t.Error("progress not applied")
	}
}
