package opt

import (
	"fmt"
	"strconv"
	"strings"
)

// ChmodRule is one comma-separated item of a --chmod spec, such as "Dg+s",
// "F644" or "u=rwX,go-w".
type ChmodRule struct {
	only   byte // 'D' (directories), 'F' (files) or 0 (both)
	octal  bool
	who    uint32 // mask of the bits the rule may touch
	op     byte   // '+', '-' or '='
	bits   uint32 // permission bits, already spread over who
	condX  bool   // X: execute only for directories or already-executable files
	setVal uint32 // octal value
}

// ParseChmod parses a --chmod spec.
func ParseChmod(spec string) ([]ChmodRule, error) {
	var rules []ChmodRule
	for _, item := range strings.Split(spec, ",") {
		r := ChmodRule{}
		s := item
		if s != "" && (s[0] == 'D' || s[0] == 'F') {
			r.only, s = s[0], s[1:]
		}
		if s == "" {
			return nil, fmt.Errorf("invalid --chmod item %q", item)
		}
		if s[0] >= '0' && s[0] <= '7' {
			v, err := strconv.ParseUint(s, 8, 32)
			if err != nil || v > 07777 {
				return nil, fmt.Errorf("invalid --chmod item %q", item)
			}
			r.octal, r.setVal = true, uint32(v)
			rules = append(rules, r)
			continue
		}
		i := 0
		for ; i < len(s) && strings.IndexByte("ugoa", s[i]) >= 0; i++ {
			switch s[i] {
			case 'u':
				r.who |= 04700
			case 'g':
				r.who |= 02070
			case 'o':
				r.who |= 01007
			case 'a':
				r.who |= 07777
			}
		}
		if r.who == 0 {
			r.who = 07777
		}
		if i >= len(s) || strings.IndexByte("+-=", s[i]) < 0 {
			return nil, fmt.Errorf("invalid --chmod item %q", item)
		}
		r.op = s[i]
		for _, c := range s[i+1:] {
			switch c {
			case 'r':
				r.bits |= 0444
			case 'w':
				r.bits |= 0222
			case 'x':
				r.bits |= 0111
			case 'X':
				r.condX = true
			case 's':
				r.bits |= 06000
			case 't':
				r.bits |= 01000
			default:
				return nil, fmt.Errorf("invalid --chmod item %q", item)
			}
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// ApplyChmod applies rules to the unix permission bits mode.
func ApplyChmod(rules []ChmodRule, mode uint32, isDir bool) uint32 {
	for _, r := range rules {
		if r.only == 'D' && !isDir || r.only == 'F' && isDir {
			continue
		}
		if r.octal {
			mode = r.setVal
			continue
		}
		bits := r.bits
		if r.condX && (isDir || mode&0111 != 0) {
			bits |= 0111
		}
		bits &= r.who
		switch r.op {
		case '+':
			mode |= bits
		case '-':
			mode &^= bits
		case '=':
			mode = mode&^r.who | bits
		}
	}
	return mode
}
