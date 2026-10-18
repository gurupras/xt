package xfer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gurupras/xt/internal/delta"
	"github.com/gurupras/xt/internal/filter"
	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
)

// connError marks errors from the connection, which end the session.
type connError struct{ err error }

func (e connError) Error() string { return "connection lost: " + e.err.Error() }
func (e connError) Unwrap() error { return e.err }

// RemoteError is an error reported by the peer.
type RemoteError string

func (e RemoteError) Error() string { return "remote: " + string(e) }

// Sender reads local files and streams them to a Receiver.
type Sender struct {
	Conn *proto.Conn
	Opts *opt.Options
	UI   *UI
	// Track makes this side drive the client's transfer progress display.
	Track bool

	Stats     proto.Stats
	PeerStats *proto.Stats

	files []proto.FileEntry
	paths []string
}

const listChunk = 1000

// Run lists srcs, sends the file list and serves requests until the
// receiver is done.
func (s *Sender) Run(srcs []string) error {
	filt, err := filter.Compile(s.Opts.Rules)
	if err != nil {
		return err
	}
	if s.UI.Client() {
		s.UI.Phase("scanning")
	}
	l := &lister{o: s.Opts, ui: s.UI, filt: filt, seen: map[string]bool{}}
	if s.Opts.Chmod != "" {
		if l.chmod, err = opt.ParseChmod(s.Opts.Chmod); err != nil {
			return err
		}
	}
	if s.Opts.FilesFrom {
		if len(srcs) != 1 {
			return errors.New("--files-from needs exactly one source directory")
		}
		for _, f := range s.Opts.FileList {
			l.addSource(strings.TrimRight(srcs[0], `/\`) + "/./" + f)
		}
	} else {
		for _, src := range srcs {
			l.addSource(src)
		}
	}
	l.finish()
	s.files, s.paths = l.entries, l.paths
	if s.UI.Client() {
		s.UI.Listed(int64(len(s.files)))
	}
	s.Stats.Errors += l.errs
	countList(&s.Stats, s.files)

	for i := 0; i < len(s.files); i += listChunk {
		chunk := s.files[i:min(i+listChunk, len(s.files))]
		if err := s.Conn.Send(&proto.Msg{Files: chunk}); err != nil {
			return connError{err}
		}
	}
	if err := s.Conn.Send(&proto.Msg{FilesEnd: true, IOError: l.errs > 0}); err != nil {
		return connError{err}
	}
	if s.Track {
		s.UI.Phase("remote-compare")
	}

	for {
		m, err := s.Conn.Recv()
		if err != nil {
			return connError{err}
		}
		switch {
		case m.Request != nil:
			if err := s.handle(m.Request); err != nil {
				return err
			}
		case m.Plan != nil:
			if s.Track {
				s.UI.SetPlan(m.Plan.Files, m.Plan.Bytes)
			}
		case m.PhaseEnd:
			if err := s.Conn.Send(&proto.Msg{PhaseEnd: true}); err != nil {
				return connError{err}
			}
		case m.Synced != nil:
			s.removeSources(m.Synced)
		case m.Done != nil:
			s.PeerStats = m.Done
			if err := s.Conn.Send(&proto.Msg{Done: &s.Stats}); err != nil {
				return connError{err}
			}
			return nil
		case m.Log != nil:
			s.UI.Relay(m.Log)
		case m.Error != "":
			return RemoteError(m.Error)
		default:
			return errors.New("protocol error: unexpected message from receiver")
		}
	}
}

// removeSources deletes source files the receiver now has
// (--remove-source-files), unless they changed since they were listed.
func (s *Sender) removeSources(idxs []int32) {
	if !s.Opts.RemoveSource || s.Opts.DryRun {
		return
	}
	for _, i := range idxs {
		if i < 0 || int(i) >= len(s.files) || s.files[i].Type == proto.TypeDir {
			continue
		}
		f, p := &s.files[i], s.paths[i]
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if f.Type == proto.TypeFile && (!fi.Mode().IsRegular() || fi.Size() != f.Size || fi.ModTime().UnixNano() != f.MTime) {
			s.UI.Error("not removing %s: it changed during the transfer", f.Path)
			s.Stats.Errors++
			continue
		}
		if err := os.Remove(p); err != nil {
			s.UI.Error("removing source %s: %v", f.Path, err)
			s.Stats.Errors++
		} else if s.Opts.Verbose > 1 {
			s.UI.Info("sender removed %s", f.Path)
		}
	}
}

func countList(st *proto.Stats, files []proto.FileEntry) {
	for _, f := range files {
		st.NumFiles++
		switch f.Type {
		case proto.TypeDir:
			st.NumDirs++
		case proto.TypeSymlink:
			st.NumLinks++
		case proto.TypeFile:
			st.NumRegular++
			st.TotalSize += f.Size
		}
	}
}

func (s *Sender) handle(req *proto.Request) error {
	if req.Index < 0 || int(req.Index) >= len(s.files) || s.files[req.Index].Type != proto.TypeFile {
		return fmt.Errorf("protocol error: bad file index %d", req.Index)
	}
	f := &s.files[req.Index]
	s.Stats.Transferred++
	s.Stats.TransferredSize += f.Size
	if req.DryRun {
		if s.Track && s.Opts.Verbose > 0 && s.Opts.Itemize == 0 {
			s.UI.Info("%s", f.Path)
		}
		return nil
	}
	var st delta.Stats
	ok := false
	if s.Track {
		defer func() { s.UI.FileEnd(proto.Stats{Literal: st.Literal, Matched: st.Matched}, ok) }()
	}

	fh, err := os.Open(s.paths[req.Index])
	if err != nil {
		return s.fail(req.Index, err)
	}
	defer fh.Close()

	h := sha256.New()
	var resumeFrom int64
	if req.ResumeLen > 0 {
		n, err := io.CopyN(h, fh, req.ResumeLen)
		if err == nil && n == req.ResumeLen && bytes.Equal(h.Sum(nil), req.ResumeSum) {
			resumeFrom = req.ResumeLen
		} else {
			if _, err := fh.Seek(0, io.SeekStart); err != nil {
				return s.fail(req.Index, err)
			}
			h.Reset()
		}
	}
	if err := s.Conn.Send(&proto.Msg{Begin: &proto.Begin{Index: req.Index, ResumeFrom: resumeFrom}}); err != nil {
		return connError{err}
	}
	var r io.Reader = io.TeeReader(fh, h)
	if s.Track {
		s.UI.FileStart(f.Path, f.Size, resumeFrom)
		r = io.TeeReader(r, s.UI)
	}
	b := &opBatcher{conn: s.Conn}
	st, err = delta.Generate(r, req.Sig, b.add)
	if err == nil {
		err = b.flush()
	}
	s.Stats.Literal += st.Literal
	s.Stats.Matched += st.Matched
	if b.err != nil {
		return connError{b.err}
	}
	if err != nil {
		s.UI.Error("reading %s: %v", f.Path, err)
		return s.send(&proto.Msg{End: &proto.End{Err: err.Error()}})
	}
	ok = true
	return s.send(&proto.Msg{End: &proto.End{Sum: h.Sum(nil)}})
}

// fail reports a file that could not be read; the receiver skips it.
func (s *Sender) fail(idx int32, err error) error {
	s.UI.Error("send %s: %v", s.files[idx].Path, err)
	if err := s.send(&proto.Msg{Begin: &proto.Begin{Index: idx}}); err != nil {
		return err
	}
	return s.send(&proto.Msg{End: &proto.End{Err: err.Error()}})
}

func (s *Sender) send(m *proto.Msg) error {
	if err := s.Conn.Send(m); err != nil {
		return connError{err}
	}
	return nil
}

// opBatcher groups delta ops into messages of a reasonable size.
type opBatcher struct {
	conn *proto.Conn
	ops  []delta.Op
	lit  int
	err  error
}

func (b *opBatcher) add(op delta.Op) error {
	if op.Kind == delta.OpLiteral {
		op.Data = bytes.Clone(op.Data)
		b.lit += len(op.Data)
	}
	b.ops = append(b.ops, op)
	if b.lit >= delta.MaxLiteral || len(b.ops) >= 512 {
		return b.flush()
	}
	return nil
}

func (b *opBatcher) flush() error {
	if len(b.ops) == 0 {
		return nil
	}
	err := b.conn.Send(&proto.Msg{Ops: b.ops})
	b.ops, b.lit = nil, 0
	if err != nil {
		b.err = err
	}
	return err
}

// lister builds the file list.
type lister struct {
	o       *opt.Options
	ui      *UI
	filt    *filter.List
	chmod   []opt.ChmodRule
	entries []proto.FileEntry
	paths   []string
	links   []*inodeKey // with --hard-links, the inode of each multiply-linked file
	seen    map[string]bool
	errs    int64
}

func (l *lister) add(e proto.FileEntry, abs string, fi fs.FileInfo) bool {
	if l.seen[e.Path] {
		return false
	}
	l.seen[e.Path] = true
	var key *inodeKey
	if l.o.HardLinks && e.Type == proto.TypeFile {
		if k, ok := hardLinkKey(fi); ok {
			key = &k
		}
	}
	l.entries = append(l.entries, e)
	l.paths = append(l.paths, abs)
	l.links = append(l.links, key)
	if len(l.entries)%256 == 0 && l.ui.Client() {
		l.ui.Listed(int64(len(l.entries)))
	}
	return true
}

// finish prunes empty directories (-m) and links up hard links (-H).
func (l *lister) finish() {
	if l.o.PruneEmpty {
		keep := map[string]bool{".": true}
		for _, e := range l.entries {
			if e.Type == proto.TypeDir {
				continue
			}
			for d := path.Dir(e.Path); !keep[d]; d = path.Dir(d) {
				keep[d] = true
			}
		}
		n := 0
		for i, e := range l.entries {
			if e.Type == proto.TypeDir && !keep[e.Path] {
				continue
			}
			l.entries[n], l.paths[n], l.links[n] = e, l.paths[i], l.links[i]
			n++
		}
		l.entries, l.paths, l.links = l.entries[:n], l.paths[:n], l.links[:n]
	}
	first := map[inodeKey]int{}
	for i, k := range l.links {
		if k == nil {
			continue
		}
		if j, ok := first[*k]; ok {
			l.entries[i].HardLink = int32(j + 1)
		} else {
			first[*k] = i
		}
	}
}

func (l *lister) stat(p string) (fs.FileInfo, error) {
	if !l.o.CopyLinks {
		return os.Lstat(p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		if lfi, lerr := os.Lstat(p); lerr == nil && lfi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink has no referent")
		}
	}
	return fi, err
}

func (l *lister) warn(format string, args ...any) {
	l.ui.Error(format, args...)
	l.errs++
}

func (l *lister) entry(rel, abs string, fi fs.FileInfo) (proto.FileEntry, bool) {
	m := fi.Mode()
	e := proto.FileEntry{Path: rel, Mode: unixPerm(m), MTime: fi.ModTime().UnixNano()}
	e.UID, e.GID, e.HasOwner = ownerOf(fi)
	switch {
	case m.IsRegular():
		e.Type = proto.TypeFile
		e.Size = fi.Size()
		if l.o.Checksum {
			sum, err := fileSum(abs)
			if err != nil {
				l.warn("checksum %s: %v", rel, err)
				return e, false
			}
			e.Sum = sum
		}
	case m.IsDir():
		e.Type = proto.TypeDir
	case m&fs.ModeSymlink != 0:
		if !l.o.Links {
			if l.o.Verbose > 0 {
				l.ui.Info("skipping non-regular file %q", rel)
			}
			return e, false
		}
		target, err := os.Readlink(abs)
		if err != nil {
			l.warn("readlink %s: %v", rel, err)
			return e, false
		}
		e.Type = proto.TypeSymlink
		e.Link = target
	default:
		if l.o.Verbose > 0 {
			l.ui.Info("skipping non-regular file %q", rel)
		}
		return e, false
	}
	if l.chmod != nil && e.Type != proto.TypeSymlink {
		e.Mode = opt.ApplyChmod(l.chmod, e.Mode, e.Type == proto.TypeDir)
	}
	return e, true
}

func (l *lister) addSource(src string) {
	if l.o.Relative {
		l.addRelative(src)
		return
	}
	contents := hasTrailingSep(src)
	clean := filepath.Clean(src)
	base := filepath.Base(clean)
	if base == "." || base == ".." || base == string(filepath.Separator) || filepath.VolumeName(clean)+string(filepath.Separator) == clean {
		contents = true
	}
	fi, err := l.stat(clean)
	if err != nil {
		l.warn("%s: %v", src, err)
		return
	}
	rel := base
	if fi.IsDir() && contents {
		rel = "."
	}
	l.addItem(clean, rel, fi, contents)
}

// addRelative lists src under its full path (-R). A "/./" in src marks
// where the path sent to the receiver starts.
func (l *lister) addRelative(src string) {
	slashed := filepath.ToSlash(src)
	var root, rel string
	if i := strings.Index(slashed, "/./"); i >= 0 {
		root, rel = src[:i], slashed[i+3:]
		if root == "" {
			root = "/"
		}
	} else {
		vol := filepath.VolumeName(src)
		root, rel = ".", slashed[len(vol):]
		if strings.HasPrefix(rel, "/") {
			root = vol + string(filepath.Separator)
		}
	}
	// Drop leading slashes and any ".." that would climb above root.
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" {
		rel = "."
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := l.stat(abs)
	if err != nil {
		l.warn("%s: %v", src, err)
		return
	}
	// Send the implied parent directories first.
	var parents []string
	for d := path.Dir(rel); d != "."; d = path.Dir(d) {
		parents = append(parents, d)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		d := parents[i]
		if l.seen[d] {
			continue
		}
		dabs := filepath.Join(root, filepath.FromSlash(d))
		dfi, err := os.Stat(dabs)
		if err != nil || !dfi.IsDir() {
			l.warn("%s: implied directory %s is not a directory", src, d)
			return
		}
		e, _ := l.entry(d, dabs, dfi)
		l.add(e, dabs, dfi)
	}
	l.addItem(abs, rel, fi, rel == ".")
}

// addItem adds one source item, descending into it if it is a directory.
// contents means the directory's entries were asked for, as with "dir/".
func (l *lister) addItem(abs, rel string, fi fs.FileInfo, contents bool) {
	if !fi.IsDir() {
		if l.filt.Excluded(rel, false) {
			return
		}
		if e, ok := l.entry(rel, abs, fi); ok {
			l.add(e, abs, fi)
		}
		return
	}
	if !l.o.Recursive && !l.o.Dirs {
		l.ui.Info("skipping directory %s", rel)
		return
	}
	if rel != "." && l.filt.Excluded(rel, true) {
		return
	}
	e, _ := l.entry(rel, abs, fi)
	if !l.add(e, abs, fi) && !l.o.Relative {
		return
	}
	switch {
	case l.o.Recursive:
		l.walk(abs, rel, []fs.FileInfo{fi}, true)
	case contents:
		// -d with "dir/" lists the directory's entries without recursing.
		l.walk(abs, rel, []fs.FileInfo{fi}, false)
	}
}

func (l *lister) walk(abs, rel string, ancestors []fs.FileInfo, recurse bool) {
	ents, err := os.ReadDir(abs)
	if err != nil {
		l.warn("%s: %v", rel, err)
	}
	for _, de := range ents {
		name := de.Name()
		cabs := filepath.Join(abs, name)
		crel := name
		if rel != "." {
			crel = rel + "/" + name
		}
		fi, err := l.stat(cabs)
		if err != nil {
			l.warn("%s: %v", crel, err)
			continue
		}
		if l.filt.Excluded(crel, fi.IsDir()) {
			continue
		}
		e, ok := l.entry(crel, cabs, fi)
		if !ok || !l.add(e, cabs, fi) {
			continue
		}
		if !fi.IsDir() || !recurse {
			continue
		}
		if l.o.OneFS && !sameDev(ancestors[0], fi) {
			// The mount point itself is sent, but not its contents.
			continue
		}
		loop := false
		for _, a := range ancestors {
			if os.SameFile(a, fi) {
				loop = true
			}
		}
		if loop {
			l.warn("%s: directory loop, not descending", crel)
			continue
		}
		l.walk(cabs, crel, append(ancestors, fi), true)
	}
}

func sameDev(a, b fs.FileInfo) bool {
	da, ok1 := devOf(a)
	db, ok2 := devOf(b)
	return !ok1 || !ok2 || da == db
}
