package xfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// unixPerm converts a FileMode to portable unix permission bits.
func unixPerm(m fs.FileMode) uint32 {
	p := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		p |= 04000
	}
	if m&fs.ModeSetgid != 0 {
		p |= 02000
	}
	if m&fs.ModeSticky != 0 {
		p |= 01000
	}
	return p
}

// fileMode converts unix permission bits to a FileMode.
func fileMode(p uint32) fs.FileMode {
	m := fs.FileMode(p & 0777)
	if p&04000 != 0 {
		m |= fs.ModeSetuid
	}
	if p&02000 != 0 {
		m |= fs.ModeSetgid
	}
	if p&01000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

func fileSum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

const partialSuffix = ".xt-partial"

// tempName returns the name of a temporary file next to target. Partial
// files use a stable name so an interrupted transfer can be resumed.
func tempName(target, suffix string) string {
	dir, base := filepath.Split(target)
	name := "." + base + suffix
	if len(name) > 200 {
		sum := sha256.Sum256([]byte(base))
		name = ".xt-" + hex.EncodeToString(sum[:12]) + suffix
	}
	return filepath.Join(dir, name)
}

func hasTrailingSep(p string) bool {
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, "/.") || p == "." {
		return true
	}
	if runtime.GOOS == "windows" && (strings.HasSuffix(p, `\`) || strings.HasSuffix(p, `\.`)) {
		return true
	}
	return false
}

var errUnsafePath = errors.New("unsafe path")

// validRel checks that a path from the peer stays inside the destination.
func validRel(p string) error {
	if p == "." {
		return nil
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w %q", errUnsafePath, p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("%w %q", errUnsafePath, p)
		}
	}
	return nil
}

// winNameErr reports why a relative slash-separated path cannot be created
// on Windows. Such names are legal elsewhere, so the receiver skips them
// rather than treating the file list as hostile.
func winNameErr(p string) error {
	for _, c := range strings.Split(p, "/") {
		if c == "." {
			continue
		}
		if i := strings.IndexFunc(c, func(r rune) bool {
			return r < 0x20 || strings.ContainsRune(`<>:"\|?*`, r)
		}); i >= 0 {
			return fmt.Errorf("name contains %q, which is not allowed on Windows", c[i])
		}
		if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
			return errors.New("name ends with a dot or space, which is not allowed on Windows")
		}
		base, _, _ := strings.Cut(c, ".")
		if winReserved(strings.ToUpper(strings.TrimRight(base, " "))) {
			return fmt.Errorf("%q is a reserved device name on Windows", c)
		}
	}
	return nil
}

func winReserved(b string) bool {
	switch b {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	return len(b) == 4 && (strings.HasPrefix(b, "COM") || strings.HasPrefix(b, "LPT")) &&
		b[3] >= '0' && b[3] <= '9'
}

// expandHome expands a leading ~ in a path given on the remote side.
func expandHome(p string) string {
	if p == "" {
		return "."
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
