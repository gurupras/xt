// Package proto defines the messages exchanged between two xt processes and
// the connection that carries them.
//
// After a one-line text handshake in each direction, both sides exchange gob
// encoded Msg values, optionally through a deflate stream.
//
// Message flow (sender = side reading files, receiver = side writing them):
//
//	client   -> server   Start
//	sender   -> receiver Files... FilesEnd
//	receiver -> sender   Plan, Request..., PhaseEnd, Request (redo)..., PhaseEnd, Done
//	sender   -> receiver (Begin Ops... End)..., PhaseEnd, (redo)..., PhaseEnd, Done
//
// With --remove-source-files the receiver sends Synced just before its Done.
//
// Either side may send Log at any time; Error aborts the session.
package proto

import (
	"bufio"
	"compress/flate"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gurupras/xt/internal/delta"
	"github.com/gurupras/xt/internal/opt"
)

// Magic starts each handshake line.
const Magic = "XTPROTO"

// Version is the protocol version; both sides must match.
const Version = 1

// FileType identifies the kind of a file list entry.
type FileType uint8

const (
	TypeFile FileType = iota
	TypeDir
	TypeSymlink
)

// FileEntry describes one file in the transfer.
type FileEntry struct {
	Path     string // slash-separated, relative to the transfer root; "." is the root dir
	Type     FileType
	Mode     uint32 // unix permission bits (07777)
	Size     int64
	MTime    int64 // unix nanoseconds
	UID, GID uint32
	HasOwner bool
	Link     string // symlink target
	Sum      []byte // whole-file hash, only with --checksum
	HardLink int32  // with --hard-links: 1 + index of an earlier entry this file is linked to
}

// Start tells the server which role to play.
type Start struct {
	Opts  opt.Options
	Role  string // "sender" or "receiver"
	Paths []string
}

// Plan tells the sender how much work the receiver has scheduled, so the
// client can show overall progress.
type Plan struct {
	Files int64
	Bytes int64
}

// Request asks the sender for one file.
type Request struct {
	Index     int32
	Sig       *delta.Signature // basis signature; nil means send the whole file
	ResumeLen int64            // bytes already present in the partial file
	ResumeSum []byte           // hash of those bytes
	DryRun    bool             // report only; no data is sent back
}

// Begin starts the data for a requested file.
type Begin struct {
	Index      int32
	ResumeFrom int64 // 0 if the partial file was rejected
}

// End finishes a file.
type End struct {
	Sum []byte // hash of the complete new file
	Err string // non-empty if the sender failed to read the file
}

// Log carries a message to be printed by the client.
type Log struct {
	Text string
	Err  bool
}

// Stats are per-side transfer counters.
type Stats struct {
	NumFiles, NumDirs, NumLinks, NumRegular int64
	TotalSize                               int64
	Transferred, TransferredSize            int64
	Literal, Matched                        int64
	Deleted                                 int64
	DeleteSkipped                           int64 // deletions withheld by --max-delete
	Errors                                  int64
}

// Msg is the wire envelope; exactly one field is set.
type Msg struct {
	Start    *Start
	Files    []FileEntry
	FilesEnd bool
	IOError  bool // with FilesEnd: some files could not be listed
	Plan     *Plan
	Request  *Request
	Begin    *Begin
	Ops      []delta.Op
	End      *End
	PhaseEnd bool
	Log      *Log
	Synced   []int32 // files now current on the receiver (--remove-source-files)
	Done     *Stats
	Error    string

	// Keepalive is sent periodically with --timeout; Recv drops it.
	Keepalive bool
}

// Hello is the parsed handshake line.
type Hello struct {
	Version       int
	OS            string
	CompressLevel int
}

// WriteHello sends the handshake line.
func WriteHello(w io.Writer, compressLevel int) error {
	_, err := fmt.Fprintf(w, "%s %d %s %d\n", Magic, Version, runtime.GOOS, compressLevel)
	return err
}

// ReadHello reads the peer's handshake line. Lines before it (for example
// output from a noisy remote shell profile) are passed to junk; if junk is
// nil they are an error.
func ReadHello(br *bufio.Reader, junk func(string)) (*Hello, error) {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if line != "" && junk != nil {
				junk(line)
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		f := strings.Fields(line)
		if len(f) == 4 && f[0] == Magic {
			v, err1 := strconv.Atoi(f[1])
			lvl, err2 := strconv.Atoi(f[3])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("malformed handshake %q", line)
			}
			return &Hello{Version: v, OS: f[2], CompressLevel: lvl}, nil
		}
		if junk == nil {
			return nil, fmt.Errorf("unexpected data before handshake: %q", line)
		}
		junk(line)
	}
}

// Counter counts bytes passing through a reader or writer.
type Counter struct{ n atomic.Int64 }

func (c *Counter) N() int64 { return c.n.Load() }

type countReader struct {
	r io.Reader
	c *Counter
}

func (r countReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.c.n.Add(int64(n))
	return n, err
}

type countWriter struct {
	w io.Writer
	c *Counter
}

func (w countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.c.n.Add(int64(n))
	return n, err
}

// CountReader wraps r so bytes read are added to c.
func CountReader(r io.Reader, c *Counter) io.Reader { return countReader{r, c} }

// CountWriter wraps w so bytes written are added to c.
func CountWriter(w io.Writer, c *Counter) io.Writer { return countWriter{w, c} }

// ErrClosed is returned by Send after Close.
var ErrClosed = errors.New("connection closed")

// maxQueued bounds the number of blocking messages waiting to be written.
const maxQueued = 64

type queued struct {
	m     *Msg
	async bool
}

// Conn is a bidirectional message stream. Send may be called from multiple
// goroutines; Recv from one.
type Conn struct {
	dec *gob.Decoder

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []queued
	blocking int
	closed   bool
	err      error
	done     chan struct{}
}

// NewConn creates a Conn over r and w. If level > 0 both directions are
// deflate-compressed at that level. closer, if non-nil, is closed after the
// last message is written.
func NewConn(r io.Reader, w io.Writer, closer io.Closer, level int) *Conn {
	c := &Conn{done: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	bw := bufio.NewWriterSize(w, 64<<10)
	var enc *gob.Encoder
	var fw *flate.Writer
	if level > 0 {
		fw, _ = flate.NewWriter(bw, level)
		enc = gob.NewEncoder(fw)
		r = flate.NewReader(r)
	} else {
		enc = gob.NewEncoder(bw)
	}
	c.dec = gob.NewDecoder(r)
	flush := func() error {
		if fw != nil {
			if err := fw.Flush(); err != nil {
				return err
			}
		}
		return bw.Flush()
	}
	go c.writer(enc, flush, closer)
	return c
}

func (c *Conn) writer(enc *gob.Encoder, flush func() error, closer io.Closer) {
	defer close(c.done)
	var werr error
	for {
		c.mu.Lock()
		for len(c.queue) == 0 && !c.closed {
			c.cond.Wait()
		}
		if len(c.queue) == 0 {
			c.mu.Unlock()
			break
		}
		item := c.queue[0]
		c.queue[0] = queued{}
		c.queue = c.queue[1:]
		if !item.async {
			c.blocking--
		}
		empty := len(c.queue) == 0
		c.cond.Broadcast()
		c.mu.Unlock()

		if werr != nil {
			continue
		}
		werr = enc.Encode(item.m)
		if werr == nil && empty {
			werr = flush()
		}
		if werr != nil {
			c.mu.Lock()
			c.err = werr
			c.cond.Broadcast()
			c.mu.Unlock()
		}
	}
	if werr == nil {
		werr = flush()
	}
	if closer != nil {
		closer.Close()
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = werr
	}
	c.mu.Unlock()
}

func (c *Conn) enqueue(m *Msg, async bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for !async && c.blocking >= maxQueued && c.err == nil && !c.closed {
		c.cond.Wait()
	}
	if c.err != nil {
		return c.err
	}
	if c.closed {
		return ErrClosed
	}
	c.queue = append(c.queue, queued{m, async})
	if !async {
		c.blocking++
	}
	c.cond.Broadcast()
	return nil
}

// Send queues m for writing, blocking while too many messages are pending.
// m must not be modified afterwards.
func (c *Conn) Send(m *Msg) error { return c.enqueue(m, false) }

// SendAsync queues m without ever blocking. Use it from goroutines that must
// keep reading to avoid deadlock.
func (c *Conn) SendAsync(m *Msg) error { return c.enqueue(m, true) }

// Recv reads the next message, skipping keepalives.
func (c *Conn) Recv() (*Msg, error) {
	for {
		m := new(Msg)
		if err := c.dec.Decode(m); err != nil {
			return nil, err
		}
		if !m.Keepalive {
			return m, nil
		}
	}
}

// Close flushes pending messages, closes the write side and waits.
func (c *Conn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done is closed once the writer has finished after Close.
func (c *Conn) Done() <-chan struct{} { return c.done }
