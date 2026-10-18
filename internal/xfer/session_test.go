package xfer

import (
	"reflect"
	"runtime"
	"testing"
)

func TestParseLocation(t *testing.T) {
	cases := []struct {
		in         string
		host, path string
	}{
		{"file", "", "file"},
		{"dir/", "", "dir/"},
		{"host:path", "host", "path"},
		{"user@host:/abs", "user@host", "/abs"},
		{"host:", "host", ""},
		{"host:C:/Users/me", "host", "C:/Users/me"},
		{`host:C:\Users\me`, "host", `C:\Users\me`},
		{"[::1]:dir", "::1", "dir"},
		{"u@[fe80::1]:dir", "u@fe80::1", "dir"},
		{"./a:b", "", "./a:b"},
		{":x", "", ":x"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ in, host, path string }{`C:\data`, "", `C:\data`})
	} else {
		cases = append(cases, struct{ in, host, path string }{"c:data", "c", "data"})
	}
	for _, c := range cases {
		l, err := ParseLocation(c.in)
		if err != nil || l.Host != c.host || l.Path != c.path {
			t.Errorf("ParseLocation(%q) = %+v, %v; want host %q path %q", c.in, l, err, c.host, c.path)
		}
	}
	for _, bad := range []string{"host::module", "rsync://host/mod"} {
		if _, err := ParseLocation(bad); err == nil {
			t.Errorf("ParseLocation(%q) succeeded", bad)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`ssh -p 2222 -o "ProxyCommand ssh -W %h:%p jump" -i 'my key'`)
	want := []string{"ssh", "-p", "2222", "-o", "ProxyCommand ssh -W %h:%p jump", "-i", "my key"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
