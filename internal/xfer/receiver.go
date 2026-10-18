package xfer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gurupras/xt/internal/delta"
	"github.com/gurupras/xt/internal/filter"
	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
)

// Receiver writes files sent by a Sender into Dest.
type Receiver struct {
	Conn *proto.Conn
	Opts *opt.Options
	UI   *UI
	Dest string
	// PeerOS is the sender's GOOS; permission bits from Windows are not
	// meaningful and are not applied.
	PeerOS string
	// Track makes this side drive the client's transfer progress display.
	Track bool
	// ListProgress shows file list reception on the client.
	ListProgress bool

	Stats     proto.Stats
	PeerStats *proto.Stats

	files    []proto.FileEntry
	filt     *filter.List
	root     string
	single   bool // destination names a single file
	listed   map[string]bool
	badName  map[int]error   // entries whose names this OS cannot represent
	dirs     map[string]bool // directories verified safe to write into
	dirFixes []dirFix
	noDelete bool // the sender had I/O errors; deleting could lose data

	// Written by generate, read by receive once genDone is closed.
	deletes  []pendingDelete // --delete-after: directories to clean at the end
	final    bool            // generate is done; deletions run immediately
	leaderOK map[int]bool    // -H: files that will be present at the end
	links    []int           // -H: entries to hard-link at the end
	genDone  chan struct{}   // closed when generate returns

	syncMu sync.Mutex
	synced []int32 // --remove-source-files: entries now current here

	jobs chan *job
	redo chan []int32
	errs atomic.Int64
}

type pendingDelete struct{ rel, dir string }

type dirFix struct {
	idx     int
	path    string
	created bool
}

type job struct {
	idx        int32
	target     string
	partial    string
	sig        *delta.Signature
	resumeLen  int64
	resumeHash hash.Hash
	existing   fs.FileInfo // regular file previously at target
	redo       bool
}

// Run receives the file list and then the files.
func (r *Receiver) Run() error {
	filt, err := filter.Compile(r.Opts.Rules)
	if err != nil {
		return err
	}
	r.filt = filt
	if r.ListProgress {
		r.UI.Phase("receiving file list")
	}
	for done := false; !done; {
		m, err := r.Conn.Recv()
		if err != nil {
			return connError{err}
		}
		switch {
		case m.Files != nil:
			r.files = append(r.files, m.Files...)
			if r.ListProgress {
				r.UI.Listed(int64(len(r.files)))
			}
		case m.FilesEnd:
			done = true
			if m.IOError && r.Opts.Delete && !r.Opts.IgnoreErrors {
				r.UI.Error("I/O error on the sending side -- skipping file deletion (use --ignore-errors to delete anyway)")
				r.noDelete = true
			}
		case m.Log != nil:
			r.UI.Relay(m.Log)
		case m.Error != "":
			return RemoteError(m.Error)
		default:
			return errors.New("protocol error: expected file list")
		}
	}
	r.UI.Listed(int64(len(r.files)))
	r.listed = make(map[string]bool, len(r.files))
	for i, e := range r.files {
		if err := validRel(e.Path); err != nil {
			return fmt.Errorf("refusing file list: %w", err)
		}
		if r.listed[e.Path] || (e.Path == "." && i != 0) {
			return fmt.Errorf("refusing file list: duplicate entry %q", e.Path)
		}
		if e.Path == "." && e.Type != proto.TypeDir {
			// "." is the destination root, which must stay a directory.
			return errors.New("refusing file list: \".\" is not a directory")
		}
		r.listed[e.Path] = true
		if l := e.HardLink; l != 0 && (l < 1 || int(l) > i || r.files[l-1].Type != proto.TypeFile || r.files[l-1].HardLink != 0 || e.Type != proto.TypeFile) {
			return fmt.Errorf("refusing file list: bad hard link on %q", e.Path)
		}
	}
	countList(&r.Stats, r.files)
	if r.Opts.ListOnly {
		r.list()
	} else if err := r.setupDest(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" && !r.single {
		r.badName = make(map[int]error)
		for i, e := range r.files {
			if err := winNameErr(e.Path); err != nil {
				r.badName[i] = err
			}
		}
	}

	r.jobs = make(chan *job, 1024)
	r.redo = make(chan []int32, 1)
	r.genDone = make(chan struct{})
	genErr := make(chan error, 1)
	go func() {
		defer close(r.genDone)
		genErr <- r.generate()
	}()
	if err := r.receive(); err != nil {
		return err
	}
	return <-genErr
}

func (r *Receiver) setupDest() error {
	dest := r.Dest
	if dest == "" {
		dest = "."
	}
	r.root = filepath.Clean(dest)
	r.dirs = map[string]bool{".": true}
	st, err := os.Stat(r.root)
	isDir := err == nil && st.IsDir()
	if len(r.files) == 1 && r.files[0].Type != proto.TypeDir && !hasTrailingSep(dest) && !isDir && !r.Opts.Relative {
		r.single = true
		if r.Opts.Mkpath && !r.Opts.DryRun {
			return os.MkdirAll(filepath.Dir(r.root), 0o755)
		}
		return nil
	}
	if isDir {
		return nil
	}
	if err == nil {
		return fmt.Errorf("destination %s is not a directory", dest)
	}
	if len(r.files) == 0 {
		return nil
	}
	if r.Opts.DryRun {
		r.UI.Info("created directory %s", dest)
		return nil
	}
	mkdir := os.Mkdir
	if r.Opts.Mkpath {
		mkdir = os.MkdirAll
	}
	if err := mkdir(r.root, 0o755); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cannot create destination %s: parent directory does not exist (use --mkpath)", dest)
		}
		return err
	}
	r.UI.Info("created directory %s", dest)
	return nil
}

// list prints the file list instead of transferring it (--list-only).
func (r *Receiver) list() {
	for _, e := range r.files {
		kind := "-"
		switch e.Type {
		case proto.TypeDir:
			kind = "d"
		case proto.TypeSymlink:
			kind = "l"
		}
		name := e.Path
		if e.Type == proto.TypeSymlink {
			name += " -> " + e.Link
		}
		perm := fileMode(e.Mode).Perm().String()[1:]
		t := time.Unix(0, e.MTime).Format("2006/01/02 15:04:05")
		r.UI.Info("%s%s %14s %s %s", kind, perm, commas(e.Size), t, name)
	}
}

func (r *Receiver) pathFor(e *proto.FileEntry) string {
	if r.single || e.Path == "." {
		return r.root
	}
	return filepath.Join(r.root, filepath.FromSlash(e.Path))
}

// checkParents ensures every ancestor of rel is a directory this run has
// verified, so a symlink in the file list can never redirect writes outside
// the destination.
func (r *Receiver) checkParents(rel string) error {
	if r.single || rel == "." {
		return nil
	}
	for d := path.Dir(rel); d != "."; d = path.Dir(d) {
		if !r.dirs[d] {
			return fmt.Errorf("parent directory %s is missing or not a directory", d)
		}
	}
	return nil
}

func (r *Receiver) fail(rel string, err error) {
	r.UI.Error("%s: %v", rel, err)
	r.errs.Add(1)
}

func (r *Receiver) sameTime(t time.Time, ns int64) bool {
	d := t.Unix() - time.Unix(0, ns).Unix()
	return max(d, -d) <= r.Opts.ModifyWindow
}

func (r *Receiver) newerThan(t time.Time, ns int64) bool {
	return t.Unix()-time.Unix(0, ns).Unix() > r.Opts.ModifyWindow
}

func (r *Receiver) permsMeaningful() bool { return r.Opts.Perms && r.PeerOS != "windows" }

// generate decides what to do with each entry, then requests the files that
// need transferring. It runs concurrently with receive.
func (r *Receiver) generate() error {
	if r.Track {
		r.UI.Phase("comparing")
	}
	var todo []int32
	var bytes int64
	for i := range r.files {
		if r.prepare(i) {
			todo = append(todo, int32(i))
			bytes += r.files[i].Size
		}
		if r.Track && i%64 == 0 {
			r.UI.Compared(int64(i))
		}
	}
	if r.Track {
		r.UI.SetPlan(int64(len(todo)), bytes)
	}
	if err := r.Conn.Send(&proto.Msg{Plan: &proto.Plan{Files: int64(len(todo)), Bytes: bytes}}); err != nil {
		return connError{err}
	}
	for _, idx := range todo {
		if err := r.request(idx, false); err != nil {
			return err
		}
	}
	if err := r.Conn.Send(&proto.Msg{PhaseEnd: true}); err != nil {
		return connError{err}
	}
	for _, idx := range <-r.redo {
		if err := r.request(idx, true); err != nil {
			return err
		}
	}
	if err := r.Conn.Send(&proto.Msg{PhaseEnd: true}); err != nil {
		return connError{err}
	}
	return nil
}

// prepare handles directories and symlinks and reports whether a regular
// file needs to be transferred.
func (r *Receiver) prepare(i int) bool {
	e := &r.files[i]
	if r.Opts.ListOnly {
		return false
	}
	if err := r.badName[i]; err != nil {
		r.fail(e.Path, err)
		return false
	}
	target := r.pathFor(e)
	if err := r.checkParents(e.Path); err != nil {
		r.fail(e.Path, err)
		return false
	}
	st, statErr := os.Lstat(target)
	exists := statErr == nil
	switch e.Type {
	case proto.TypeDir:
		r.prepareDir(i, target, st, exists)
		return false
	case proto.TypeSymlink:
		r.prepareSymlink(i, e, target, st, exists)
		return false
	}
	if exists && st.IsDir() && !(r.Opts.Delete || r.Opts.Force) {
		r.fail(e.Path, errors.New("cannot replace a directory with a file (use --force or --delete)"))
		return false
	}
	if r.Opts.Existing && !exists {
		return false
	}
	if r.Opts.IgnoreExisting && exists {
		return false
	}
	if r.Opts.MaxSize > 0 && e.Size > r.Opts.MaxSize || e.Size < r.Opts.MinSize {
		if r.Opts.Verbose > 1 {
			r.UI.Info("%s is outside the size limits, skipping", e.Path)
		}
		return false
	}
	if r.Opts.Update && exists && !st.IsDir() && r.newerThan(st.ModTime(), e.MTime) {
		if r.Opts.Verbose > 1 {
			r.UI.Info("%s is newer on the receiver, skipping", e.Path)
		}
		return false
	}
	if r.Opts.HardLinks && e.HardLink > 0 && r.leaderOK[int(e.HardLink-1)] {
		// Linked to its leader once the leader is in place.
		r.links = append(r.links, i)
		return false
	}
	regular := exists && st.Mode().IsRegular()
	if regular && !r.Opts.IgnoreTimes && r.unchanged(e, st, target) {
		r.itemize(".f", e, st, false)
		if !r.Opts.DryRun {
			r.fixAttrs(e, target, st)
		}
		r.markLeader(i)
		r.markSynced(int32(i))
		return false
	}
	if regular {
		r.itemize(r.arrow()+"f", e, st, true)
	} else {
		r.itemize(r.arrow()+"f", e, nil, true)
	}
	r.markLeader(i)
	return true
}

func (r *Receiver) markLeader(i int) {
	if r.Opts.HardLinks && r.files[i].HardLink == 0 {
		if r.leaderOK == nil {
			r.leaderOK = map[int]bool{}
		}
		r.leaderOK[i] = true
	}
}

func (r *Receiver) markSynced(i int32) {
	if !r.Opts.RemoveSource || r.Opts.DryRun {
		return
	}
	r.syncMu.Lock()
	r.synced = append(r.synced, i)
	r.syncMu.Unlock()
}

// arrow is the itemize prefix for a transferred file: '<' when the files
// are going to a remote host, '>' when they arrive on this one.
func (r *Receiver) arrow() string {
	if r.UI.Client() {
		return ">"
	}
	return "<"
}

// itemize prints an --itemize-changes line. code is the two-letter update
// type; st is the existing item, or nil if it is being created. Items with
// no changes are only shown with -ii.
func (r *Receiver) itemize(code string, e *proto.FileEntry, st fs.FileInfo, xfer bool) {
	if r.Opts.Itemize == 0 {
		return
	}
	name := e.Path
	if e.Type == proto.TypeDir && name != "." {
		name += "/"
	}
	if e.Type == proto.TypeSymlink {
		name += " -> " + e.Link
	}
	if st == nil {
		r.UI.Info("%s+++++++++ %s", code, name)
		return
	}
	f := []byte(".........")
	if e.Type == proto.TypeFile && xfer && r.Opts.Checksum {
		f[0] = 'c'
	}
	if e.Type == proto.TypeFile && st.Size() != e.Size {
		f[1] = 's'
	}
	if !(e.Type == proto.TypeDir && r.Opts.OmitDirTimes) && e.Type != proto.TypeSymlink && !r.sameTime(st.ModTime(), e.MTime) {
		f[2] = 'T'
		if r.Opts.Times {
			f[2] = 't'
		}
	}
	if r.permsMeaningful() && e.Type != proto.TypeSymlink && unixPerm(st.Mode()) != e.Mode {
		f[3] = 'p'
	}
	if uid, gid, ok := ownerOf(st); ok && e.HasOwner {
		if r.Opts.Owner && isRoot() && uid != e.UID {
			f[4] = 'o'
		}
		if r.Opts.Group && gid != e.GID {
			f[5] = 'g'
		}
	}
	flags := string(f)
	if flags == "........." && !xfer {
		if r.Opts.Itemize < 2 {
			return
		}
		flags = "         "
	}
	r.UI.Info("%s%s %s", code, flags, name)
}

func (r *Receiver) unchanged(e *proto.FileEntry, st fs.FileInfo, target string) bool {
	if st.Size() != e.Size {
		return false
	}
	if r.Opts.Checksum {
		sum, err := fileSum(target)
		return err == nil && bytes.Equal(sum, e.Sum)
	}
	if r.Opts.SizeOnly {
		return true
	}
	return r.sameTime(st.ModTime(), e.MTime)
}

func (r *Receiver) prepareDir(i int, target string, st fs.FileInfo, exists bool) {
	e := &r.files[i]
	dry := r.Opts.DryRun
	if e.Path == "." {
		// The destination root, already set up by setupDest.
		if st != nil {
			r.itemize(".d", e, st, false)
		}
		r.dirFixes = append(r.dirFixes, dirFix{idx: i, path: target})
		r.deleteExtraneous(e.Path, target)
		return
	}
	if exists && !st.IsDir() {
		if !dry {
			var err error
			if r.Opts.Backup {
				err = r.backup(e.Path, target)
			} else {
				err = os.Remove(target)
			}
			if err != nil {
				r.fail(e.Path, err)
				return
			}
		}
		exists = false
	}
	if !exists {
		if !dry {
			if err := os.Mkdir(target, 0o700); err != nil {
				r.fail(e.Path, err)
				return
			}
		}
		r.itemize("cd", e, nil, false)
		if r.Opts.Verbose > 0 && r.Opts.Itemize == 0 {
			r.UI.Info("%s/", e.Path)
		}
	} else {
		r.itemize(".d", e, st, false)
	}
	r.dirs[e.Path] = true
	r.dirFixes = append(r.dirFixes, dirFix{idx: i, path: target, created: !exists})
	if exists {
		r.deleteExtraneous(e.Path, target)
	}
}

func (r *Receiver) deleteExtraneous(rel, dir string) {
	if !r.Opts.Delete || !r.Opts.Recursive || r.single || r.noDelete {
		return
	}
	if r.Opts.DeleteAfter && !r.final {
		r.deletes = append(r.deletes, pendingDelete{rel, dir})
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.fail(rel, err)
		}
		return
	}
	join := func(name string) string {
		if rel == "." {
			return name
		}
		return rel + "/" + name
	}
	for _, de := range ents {
		name := de.Name()
		child := join(name)
		if r.listed[child] {
			continue
		}
		if orig, ok := partialOrigin(name); ok && r.listed[join(orig)] {
			continue
		}
		if r.filt.Excluded(child, de.IsDir()) && !r.Opts.DeleteExcluded {
			continue
		}
		abs := filepath.Join(dir, name)
		if r.protected(name, abs) {
			continue
		}
		if r.Opts.MaxDelete >= 0 && r.Stats.Deleted >= r.Opts.MaxDelete {
			r.Stats.DeleteSkipped++
			continue
		}
		suffix := ""
		if de.IsDir() {
			suffix = "/"
		}
		switch {
		case r.Opts.Itemize > 0:
			r.UI.Info("%-11s %s%s", "*deleting", child, suffix)
		case r.Opts.Verbose > 0:
			r.UI.Info("deleting %s%s", child, suffix)
		}
		r.Stats.Deleted++
		if r.Opts.DryRun {
			continue
		}
		if r.Opts.Backup {
			if err := r.backup(child, abs); err != nil {
				r.fail(child, err)
			}
			continue
		}
		if err := os.RemoveAll(abs); err != nil {
			r.fail(child, err)
		}
	}
}

// protected reports whether a destination entry must survive --delete:
// earlier backups and the backup directory itself.
func (r *Receiver) protected(name, abs string) bool {
	if !r.Opts.Backup {
		return false
	}
	if r.Opts.BackupDir == "" {
		return r.Opts.Suffix != "" && strings.HasSuffix(name, r.Opts.Suffix)
	}
	return filepath.Clean(abs) == r.backupRoot()
}

func (r *Receiver) backupRoot() string {
	dir := r.Opts.BackupDir
	if !filepath.IsAbs(dir) {
		base := r.root
		if r.single {
			base = filepath.Dir(r.root)
		}
		dir = filepath.Join(base, dir)
	}
	return filepath.Clean(dir)
}

// backup moves an existing destination item out of the way (--backup).
func (r *Receiver) backup(rel, target string) error {
	dst := target + r.Opts.Suffix
	if r.Opts.BackupDir != "" {
		name := rel
		if r.single {
			name = filepath.Base(target)
		}
		dst = filepath.Join(r.backupRoot(), filepath.FromSlash(name)) + r.Opts.Suffix
		if err := r.mkBackupDir(filepath.Dir(dst)); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	if st, err := os.Lstat(dst); err == nil && st.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	if err := rename(target, dst); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if r.Opts.Verbose > 1 {
		r.UI.Info("backed up %s to %s", rel, dst)
	}
	return nil
}

// mkBackupDir creates dir inside the --backup-dir tree. Below the
// destination (or below the backup root, if that is elsewhere) it creates
// one component at a time and refuses symlinks, which the sender may have
// placed there in this or an earlier run to redirect backups.
func (r *Receiver) mkBackupDir(dir string) error {
	base := r.backupRoot()
	if rel, err := filepath.Rel(r.root, base); err == nil && !r.single && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		base = r.root
	} else if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return err
	}
	p := base
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		if c == "." {
			continue
		}
		p = filepath.Join(p, c)
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", p)
		}
	}
	return nil
}

func (r *Receiver) prepareSymlink(i int, e *proto.FileEntry, target string, st fs.FileInfo, exists bool) {
	if exists && st.Mode()&fs.ModeSymlink != 0 {
		if cur, err := os.Readlink(target); err == nil && cur == e.Link {
			r.markSynced(int32(i))
			return
		}
	}
	if exists && st.IsDir() && !(r.Opts.Delete || r.Opts.Force) {
		r.fail(e.Path, errors.New("cannot replace a directory with a symlink (use --force or --delete)"))
		return
	}
	switch {
	case r.Opts.Itemize > 0 && exists && st.Mode()&fs.ModeSymlink != 0:
		r.UI.Info("cLc........ %s -> %s", e.Path, e.Link)
	case r.Opts.Itemize > 0:
		r.itemize("cL", e, nil, false)
	case r.Opts.Verbose > 0:
		r.UI.Info("%s -> %s", e.Path, e.Link)
	}
	if r.Opts.DryRun {
		return
	}
	if exists && !st.IsDir() && r.Opts.Backup {
		if err := r.backup(e.Path, target); err != nil {
			r.fail(e.Path, err)
			return
		}
	}
	tmp := tempName(target, ".xt-tmp")
	os.Remove(tmp)
	if err := os.Symlink(e.Link, tmp); err != nil {
		r.fail(e.Path, fmt.Errorf("creating symlink: %w", err))
		return
	}
	if exists && st.IsDir() {
		os.RemoveAll(target)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		r.fail(e.Path, err)
		return
	}
	r.chown(target, e)
	r.markSynced(int32(i))
}

func (r *Receiver) chown(p string, e *proto.FileEntry) {
	if runtime.GOOS == "windows" || !e.HasOwner || !(r.Opts.Owner || r.Opts.Group) {
		return
	}
	uid, gid := -1, -1
	if r.Opts.Owner && isRoot() {
		uid = int(e.UID)
	}
	if r.Opts.Group {
		gid = int(e.GID)
	}
	if uid == -1 && gid == -1 {
		return
	}
	if err := os.Lchown(p, uid, gid); err != nil && isRoot() {
		r.fail(e.Path, err)
	}
}

// fixAttrs updates metadata of a file whose contents are already current.
func (r *Receiver) fixAttrs(e *proto.FileEntry, target string, st fs.FileInfo) {
	if r.Opts.Times && !r.sameTime(st.ModTime(), e.MTime) {
		if err := os.Chtimes(target, time.Time{}, time.Unix(0, e.MTime)); err != nil {
			r.fail(e.Path, err)
		}
	}
	if r.Opts.Owner || r.Opts.Group {
		if uid, gid, ok := ownerOf(st); !ok || uid != e.UID || gid != e.GID {
			r.chown(target, e)
		}
	}
	if r.permsMeaningful() && unixPerm(st.Mode()) != e.Mode {
		if err := os.Chmod(target, fileMode(e.Mode)); err != nil {
			r.fail(e.Path, err)
		}
	} else if r.executability() {
		if m := withExec(unixPerm(st.Mode()), e.Mode); m != unixPerm(st.Mode()) {
			if err := os.Chmod(target, fileMode(m)); err != nil {
				r.fail(e.Path, err)
			}
		}
	}
}

// executability reports whether -E applies: it only matters without -p.
func (r *Receiver) executability() bool {
	return r.Opts.Executability && !r.permsMeaningful() && r.PeerOS != "windows" && runtime.GOOS != "windows"
}

// withExec makes mode executable (where readable) if src is executable,
// and not executable otherwise.
func withExec(mode, src uint32) uint32 {
	if src&0111 == 0 {
		return mode &^ 0111
	}
	return mode | (mode&0444)>>2
}

func (r *Receiver) finalMode(e *proto.FileEntry, existing fs.FileInfo) fs.FileMode {
	if r.permsMeaningful() {
		return fileMode(e.Mode)
	}
	// Like rsync, new files get only the source's rwx bits, never
	// setuid, setgid or sticky.
	mode := e.Mode & 0o777 &^ umask
	if existing != nil {
		mode = unixPerm(existing.Mode())
	}
	if r.executability() {
		mode = withExec(mode, e.Mode)
	}
	return fileMode(mode)
}

// request sends a Request for file idx, queueing the job first so the
// receive loop always finds it.
func (r *Receiver) request(idx int32, redo bool) error {
	e := &r.files[idx]
	if r.Opts.DryRun {
		if r.Track && r.Opts.Verbose > 0 && r.Opts.Itemize == 0 {
			r.UI.Info("%s", e.Path)
		}
		return r.send(&proto.Msg{Request: &proto.Request{Index: idx, DryRun: true}})
	}
	target := r.pathFor(e)
	j := &job{idx: idx, target: target, partial: tempName(target, partialSuffix), redo: redo}
	if st, err := os.Lstat(target); err == nil && st.Mode().IsRegular() {
		j.existing = st
	}
	if redo {
		os.Remove(j.partial)
	} else {
		j.resumeLen, j.resumeHash = r.resumeInfo(j.partial, e.Size)
	}
	if !r.Opts.WholeFile && !redo && j.existing != nil && j.existing.Size() > 0 {
		sig, err := signature(target, j.existing.Size())
		if err != nil {
			r.UI.Error("%s: reading basis: %v (sending whole file)", e.Path, err)
		}
		j.sig = sig
	}
	req := &proto.Request{Index: idx, Sig: j.sig, ResumeLen: j.resumeLen}
	if j.resumeHash != nil {
		req.ResumeSum = j.resumeHash.Sum(nil)
	}
	r.jobs <- j
	return r.send(&proto.Msg{Request: req})
}

func (r *Receiver) send(m *proto.Msg) error {
	if err := r.Conn.Send(m); err != nil {
		return connError{err}
	}
	return nil
}

func signature(p string, size int64) (*delta.Signature, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return delta.ComputeSignature(bufio.NewReaderSize(f, 256<<10), delta.BlockSizeFor(size))
}

// resumeInfo hashes an existing partial file so the sender can verify it
// and continue from where it stopped.
func (r *Receiver) resumeInfo(partial string, size int64) (int64, hash.Hash) {
	st, err := os.Lstat(partial)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > size {
		return 0, nil
	}
	f, err := os.Open(partial)
	if err != nil {
		return 0, nil
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil || n == 0 {
		return 0, nil
	}
	return n, h
}

// active is the file currently being received.
type active struct {
	j     *job
	e     *proto.FileEntry
	f     *os.File
	sw    *sparseWriter
	bw    *bufio.Writer
	h     hash.Hash
	basis *os.File
	ap    *delta.Applier
	st    proto.Stats
	err   error
}

func (r *Receiver) receive() error {
	var cur *active
	var redo []int32
	phases := 0
	defer func() {
		if cur != nil {
			r.abort(cur)
		}
	}()
	for {
		m, err := r.Conn.Recv()
		if err != nil {
			return connError{err}
		}
		switch {
		case m.Begin != nil:
			if cur != nil {
				return errors.New("protocol error: nested file")
			}
			var j *job
			select {
			case j = <-r.jobs:
			default:
				return errors.New("protocol error: data for a file that was not requested")
			}
			if j.idx != m.Begin.Index {
				return fmt.Errorf("protocol error: got file %d, expected %d", m.Begin.Index, j.idx)
			}
			cur = r.begin(j, m.Begin.ResumeFrom)
		case m.Ops != nil:
			if cur == nil {
				return errors.New("protocol error: data outside a file")
			}
			r.apply(cur, m.Ops)
		case m.End != nil:
			if cur == nil {
				return errors.New("protocol error: end outside a file")
			}
			if r.finish(cur, m.End) {
				redo = append(redo, cur.j.idx)
			}
			cur = nil
		case m.PhaseEnd:
			phases++
			if phases == 1 {
				r.redo <- redo
				break
			}
			<-r.genDone
			r.final = true
			for _, d := range r.deletes {
				r.deleteExtraneous(d.rel, d.dir)
			}
			r.linkAll()
			if !r.Opts.DryRun {
				r.finalizeDirs()
			}
			r.syncMu.Lock()
			synced := r.synced
			r.syncMu.Unlock()
			// gob drops an empty slice, which would arrive as a message
			// with no field set.
			if len(synced) > 0 {
				if err := r.Conn.SendAsync(&proto.Msg{Synced: synced}); err != nil {
					return connError{err}
				}
			}
			r.Stats.Errors = r.errs.Load()
			if err := r.Conn.SendAsync(&proto.Msg{Done: &r.Stats}); err != nil {
				return connError{err}
			}
		case m.Done != nil:
			if phases < 2 {
				return errors.New("protocol error: early done")
			}
			r.PeerStats = m.Done
			return nil
		case m.Log != nil:
			r.UI.Relay(m.Log)
		case m.Error != "":
			return RemoteError(m.Error)
		default:
			return errors.New("protocol error: unexpected message from sender")
		}
	}
}

func (r *Receiver) begin(j *job, resumeFrom int64) *active {
	a := &active{j: j, e: &r.files[j.idx]}
	var err error
	if resumeFrom > 0 {
		if resumeFrom != j.resumeLen || j.resumeHash == nil {
			a.err = errors.New("protocol error: unexpected resume offset")
			return a
		}
		a.f, err = os.OpenFile(j.partial, os.O_WRONLY|oNoFollow, 0)
		if err == nil {
			err = sameRegular(a.f, j.partial)
		}
		if err == nil {
			err = a.f.Truncate(resumeFrom)
		}
		if err == nil {
			_, err = a.f.Seek(resumeFrom, io.SeekStart)
		}
		a.h = j.resumeHash
		if r.Opts.Verbose > 1 {
			r.UI.Info("%s: resuming at %s", a.e.Path, humanBytes(resumeFrom))
		}
	} else {
		// Never write through whatever is at the partial name: the file
		// list may have placed a symlink there to redirect the write
		// outside the destination. O_EXCL refuses to follow symlinks.
		if err = os.Remove(j.partial); err == nil || errors.Is(err, fs.ErrNotExist) {
			a.f, err = os.OpenFile(j.partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		}
		a.h = sha256.New()
	}
	if err != nil {
		a.err = err
		return a
	}
	var out io.Writer = a.f
	if r.Opts.Sparse {
		a.sw = &sparseWriter{f: a.f}
		out = a.sw
	}
	a.bw = bufio.NewWriterSize(out, 256<<10)
	w := io.MultiWriter(a.bw, a.h)
	if r.Track {
		r.UI.FileStart(a.e.Path, a.e.Size, resumeFrom)
		w = io.MultiWriter(w, r.UI)
	}
	if j.sig != nil {
		if a.basis, err = os.Open(j.target); err != nil {
			a.err = err
			return a
		}
	}
	var basis io.ReaderAt
	if a.basis != nil {
		basis = a.basis
	}
	a.ap = delta.NewApplier(basis, j.sig, w)
	return a
}

// sameRegular checks that f, opened from p, is the regular file at p and
// not something a symlink led to.
func sameRegular(f *os.File, p string) error {
	fst, err := f.Stat()
	if err != nil {
		return err
	}
	lst, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fst.Mode().IsRegular() || !os.SameFile(fst, lst) {
		return fmt.Errorf("%s is not a regular file", filepath.Base(p))
	}
	return nil
}

func (r *Receiver) apply(a *active, ops []delta.Op) {
	if a.err != nil {
		return
	}
	for _, op := range ops {
		if err := a.ap.Apply(op); err != nil {
			a.err = err
			return
		}
		if op.Kind == delta.OpLiteral {
			a.st.Literal += int64(len(op.Data))
		} else if sig := a.j.sig; sig != nil {
			start := int64(op.Block) * int64(sig.BlockSize)
			end := min((int64(op.Block)+int64(op.Count))*int64(sig.BlockSize), sig.BasisSize)
			a.st.Matched += end - start
		}
	}
}

func (a *active) close() error {
	var err error
	if a.bw != nil {
		err = a.bw.Flush()
	}
	if a.sw != nil && err == nil {
		err = a.sw.finish()
	}
	if a.f != nil {
		if cerr := a.f.Close(); err == nil {
			err = cerr
		}
	}
	if a.basis != nil {
		a.basis.Close()
	}
	return err
}

// abort handles a connection lost mid-file. With --partial the data received
// so far is kept for the next run to resume from.
func (r *Receiver) abort(a *active) {
	a.close()
	if r.Track {
		r.UI.FileEnd(a.st, false)
	}
	r.dropPartial(a)
}

func (r *Receiver) dropPartial(a *active) {
	if !r.Opts.Partial {
		os.Remove(a.j.partial)
	} else if st, err := os.Stat(a.j.partial); err == nil && st.Size() == 0 {
		os.Remove(a.j.partial)
	}
}

// finish completes a file and reports whether it should be retried.
func (r *Receiver) finish(a *active, end *proto.End) (retry bool) {
	cerr := a.close()
	ok := false
	if r.Track {
		defer func() { r.UI.FileEnd(a.st, ok) }()
	}
	switch {
	case end.Err != "":
		// The sender already reported the error.
		r.errs.Add(1)
		r.dropPartial(a)
		return false
	case a.err != nil || cerr != nil:
		r.fail(a.e.Path, errors.Join(a.err, cerr))
		r.dropPartial(a)
		return false
	case !bytes.Equal(a.h.Sum(nil), end.Sum):
		os.Remove(a.j.partial)
		if a.j.redo {
			r.fail(a.e.Path, errors.New("checksum mismatch after retry"))
			return false
		}
		if r.Opts.Verbose > 0 {
			r.UI.Info("%s: checksum mismatch, retrying", a.e.Path)
		}
		return true
	}
	e, p := a.e, a.j.partial
	if r.Opts.Times {
		if err := os.Chtimes(p, time.Time{}, time.Unix(0, e.MTime)); err != nil {
			r.fail(e.Path, err)
		}
	}
	r.chown(p, e)
	if err := os.Chmod(p, r.finalMode(e, a.j.existing)); err != nil {
		r.fail(e.Path, err)
	}
	if st, err := os.Lstat(a.j.target); err == nil && st.IsDir() {
		if err := os.RemoveAll(a.j.target); err != nil {
			r.fail(e.Path, err)
			return false
		}
	} else if err == nil && r.Opts.Backup {
		if err := r.backup(e.Path, a.j.target); err != nil {
			r.fail(e.Path, err)
			r.dropPartial(a)
			return false
		}
	}
	if err := rename(p, a.j.target); err != nil {
		r.fail(e.Path, err)
		r.dropPartial(a)
		return false
	}
	r.markSynced(a.j.idx)
	ok = true
	return false
}

// sparseWriter turns runs of zeros into holes (--sparse).
type sparseWriter struct {
	f    *os.File
	hole int64 // zero bytes skipped but not yet seeked over
}

const sparseBlock = 4096

var zeroBlock [sparseBlock]byte

func (w *sparseWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		// Collect a run of non-zero blocks and write it in one call.
		end := 0
		for end < len(p) {
			k := min(sparseBlock, len(p)-end)
			if bytes.Equal(p[end:end+k], zeroBlock[:k]) {
				break
			}
			end += k
		}
		if end > 0 {
			if err := w.seek(); err != nil {
				return 0, err
			}
			if _, err := w.f.Write(p[:end]); err != nil {
				return 0, err
			}
			p = p[end:]
			continue
		}
		k := min(sparseBlock, len(p))
		w.hole += int64(k)
		p = p[k:]
	}
	return n, nil
}

func (w *sparseWriter) seek() error {
	if w.hole == 0 {
		return nil
	}
	_, err := w.f.Seek(w.hole, io.SeekCurrent)
	w.hole = 0
	return err
}

// finish extends the file over a trailing hole.
func (w *sparseWriter) finish() error {
	if w.hole == 0 {
		return nil
	}
	off, err := w.f.Seek(w.hole, io.SeekCurrent)
	w.hole = 0
	if err != nil {
		return err
	}
	return w.f.Truncate(off)
}

// rename replaces dst with src. On Windows a read-only destination must be
// made writable first.
func rename(src, dst string) error {
	err := os.Rename(src, dst)
	if err != nil && runtime.GOOS == "windows" {
		if st, serr := os.Lstat(dst); serr == nil && st.Mode().Perm()&0o200 == 0 {
			os.Chmod(dst, 0o666)
			err = os.Rename(src, dst)
		}
	}
	return err
}

// linkAll creates the hard links deferred by prepare (-H), now that every
// leader file is in place.
func (r *Receiver) linkAll() {
	for _, i := range r.links {
		e := &r.files[i]
		lead := &r.files[e.HardLink-1]
		target, leadPath := r.pathFor(e), r.pathFor(lead)
		lst, err := os.Lstat(leadPath)
		if err != nil || !lst.Mode().IsRegular() {
			if !r.Opts.DryRun {
				r.fail(e.Path, fmt.Errorf("hard link target %s is missing", lead.Path))
			}
			continue
		}
		st, err := os.Lstat(target)
		if err == nil && os.SameFile(st, lst) {
			r.markSynced(int32(i))
			continue
		}
		switch {
		case r.Opts.Itemize > 0:
			r.UI.Info("hf+++++++++ %s => %s", e.Path, lead.Path)
		case r.Opts.Verbose > 0:
			r.UI.Info("%s => %s", e.Path, lead.Path)
		}
		if r.Opts.DryRun {
			continue
		}
		if err == nil {
			if st.IsDir() && !(r.Opts.Delete || r.Opts.Force) {
				r.fail(e.Path, errors.New("cannot replace a directory with a file (use --force or --delete)"))
				continue
			}
			if !st.IsDir() && r.Opts.Backup {
				if err := r.backup(e.Path, target); err != nil {
					r.fail(e.Path, err)
					continue
				}
			} else if err := os.RemoveAll(target); err != nil {
				r.fail(e.Path, err)
				continue
			}
		}
		if err := os.Link(leadPath, target); err != nil {
			r.fail(e.Path, err)
			continue
		}
		r.markSynced(int32(i))
	}
}

func (r *Receiver) finalizeDirs() {
	for i := len(r.dirFixes) - 1; i >= 0; i-- {
		d := r.dirFixes[i]
		e := &r.files[d.idx]
		if r.Opts.Times && !r.Opts.OmitDirTimes {
			if err := os.Chtimes(d.path, time.Time{}, time.Unix(0, e.MTime)); err != nil {
				r.fail(e.Path, err)
			}
		}
		r.chown(d.path, e)
		var mode fs.FileMode
		switch {
		case r.permsMeaningful():
			mode = fileMode(e.Mode)
		case d.created:
			mode = fileMode(e.Mode & 0o777 &^ umask)
		default:
			continue
		}
		if runtime.GOOS == "windows" {
			continue
		}
		if err := os.Chmod(d.path, mode); err != nil {
			r.fail(e.Path, err)
		}
	}
}
