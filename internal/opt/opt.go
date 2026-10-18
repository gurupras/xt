// Package opt parses xt's rsync-compatible command line.
package opt

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gurupras/xt/internal/filter"
)

// Options holds the parsed command line. The whole struct is sent to the
// remote side, which only looks at the transfer-related fields.
type Options struct {
	Recursive      bool
	Dirs           bool
	Links          bool
	CopyLinks      bool
	Perms          bool
	Times          bool
	Owner          bool
	Group          bool
	Devices        bool
	Specials       bool
	Update         bool
	Existing       bool
	IgnoreExisting bool
	SizeOnly       bool
	Checksum       bool
	ModifyWindow   int64
	WholeFile      bool
	Partial        bool
	Delete         bool
	DeleteExcluded bool
	Force          bool
	DryRun         bool
	Verbose        int
	Quiet          bool
	Rules          []filter.Rule
	IgnoreTimes    bool
	MaxSize        int64 // 0 = no limit
	MinSize        int64
	Itemize        int
	RemoveSource   bool
	Backup         bool
	Suffix         string
	BackupDir      string
	BwLimit        int64 // bytes per second; 0 = unlimited
	Timeout        int   // I/O timeout in seconds; 0 = none
	OneFS          bool
	Relative       bool
	FilesFrom      bool
	FileList       []string // entries read from --files-from
	Chmod          string
	DeleteAfter    bool
	MaxDelete      int64 // -1 = unlimited
	IgnoreErrors   bool
	HardLinks      bool
	Sparse         bool
	ListOnly       bool
	PruneEmpty     bool
	OmitDirTimes   bool
	Executability  bool
	Mkpath         bool

	// Client-side only.
	Compress      bool
	CompressLevel int
	Progress      bool
	NoProgress    bool
	Human         bool
	Stats         bool
	Rsh           string
	XtPath        string
	WholeFileMode int  // 0 = automatic, 1 = forced on, 2 = forced off
	IPVersion     byte // '4' or '6' to pass to ssh
	Args          []string
	Help          bool
	Version       bool

	recursiveSet bool // -r given explicitly, not just implied by -a
	filesFrom    string
	from0        bool
	cvsExclude   bool
}

type unsupported string

var unsupportedShort = map[byte]unsupported{
	'A': "--acls", 'X': "--xattrs", 'k': "--copy-dirlinks", 'K': "--keep-dirlinks",
	'F': "filter merge files", 'y': "--fuzzy", 'T': "--temp-dir", 'B': "--block-size",
	'U': "--atimes", 'N': "--crtimes", 'M': "--remote-option", 's': "--secluded-args",
}

// shortToLong maps letters usable in --no-X to the long option they negate.
var shortToLong = map[byte]string{
	'r': "recursive", 'd': "dirs", 'l': "links", 'L': "copy-links", 'p': "perms",
	't': "times", 'o': "owner", 'g': "group", 'D': "D", 'u': "update", 'c': "checksum",
	'W': "whole-file", 'z': "compress", 'v': "verbose", 'n': "dry-run", 'h': "human-readable",
	'q': "quiet", 'I': "ignore-times", 'i': "itemize-changes", 'b': "backup", 'x': "one-file-system",
	'R': "relative", 'H': "hard-links", 'S': "sparse", 'm': "prune-empty-dirs",
	'O': "omit-dir-times", 'J': "omit-link-times", 'E': "executability", 'C': "cvs-exclude",
}

func boolSetter(name string) (func(o *Options, v bool), bool) {
	f, ok := map[string]func(o *Options, v bool){
		"recursive":           func(o *Options, v bool) { o.Recursive, o.recursiveSet = v, v },
		"dirs":                func(o *Options, v bool) { o.Dirs = v },
		"links":               func(o *Options, v bool) { o.Links = v },
		"copy-links":          func(o *Options, v bool) { o.CopyLinks = v },
		"perms":               func(o *Options, v bool) { o.Perms = v },
		"times":               func(o *Options, v bool) { o.Times = v },
		"owner":               func(o *Options, v bool) { o.Owner = v },
		"group":               func(o *Options, v bool) { o.Group = v },
		"devices":             func(o *Options, v bool) { o.Devices = v },
		"specials":            func(o *Options, v bool) { o.Specials = v },
		"D":                   func(o *Options, v bool) { o.Devices, o.Specials = v, v },
		"update":              func(o *Options, v bool) { o.Update = v },
		"existing":            func(o *Options, v bool) { o.Existing = v },
		"ignore-non-existing": func(o *Options, v bool) { o.Existing = v },
		"ignore-existing":     func(o *Options, v bool) { o.IgnoreExisting = v },
		"size-only":           func(o *Options, v bool) { o.SizeOnly = v },
		"checksum":            func(o *Options, v bool) { o.Checksum = v },
		"partial":             func(o *Options, v bool) { o.Partial = v },
		"delete":              func(o *Options, v bool) { o.Delete = v },
		"del":                 func(o *Options, v bool) { o.Delete = v },
		"delete-before":       func(o *Options, v bool) { o.Delete = v },
		"delete-during":       func(o *Options, v bool) { o.Delete = v },
		"delete-excluded":     func(o *Options, v bool) { o.Delete, o.DeleteExcluded = o.Delete || v, v },
		"force":               func(o *Options, v bool) { o.Force = v },
		"dry-run":             func(o *Options, v bool) { o.DryRun = v },
		"quiet":               func(o *Options, v bool) { o.Quiet = v },
		"compress":            func(o *Options, v bool) { o.Compress = v },
		"progress":            func(o *Options, v bool) { o.Progress, o.NoProgress = v, !v },
		"human-readable":      func(o *Options, v bool) { o.Human = v },
		"stats":               func(o *Options, v bool) { o.Stats = v },
		"delete-after":        func(o *Options, v bool) { o.Delete, o.DeleteAfter = o.Delete || v, v },
		"delete-delay":        func(o *Options, v bool) { o.Delete, o.DeleteAfter = o.Delete || v, v },
		"ignore-times":        func(o *Options, v bool) { o.IgnoreTimes = v },
		"remove-source-files": func(o *Options, v bool) { o.RemoveSource = v },
		"backup":              func(o *Options, v bool) { o.Backup = v },
		"one-file-system":     func(o *Options, v bool) { o.OneFS = v },
		"relative":            func(o *Options, v bool) { o.Relative = v },
		"ignore-errors":       func(o *Options, v bool) { o.IgnoreErrors = v },
		"hard-links":          func(o *Options, v bool) { o.HardLinks = v },
		"sparse":              func(o *Options, v bool) { o.Sparse = v },
		"list-only":           func(o *Options, v bool) { o.ListOnly = v },
		"prune-empty-dirs":    func(o *Options, v bool) { o.PruneEmpty = v },
		"omit-dir-times":      func(o *Options, v bool) { o.OmitDirTimes = v },
		"executability":       func(o *Options, v bool) { o.Executability = v },
		"mkpath":              func(o *Options, v bool) { o.Mkpath = v },
		"cvs-exclude":         func(o *Options, v bool) { o.cvsExclude = v },
		"from0":               func(o *Options, v bool) { o.from0 = v },
		"itemize-changes": func(o *Options, v bool) {
			if v {
				o.Itemize++
			} else {
				o.Itemize = 0
			}
		},
		"ipv4": func(o *Options, v bool) { o.IPVersion = '4' },
		"ipv6": func(o *Options, v bool) { o.IPVersion = '6' },
		// Symlink times are never set and ids are always sent numerically.
		"omit-link-times": func(o *Options, v bool) {},
		"numeric-ids":     func(o *Options, v bool) {},
		// xt always builds the whole file list before transferring.
		"inc-recursive": func(o *Options, v bool) {},
		"i-r":           func(o *Options, v bool) {},
		"verbose": func(o *Options, v bool) {
			if v {
				o.Verbose++
			} else {
				o.Verbose = 0
			}
		},
		"whole-file": func(o *Options, v bool) {
			if v {
				o.WholeFileMode = 1
			} else {
				o.WholeFileMode = 2
			}
		},
	}[name]
	return f, ok
}

func (o *Options) archive() {
	o.Recursive, o.Links, o.Perms, o.Times = true, true, true, true
	o.Group, o.Owner, o.Devices, o.Specials = true, true, true, true
}

// Parse parses command line arguments (without the program name).
func Parse(args []string) (*Options, error) {
	o := &Options{CompressLevel: 6, XtPath: "xt", MaxDelete: -1}
	if len(args) == 1 && args[0] == "-h" {
		o.Help = true
		return o, nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.Args = append(o.Args, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			next := func() (string, error) {
				if hasVal {
					return val, nil
				}
				if i+1 >= len(args) {
					return "", fmt.Errorf("option --%s requires an argument", name)
				}
				i++
				return args[i], nil
			}
			if err := o.long(name, hasVal, next); err != nil {
				return nil, err
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'e' || c == 'f' {
					v := a[j+1:]
					if v == "" {
						if i+1 >= len(args) {
							return nil, fmt.Errorf("option -%c requires an argument", c)
						}
						i++
						v = args[i]
					}
					var err error
					if c == 'e' {
						o.Rsh = v
					} else {
						err = o.addFilter(v)
					}
					if err != nil {
						return nil, err
					}
					break
				}
				if err := o.short(c); err != nil {
					return nil, err
				}
			}
		default:
			o.Args = append(o.Args, a)
		}
	}
	switch o.WholeFileMode {
	case 1:
		o.WholeFile = true
	case 2:
		o.WholeFile = false
	}
	if o.Suffix == "" && o.BackupDir == "" {
		o.Suffix = "~"
	}
	if o.BackupDir != "" {
		o.Backup = true
	}
	if o.cvsExclude {
		// rsync's default CVS ignore list, checked after the user's rules.
		for _, p := range strings.Fields(cvsIgnore) {
			o.Rules = append(o.Rules, filter.Rule{Pattern: p})
		}
	}
	if o.Chmod != "" {
		if _, err := ParseChmod(o.Chmod); err != nil {
			return nil, err
		}
	}
	if o.filesFrom != "" {
		if err := o.readFilesFrom(); err != nil {
			return nil, err
		}
	}
	return o, nil
}

const cvsIgnore = `RCS SCCS CVS CVS.adm RCSLOG cvslog.* tags TAGS .make.state .nse_depinfo
*~ #* .#* ,* _$* *$ *.old *.bak *.BAK *.orig *.rej .del-* *.a *.olb *.o *.obj
*.so *.exe *.Z *.elc *.ln core .svn/ .git/ .hg/ .bzr/`

// readFilesFrom loads the --files-from list. Like rsync it implies
// --relative and --dirs, and -a no longer implies -r.
func (o *Options) readFilesFrom() error {
	// ":file" or "host:file"; a one-letter prefix is a Windows drive.
	if i := strings.IndexByte(o.filesFrom, ':'); i == 0 || i > 1 && !strings.ContainsAny(o.filesFrom[:i], `/\`) {
		return fmt.Errorf("--files-from on a remote host is not supported; give a local file")
	}
	var data []byte
	var err error
	if o.filesFrom == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(o.filesFrom)
	}
	if err != nil {
		return err
	}
	sep := "\n"
	if o.from0 {
		sep = "\x00"
	}
	o.FileList = []string{}
	for _, line := range strings.Split(string(data), sep) {
		if !o.from0 {
			line = strings.TrimRight(line, "\r")
			if line == "" || line[0] == '#' || line[0] == ';' {
				continue
			}
		}
		if line != "" {
			o.FileList = append(o.FileList, line)
		}
	}
	o.FilesFrom, o.Relative, o.Dirs = true, true, true
	if !o.recursiveSet {
		o.Recursive = false
	}
	return nil
}

// ParseSize parses a size such as "100", "1.5M", "10KiB" or "2GB". Bare
// suffixes and *iB are powers of 1024, *B powers of 1000; unit is the
// multiplier for a number without a suffix.
func ParseSize(s string, unit int64) (int64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	num, suf := t[:i], strings.ToUpper(t[i:])
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := float64(unit)
	if suf != "" {
		base := 1024.0
		switch {
		case strings.HasSuffix(suf, "IB"):
			suf = strings.TrimSuffix(suf, "IB")
		case len(suf) == 2 && suf[1] == 'B':
			base, suf = 1000, suf[:1]
		}
		exp := strings.Index("BKMGTP", suf)
		if len(suf) != 1 || exp < 0 {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		mult = 1
		for range exp {
			mult *= base
		}
	}
	return int64(f * mult), nil
}

func (o *Options) short(c byte) error {
	switch c {
	case 'a':
		o.archive()
	case 'v':
		o.Verbose++
	case 'q':
		o.Quiet = true
	case 'z':
		o.Compress = true
	case 'u':
		o.Update = true
	case 'p':
		o.Perms = true
	case 'r':
		o.Recursive, o.recursiveSet = true, true
	case 't':
		o.Times = true
	case 'l':
		o.Links = true
	case 'L':
		o.CopyLinks = true
	case 'o':
		o.Owner = true
	case 'g':
		o.Group = true
	case 'D':
		o.Devices, o.Specials = true, true
	case 'd':
		o.Dirs = true
	case 'n':
		o.DryRun = true
	case 'c':
		o.Checksum = true
	case 'W':
		o.WholeFileMode = 1
	case 'P':
		o.Partial, o.Progress, o.NoProgress = true, true, false
	case 'h':
		o.Human = true
	case 'I', 'i', 'b', 'x', 'R', 'H', 'S', 'm', 'O', 'J', 'E', 'C':
		set, _ := boolSetter(shortToLong[c])
		set(o, true)
	case '0':
		o.from0 = true
	case '4', '6':
		o.IPVersion = c
	case '8':
		// File names are always passed through unchanged.
	default:
		if name, ok := unsupportedShort[c]; ok {
			return fmt.Errorf("option -%c (%s) is not supported by xt", c, name)
		}
		return fmt.Errorf("unknown option -%c", c)
	}
	return nil
}

func (o *Options) long(name string, hasVal bool, next func() (string, error)) error {
	if set, ok := boolSetter(name); ok && !hasVal {
		set(o, true)
		return nil
	}
	if neg, ok := strings.CutPrefix(name, "no-"); ok && !hasVal {
		if len(neg) == 1 {
			if l, ok := shortToLong[neg[0]]; ok {
				neg = l
			}
		}
		if set, ok := boolSetter(neg); ok {
			set(o, false)
			return nil
		}
	}
	switch name {
	case "archive":
		o.archive()
		return nil
	case "help":
		o.Help = true
		return nil
	case "version":
		o.Version = true
		return nil
	}
	val, err := func() (string, error) {
		switch name {
		case "rsh", "rsync-path", "xt-path", "exclude", "include", "exclude-from",
			"include-from", "filter", "modify-window", "compress-level", "max-size", "min-size",
			"suffix", "backup-dir", "bwlimit", "timeout", "files-from", "chmod", "max-delete", "info":
			return next()
		}
		return "", nil
	}()
	if err != nil {
		return err
	}
	switch name {
	case "rsh":
		o.Rsh = val
	case "rsync-path", "xt-path":
		o.XtPath = val
	case "exclude":
		o.Rules = append(o.Rules, filter.Rule{Pattern: val})
	case "include":
		o.Rules = append(o.Rules, filter.Rule{Include: true, Pattern: val})
	case "exclude-from", "include-from":
		return o.readRules(val, name == "include-from")
	case "filter":
		return o.addFilter(val)
	case "modify-window":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid --modify-window %q", val)
		}
		o.ModifyWindow = n
	case "compress-level":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 || n > 9 {
			return fmt.Errorf("invalid --compress-level %q (0-9)", val)
		}
		o.CompressLevel = n
		o.Compress = n > 0
	case "max-size", "min-size":
		n, err := ParseSize(val, 1)
		if err != nil {
			return fmt.Errorf("--%s: %v", name, err)
		}
		if name == "max-size" {
			o.MaxSize = n
		} else {
			o.MinSize = n
		}
	case "bwlimit":
		n, err := ParseSize(val, 1024)
		if err != nil {
			return fmt.Errorf("--bwlimit: %v", err)
		}
		o.BwLimit = n
	case "suffix":
		o.Suffix = val
	case "backup-dir":
		o.BackupDir = val
	case "timeout":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid --timeout %q", val)
		}
		o.Timeout = n
	case "max-delete":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid --max-delete %q", val)
		}
		o.MaxDelete = n
	case "files-from":
		o.filesFrom = val
	case "chmod":
		if o.Chmod != "" {
			val = o.Chmod + "," + val
		}
		o.Chmod = val
	case "info":
		return o.info(val)
	default:
		switch name {
		case "acls", "xattrs", "inplace", "append", "append-verify", "partial-dir",
			"temp-dir", "link-dest", "copy-dest", "compare-dest", "chown", "usermap", "groupmap",
			"contimeout", "fuzzy", "block-size", "daemon", "port", "password-file",
			"copy-dirlinks", "keep-dirlinks", "copy-unsafe-links", "safe-links", "atimes",
			"crtimes", "write-batch", "read-batch", "log-file", "out-format", "debug":
			return fmt.Errorf("option --%s is not supported by xt", name)
		}
		return fmt.Errorf("unknown option --%s", name)
	}
	return nil
}

// info handles the --info flags that map onto xt's output settings.
func (o *Options) info(val string) error {
	for _, f := range strings.Split(val, ",") {
		switch strings.ToLower(f) {
		case "progress2":
			// The live display already shows whole-transfer progress.
			o.Progress, o.NoProgress = false, false
		case "progress", "progress1":
			o.Progress, o.NoProgress = true, false
		case "progress0":
			o.Progress, o.NoProgress = false, true
		case "stats", "stats1", "stats2", "stats3":
			o.Stats = true
		case "stats0":
			o.Stats = false
		case "name", "name1", "name2":
			o.Verbose = max(o.Verbose, 1)
		case "name0":
			o.Verbose = 0
		case "":
		default:
			return fmt.Errorf("--info=%s is not supported by xt (supported: progress0-2, stats0-3, name0-2)", f)
		}
	}
	return nil
}

func (o *Options) addFilter(s string) error {
	r, err := filter.ParseRule(s)
	if err != nil {
		return err
	}
	o.Rules = append(o.Rules, r)
	return nil
}

func (o *Options) readRules(file string, include bool) error {
	var r io.Reader = os.Stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "+ ") || strings.HasPrefix(line, "- ") {
			if err := o.addFilter(line); err != nil {
				return err
			}
			continue
		}
		o.Rules = append(o.Rules, filter.Rule{Include: include, Pattern: line})
	}
	return sc.Err()
}

// Usage is the --help text.
const Usage = `xt - fast, resumable file transfer over ssh (rsync-compatible flags)

Usage:
  xt [OPTION...] SRC... [USER@]HOST:DEST    push to a remote host
  xt [OPTION...] [USER@]HOST:SRC... DEST    pull from a remote host
  xt [OPTION...] SRC... DEST                copy locally
  xt [OPTION...] SRC                        list files (like --list-only)

xt must be installed and on the PATH of the remote host (or use --xt-path).
A trailing slash on a source directory copies its contents rather than the
directory itself, exactly like rsync.

Options:
  -v, --verbose            list transferred files (repeat for more detail)
  -q, --quiet              suppress non-error messages
  -a, --archive            archive mode; equals -rlptgoD
  -r, --recursive          recurse into directories
  -d, --dirs               transfer directories without recursing
  -l, --links              copy symlinks as symlinks
  -L, --copy-links         transform symlinks into the files they point to
  -p, --perms              preserve permissions
  -t, --times              preserve modification times
  -o, --owner              preserve owner (super-user only)
  -g, --group              preserve group
  -D                       accepted for compatibility (devices are skipped)
  -R, --relative           use full path names ("/./" marks where they start)
  -H, --hard-links         preserve hard links
  -S, --sparse             write runs of zeros as holes
  -E, --executability      preserve executability (without -p)
      --chmod=CHMOD        change permissions of sent files (e.g. D755,F644)
  -O, --omit-dir-times     don't preserve directory mtimes
  -x, --one-file-system    don't cross filesystem boundaries
  -m, --prune-empty-dirs   don't send empty directories
      --mkpath             create the destination's missing parent dirs
  -u, --update             skip files that are newer on the receiver
      --existing           skip creating new files on the receiver
      --ignore-existing    skip updating files that exist on the receiver
  -I, --ignore-times       don't skip files that match size and mtime
  -c, --checksum           compare files by checksum, not size and mtime
      --size-only          compare files by size only
      --modify-window=N    treat mtimes within N seconds as equal
      --max-size=SIZE      don't transfer files larger than SIZE (e.g. 10M)
      --min-size=SIZE      don't transfer files smaller than SIZE
  -W, --whole-file         send whole files (no delta; default for local copies)
      --no-whole-file      always use the delta algorithm
      --partial            keep partially transferred files for resuming
  -P                       same as --partial --progress
      --progress           show per-file progress lines
      --no-progress        disable the live progress display
      --delete             delete extraneous files from destination dirs
      --delete-after       delete after the transfer, not during it
      --delete-excluded    also delete excluded files from destination dirs
      --max-delete=NUM     don't delete more than NUM files (exit 25 if hit)
      --ignore-errors      delete even if there were I/O errors on the sender
      --force              replace directories with non-directories
      --remove-source-files  remove synchronized files from the sender
  -b, --backup             keep replaced and deleted files as backups
      --suffix=SUFFIX      backup suffix (default "~" without --backup-dir)
      --backup-dir=DIR     put backups in DIR (relative to the destination)
  -n, --dry-run            show what would be transferred
  -i, --itemize-changes    print a change summary for each updated item
      --list-only          list the source files instead of copying
  -z, --compress           compress data on the wire
      --compress-level=N   compression level 1-9 (default 6)
      --exclude=PATTERN    exclude files matching PATTERN
      --include=PATTERN    don't exclude files matching PATTERN
      --exclude-from=FILE  read exclude patterns from FILE
      --include-from=FILE  read include patterns from FILE
  -f, --filter=RULE        add a filter rule ("- PATTERN" or "+ PATTERN")
  -C, --cvs-exclude        exclude files the way CVS does (*.o, .git/, ...)
      --files-from=FILE    read the list of source files from FILE ("-" = stdin)
  -0, --from0              --files-from names are separated by NUL bytes
  -e, --rsh=COMMAND        remote shell to use (default "ssh")
      --xt-path=PROGRAM    xt command on the remote host (alias --rsync-path)
  -4, -6                   make ssh use IPv4 / IPv6
      --bwlimit=RATE       limit outgoing data rate (KiB/s, or with a suffix)
      --timeout=SECONDS    exit 30 if no data arrives for SECONDS
  -h, --human-readable     output numbers in a human-readable format
      --stats              print detailed transfer statistics
      --info=FLAGS         progress2 (live display), progress, stats, name
      --version            print version
      --help               show this help

Interrupted transfers resume automatically when run with --partial or -P:
the partial file is verified by hash and only the remainder is sent.
`
