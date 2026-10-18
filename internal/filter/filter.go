// Package filter implements a subset of rsync's include/exclude rules.
//
// Paths are slash-separated and relative to the transfer root. Rules are
// checked in order and the first match wins; unmatched paths are included.
//
//   - a pattern without a slash matches the final path component
//   - a pattern with a slash (other than a trailing one) or "**" matches the
//     full path, starting at any component boundary
//   - a leading slash anchors the pattern to the transfer root
//   - a trailing slash matches directories only
//   - "*" matches within a component, "**" across components, "?" one char,
//     "[...]" a character class; "dir/***" matches dir and everything in it
package filter

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Rule is one include or exclude pattern.
type Rule struct {
	Include bool
	Pattern string
}

func (r Rule) String() string {
	if r.Include {
		return "+ " + r.Pattern
	}
	return "- " + r.Pattern
}

// ParseRule parses an rsync --filter style rule such as "- *.o" or
// "include foo/".
func ParseRule(s string) (Rule, error) {
	kind, pat, ok := strings.Cut(s, " ")
	if !ok || pat == "" {
		return Rule{}, fmt.Errorf("invalid filter rule %q", s)
	}
	switch kind {
	case "-", "exclude":
		return Rule{Pattern: pat}, nil
	case "+", "include":
		return Rule{Include: true, Pattern: pat}, nil
	}
	return Rule{}, fmt.Errorf("unsupported filter rule %q (only + and - are supported)", s)
}

type compiled struct {
	include  bool
	dirOnly  bool
	fullPath bool
	re       *regexp.Regexp
}

// List is a compiled rule list. A nil *List excludes nothing.
type List struct {
	rules []compiled
}

// Compile compiles rules into a List.
func Compile(rules []Rule) (*List, error) {
	l := &List{}
	for _, r := range rules {
		c, err := compile(r)
		if err != nil {
			return nil, err
		}
		l.rules = append(l.rules, c)
	}
	return l, nil
}

func compile(r Rule) (compiled, error) {
	p := r.Pattern
	c := compiled{include: r.Include}
	if strings.HasSuffix(p, "/***") {
		// Matches the directory itself and everything beneath it.
		p = strings.TrimSuffix(p, "/***")
		c.fullPath = true
		return finish(c, p, `(/.*)?$`)
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
		c.dirOnly = true
	}
	return finish(c, p, `$`)
}

func finish(c compiled, p, suffix string) (compiled, error) {
	anchored := strings.HasPrefix(p, "/")
	p = strings.TrimLeft(p, "/")
	c.fullPath = c.fullPath || anchored || strings.Contains(p, "/") || strings.Contains(p, "**")
	prefix := "^"
	if c.fullPath && !anchored {
		prefix = `(^|/)`
	}
	re, err := regexp.Compile(prefix + globToRegexp(p) + suffix)
	if err != nil {
		return c, fmt.Errorf("invalid pattern %q: %v", p, err)
	}
	c.re = re
	return c, nil
}

func globToRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i > 0 && p[i-1] == '/' && strings.HasPrefix(p[i:], "**/") {
				// "/**/" also matches a single slash (zero directories).
				b.WriteString("(.*/)?")
				i += 2
			} else if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				for i+1 < len(p) && p[i+1] == '*' {
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case '\\':
			if i+1 < len(p) {
				i++
				b.WriteString(regexp.QuoteMeta(p[i : i+1]))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// Excluded reports whether rel should be skipped.
func (l *List) Excluded(rel string, isDir bool) bool {
	if l == nil || rel == "." || rel == "" {
		return false
	}
	base := path.Base(rel)
	for _, r := range l.rules {
		if r.dirOnly && !isDir {
			continue
		}
		subject := base
		if r.fullPath {
			subject = rel
		}
		if r.re.MatchString(subject) {
			return !r.include
		}
	}
	return false
}
