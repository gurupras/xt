package filter

import "testing"

func TestExcluded(t *testing.T) {
	rules := []Rule{
		{Include: true, Pattern: "keep.o"},
		{Pattern: "*.o"},
		{Pattern: "/build/"},
		{Pattern: "cache/"},
		{Pattern: "docs/**/draft*"},
		{Pattern: "a/b"},
		{Pattern: "vendor/***"},
		{Pattern: "file[0-9].txt"},
	}
	l, err := Compile(rules)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path  string
		dir   bool
		excl  bool
		label string
	}{
		{"x.o", false, true, "basename glob"},
		{"src/deep/x.o", false, true, "basename glob deep"},
		{"src/keep.o", false, false, "earlier include wins"},
		{"build", true, true, "anchored dir"},
		{"src/build", true, false, "anchored only at root"},
		{"build", false, false, "dir-only rule skips files"},
		{"x/cache", true, true, "unanchored dir"},
		{"docs/v1/old/draft-1", false, true, "double star"},
		{"docs/draft", false, true, "double star zero dirs"},
		{"z/a/b", false, true, "slash pattern at component boundary"},
		{"za/b", false, false, "slash pattern not mid-component"},
		{"vendor", true, true, "triple star dir"},
		{"vendor/x/y", false, true, "triple star contents"},
		{"file3.txt", false, true, "class"},
		{"filex.txt", false, false, "class miss"},
		{".", true, false, "root never excluded"},
	}
	for _, c := range cases {
		if got := l.Excluded(c.path, c.dir); got != c.excl {
			t.Errorf("%s: Excluded(%q, %v) = %v, want %v", c.label, c.path, c.dir, got, c.excl)
		}
	}
}

func TestParseRule(t *testing.T) {
	r, err := ParseRule("+ foo/")
	if err != nil || !r.Include || r.Pattern != "foo/" {
		t.Fatalf("got %+v %v", r, err)
	}
	if _, err := ParseRule("merge .rules"); err == nil {
		t.Fatal("expected error for merge rule")
	}
}
