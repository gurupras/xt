package xfer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
)

// Exit codes, following rsync where there is an equivalent.
const (
	ExitOK          = 0
	ExitUsage       = 1
	ExitProtocol    = 12
	ExitInterrupted = 20
	ExitPartial     = 23
	ExitMaxDelete   = 25
	ExitTimeout     = 30
)

// Location is a parsed SRC or DEST argument.
type Location struct {
	Host string // empty for local paths; may include user@
	Path string
}

// ParseLocation splits [user@]host:path. A colon after a slash, or a single
// drive letter on Windows, means the argument is a local path.
func ParseLocation(s string) (Location, error) {
	if strings.HasPrefix(s, "rsync://") {
		return Location{}, errors.New("rsync:// URLs are not supported; use host:path")
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return Location{Path: s}, nil
	}
	if open := strings.IndexByte(s, '['); open >= 0 && open < colon {
		// [ipv6]:path or user@[ipv6]:path
		if end := strings.Index(s, "]:"); end > open {
			return Location{Host: s[:open] + s[open+1:end], Path: s[end+2:]}, nil
		}
	}
	prefix := s[:colon]
	if prefix == "" || strings.ContainsAny(prefix, `/\`) {
		return Location{Path: s}, nil
	}
	if runtime.GOOS == "windows" && len(prefix) == 1 {
		return Location{Path: s}, nil
	}
	if strings.HasPrefix(s[colon+1:], ":") {
		return Location{}, errors.New("rsync daemon syntax (host::module) is not supported; use host:path")
	}
	return Location{Host: prefix, Path: s[colon+1:]}, nil
}

// splitArgs splits a command line on whitespace, honouring quotes.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	in := false
	var quote rune
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote, in = c, true
		case c == ' ' || c == '\t':
			if in {
				args = append(args, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	if in {
		args = append(args, cur.String())
	}
	return args
}

type client struct {
	o  *opt.Options
	ui *UI

	mu          sync.Mutex
	abortFn     func()
	interrupted atomic.Bool
	timedOut    atomic.Bool

	sent, recvd proto.Counter
	remote      bool
	senderSt    *proto.Stats
	recvSt      *proto.Stats
}

func (c *client) setAbort(f func()) {
	c.mu.Lock()
	c.abortFn = f
	c.mu.Unlock()
}

func (c *client) abort() {
	c.mu.Lock()
	f := c.abortFn
	c.mu.Unlock()
	if f != nil {
		f()
	}
}

type usageError struct{ error }

// RunClient runs a transfer described by o and returns the exit code.
func RunClient(o *opt.Options) int {
	c := &client{o: o, ui: NewClientUI(o)}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			if c.interrupted.Swap(true) {
				os.Exit(ExitInterrupted)
			}
			c.abort()
		}
	}()

	start := time.Now()
	err := c.run()
	c.ui.Close()

	if c.timedOut.Load() {
		fmt.Fprintf(os.Stderr, "xt: timeout: no data received for %d seconds\n", o.Timeout)
		return ExitTimeout
	}
	if c.interrupted.Load() {
		fmt.Fprintln(os.Stderr, "xt: interrupted")
		if o.Partial {
			fmt.Fprintln(os.Stderr, "xt: partial files were kept; run the same command again to resume")
		} else {
			fmt.Fprintln(os.Stderr, "xt: hint: use --partial (or -P) so an interrupted transfer can be resumed")
		}
		return ExitInterrupted
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(os.Stderr, "xt: %v\n", err)
		return ExitUsage
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "xt: %v\n", err)
		return ExitProtocol
	}
	if !o.ListOnly {
		c.report(time.Since(start))
	}
	if c.errors() > 0 {
		fmt.Fprintln(os.Stderr, "xt: some files could not be transferred (see errors above)")
		return ExitPartial
	}
	if c.recvSt != nil && c.recvSt.DeleteSkipped > 0 {
		fmt.Fprintf(os.Stderr, "xt: deletions stopped due to --max-delete limit (%s not deleted)\n", commas(c.recvSt.DeleteSkipped))
		return ExitMaxDelete
	}
	return ExitOK
}

// run parses the locations and performs the transfer.
func (c *client) run() error {
	args := c.o.Args
	if len(args) == 0 {
		return usageError{errors.New("need at least one SRC and a DEST (see xt --help)")}
	}
	// With a single argument, like rsync, list the source instead.
	// --list-only ignores any destination.
	if len(args) == 1 || c.o.ListOnly {
		if len(args) > 1 {
			args = args[:len(args)-1]
		}
		c.o.ListOnly = true
		if !c.o.Recursive {
			c.o.Dirs = true
		}
		args = append(args, "")
	}
	dst, err := ParseLocation(args[len(args)-1])
	if err != nil {
		return usageError{err}
	}
	var host string
	var srcPaths []string
	for i, s := range args[:len(args)-1] {
		l, err := ParseLocation(s)
		if err != nil {
			return usageError{err}
		}
		if i == 0 {
			host = l.Host
		} else if l.Host != host {
			return usageError{errors.New("all sources must be on the same host")}
		}
		srcPaths = append(srcPaths, l.Path)
	}
	switch {
	case host != "" && dst.Host != "":
		return usageError{errors.New("copying between two remote hosts is not supported")}
	case c.o.ListOnly && host != "":
		return c.pull(host, srcPaths, "")
	case c.o.ListOnly:
		return c.local(srcPaths, "")
	case host == "" && dst.Host == "":
		return c.local(srcPaths, dst.Path)
	case dst.Host != "":
		return c.push(dst.Host, srcPaths, dst.Path)
	default:
		return c.pull(host, srcPaths, dst.Path)
	}
}

func (c *client) errors() int64 {
	var n int64
	if c.senderSt != nil {
		n += c.senderSt.Errors
	}
	if c.recvSt != nil {
		n += c.recvSt.Errors
	}
	return n
}

// firstError prefers a root cause over the "connection lost" it triggers on
// the other side.
func firstError(errs ...error) error {
	var fallback error
	for _, err := range errs {
		if err == nil {
			continue
		}
		var ce connError
		if !errors.As(err, &ce) {
			return err
		}
		if fallback == nil {
			fallback = err
		}
	}
	return fallback
}

func (c *client) local(srcs []string, dest string) error {
	if c.o.WholeFileMode == 0 {
		c.o.WholeFile = true
	}
	sr, sw := io.Pipe() // sender -> receiver
	rr, rw := io.Pipe() // receiver -> sender
	limited := &rateWriter{w: sw}
	limited.rate.Store(c.o.BwLimit)
	sconn := proto.NewConn(rr, proto.CountWriter(limited, &c.sent), sw, 0)
	rconn := proto.NewConn(sr, proto.CountWriter(rw, &c.recvd), rw, 0)
	abort := func() {
		err := errors.New("aborted")
		sr.CloseWithError(err)
		sw.CloseWithError(err)
		rr.CloseWithError(err)
		rw.CloseWithError(err)
	}
	c.setAbort(abort)
	snd := &Sender{Conn: sconn, Opts: c.o, UI: c.ui}
	rcv := &Receiver{Conn: rconn, Opts: c.o, UI: c.ui, Dest: dest, PeerOS: runtime.GOOS, Track: true}
	serrc := make(chan error, 1)
	go func() {
		err := snd.Run(srcs)
		if err != nil {
			abort()
		}
		sconn.Close()
		serrc <- err
	}()
	rerr := rcv.Run()
	if rerr != nil {
		abort()
	}
	rconn.Close()
	serr := <-serrc
	c.senderSt, c.recvSt = &snd.Stats, &rcv.Stats
	return firstError(rerr, serr)
}

type remote struct {
	cmd   *exec.Cmd
	conn  *proto.Conn
	hello *proto.Hello
	once  sync.Once
}

func (r *remote) kill() {
	r.once.Do(func() {
		if r.cmd.Process != nil {
			r.cmd.Process.Kill()
		}
	})
}

// dial starts "xt --server" on host through the remote shell.
func (c *client) dial(host string) (*remote, error) {
	rsh := c.o.Rsh
	if rsh == "" {
		rsh = os.Getenv("XT_RSH")
	}
	if rsh == "" {
		rsh = "ssh"
	}
	argv := splitArgs(rsh)
	if len(argv) == 0 {
		return nil, errors.New("empty remote shell command")
	}
	if v := c.o.IPVersion; v != 0 && strings.TrimSuffix(filepath.Base(argv[0]), ".exe") == "ssh" {
		argv = append([]string{argv[0], "-" + string(v)}, argv[1:]...)
	}
	argv = append(argv, host, c.o.XtPath, "--server")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	r := &remote{cmd: cmd}
	c.setAbort(r.kill)
	c.remote = true

	level := 0
	if c.o.Compress {
		level = c.o.CompressLevel
	}
	limited := &rateWriter{w: stdin}
	limited.rate.Store(c.o.BwLimit)
	w := proto.CountWriter(limited, &c.sent)
	if err := proto.WriteHello(w, level); err != nil {
		r.kill()
		cmd.Wait()
		return nil, c.startError(host, err)
	}
	br := bufio.NewReaderSize(proto.CountReader(stdout, &c.recvd), 64<<10)
	var junk []string
	hello, err := proto.ReadHello(br, func(line string) {
		junk = append(junk, line)
		if c.o.Verbose > 1 {
			c.ui.Info("remote: %s", line)
		}
	})
	if err != nil {
		r.kill()
		cmd.Wait()
		if len(junk) > 0 {
			err = fmt.Errorf("%w; remote printed:\n  %s", err, strings.Join(junk, "\n  "))
		}
		return nil, c.startError(host, err)
	}
	if hello.Version != proto.Version {
		r.kill()
		cmd.Wait()
		return nil, fmt.Errorf("protocol mismatch: local xt speaks version %d, remote speaks %d; install the same xt version on both hosts", proto.Version, hello.Version)
	}
	r.hello = hello
	r.conn = proto.NewConn(br, w, stdin, level)
	return r, nil
}

func (c *client) startError(host string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) {
		err = errors.New("connection closed before the handshake")
	}
	return fmt.Errorf("could not start xt on %s: %v\n(is xt installed and on the remote PATH? use --xt-path to give its location)", host, err)
}

// finish shuts the connection down and waits for the remote shell.
func (r *remote) finish(roleErr error) error {
	if roleErr != nil {
		var ce connError
		var re RemoteError
		if !errors.As(roleErr, &ce) && !errors.As(roleErr, &re) {
			r.conn.SendAsync(&proto.Msg{Error: roleErr.Error()})
		}
		go func() {
			time.Sleep(2 * time.Second)
			r.kill()
		}()
		r.conn.Close()
		r.kill()
		r.cmd.Wait()
		return roleErr
	}
	r.conn.Close()
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("remote shell: %w", err)
		}
	case <-time.After(30 * time.Second):
		r.kill()
		<-done
	}
	return nil
}

func (c *client) push(host string, srcs []string, dest string) error {
	r, err := c.dial(host)
	if err != nil {
		return err
	}
	if err := r.conn.Send(&proto.Msg{Start: &proto.Start{Opts: *c.o, Role: "receiver", Paths: []string{dest}}}); err != nil {
		return r.finish(connError{err})
	}
	defer c.watch(r)()
	snd := &Sender{Conn: r.conn, Opts: c.o, UI: c.ui, Track: true}
	err = snd.Run(srcs)
	c.senderSt, c.recvSt = &snd.Stats, snd.PeerStats
	return r.finish(err)
}

func (c *client) pull(host string, srcs []string, dest string) error {
	r, err := c.dial(host)
	if err != nil {
		return err
	}
	if err := r.conn.Send(&proto.Msg{Start: &proto.Start{Opts: *c.o, Role: "sender", Paths: srcs}}); err != nil {
		return r.finish(connError{err})
	}
	defer c.watch(r)()
	rcv := &Receiver{Conn: r.conn, Opts: c.o, UI: c.ui, Dest: dest, PeerOS: r.hello.OS, Track: true, ListProgress: true}
	err = rcv.Run()
	c.senderSt, c.recvSt = rcv.PeerStats, &rcv.Stats
	return r.finish(err)
}

// watch starts the --timeout watchdog for a remote session.
func (c *client) watch(r *remote) (stop func()) {
	if c.o.Timeout <= 0 {
		return func() {}
	}
	return watchdog(r.conn, &c.recvd, time.Duration(c.o.Timeout)*time.Second, func() {
		c.timedOut.Store(true)
		r.kill()
	})
}

func (c *client) report(elapsed time.Duration) {
	o := c.o
	if o.Quiet || c.senderSt == nil || c.recvSt == nil {
		return
	}
	s, r := c.senderSt, c.recvSt
	num := commas
	if o.Human {
		num = func(n int64) string { return humanBytes(n) }
	}
	if o.Stats {
		fmt.Printf("\nNumber of files: %s (reg: %s, dir: %s, link: %s)\n",
			commas(s.NumFiles), commas(s.NumRegular), commas(s.NumDirs), commas(s.NumLinks))
		fmt.Printf("Number of deleted files: %s\n", commas(r.Deleted))
		fmt.Printf("Number of regular files transferred: %s\n", commas(s.Transferred))
		fmt.Printf("Total file size: %s bytes\n", num(s.TotalSize))
		fmt.Printf("Total transferred file size: %s bytes\n", num(s.TransferredSize))
		fmt.Printf("Literal data: %s bytes\n", num(s.Literal))
		fmt.Printf("Matched data: %s bytes\n", num(s.Matched))
		fmt.Printf("Total bytes sent: %s\n", num(c.sent.N()))
		fmt.Printf("Total bytes received: %s\n\n", num(c.recvd.N()))
	}
	if !(o.Verbose > 0 || o.Stats || c.ui.tty) {
		return
	}
	var parts []string
	if s.Transferred == 0 && r.Deleted == 0 {
		parts = append(parts, fmt.Sprintf("up to date (%s files, %s)", commas(s.NumFiles), humanBytes(s.TotalSize)))
	} else {
		parts = append(parts, fmt.Sprintf("%s of %s files transferred (%s)", commas(s.Transferred), commas(s.NumFiles), humanBytes(s.TransferredSize)))
		if r.Deleted > 0 {
			parts = append(parts, fmt.Sprintf("%s deleted", commas(r.Deleted)))
		}
	}
	if c.remote {
		wire := c.sent.N() + c.recvd.N()
		p := fmt.Sprintf("sent %s, received %s", humanBytes(c.sent.N()), humanBytes(c.recvd.N()))
		if wire > 0 && s.TotalSize > 0 {
			p += fmt.Sprintf(", speedup %.1fx", float64(s.TotalSize)/float64(wire))
		}
		parts = append(parts, p)
	}
	t := "in " + humanDuration(elapsed)
	if s.TransferredSize > 0 && !o.DryRun {
		t += " (" + humanRate(s.TransferredSize, elapsed) + ")"
	}
	parts = append(parts, t)
	line := strings.Join(parts, " · ")
	if o.DryRun {
		line += " (dry run)"
	}
	fmt.Println(line)
}

// Serve runs the server side of a session on stdin/stdout.
func Serve(in io.Reader, out io.WriteCloser) int {
	if err := proto.WriteHello(out, 0); err != nil {
		return ExitProtocol
	}
	var recvd proto.Counter
	br := bufio.NewReaderSize(proto.CountReader(in, &recvd), 64<<10)
	hello, err := proto.ReadHello(br, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xt --server: handshake failed: %v\n", err)
		return ExitProtocol
	}
	if hello.Version != proto.Version {
		fmt.Fprintf(os.Stderr, "xt --server: protocol mismatch (client %d, server %d)\n", hello.Version, proto.Version)
		return ExitProtocol
	}
	limited := &rateWriter{w: out}
	conn := proto.NewConn(br, limited, out, hello.CompressLevel)
	m, err := conn.Recv()
	if err != nil || m.Start == nil {
		fmt.Fprintln(os.Stderr, "xt --server: expected a start message")
		return ExitProtocol
	}
	o := &m.Start.Opts
	limited.rate.Store(o.BwLimit)
	if o.Timeout > 0 {
		defer watchdog(conn, &recvd, time.Duration(o.Timeout)*time.Second, func() {
			fmt.Fprintf(os.Stderr, "xt --server: timeout: no data received for %d seconds\n", o.Timeout)
			os.Exit(ExitTimeout)
		})()
	}
	ui := NewRemoteUI(conn, o)
	paths := make([]string, len(m.Start.Paths))
	for i, p := range m.Start.Paths {
		paths[i] = expandHome(p)
	}
	switch m.Start.Role {
	case "sender":
		err = (&Sender{Conn: conn, Opts: o, UI: ui}).Run(paths)
	case "receiver":
		if len(paths) != 1 {
			err = errors.New("receiver needs exactly one destination")
			break
		}
		err = (&Receiver{Conn: conn, Opts: o, UI: ui, Dest: paths[0], PeerOS: hello.OS}).Run()
	default:
		err = fmt.Errorf("unknown role %q", m.Start.Role)
	}
	if err != nil {
		var ce connError
		var re RemoteError
		if !errors.As(err, &ce) && !errors.As(err, &re) {
			conn.SendAsync(&proto.Msg{Error: err.Error()})
		}
		select {
		case <-closeAsync(conn):
		case <-time.After(2 * time.Second):
		}
		return ExitProtocol
	}
	conn.Close()
	return ExitOK
}

func closeAsync(c *proto.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	return done
}
