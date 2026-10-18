package xfer

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
)

// UI prints messages and tracks progress. On the server it relays messages
// to the client instead of printing them.
type UI struct {
	mu   sync.Mutex
	conn *proto.Conn // set on the server

	out, errw io.Writer
	quiet     bool
	verbose   int
	fileLines bool // print a summary line per transferred file (--progress)
	itemize   bool // the receiver prints -i lines in place of file names
	human     bool

	live    bool // redraw a status line on the terminal
	tty     bool // live display was enabled; stays set after Close
	fd      int
	lineLen int // runes of the status line currently on screen
	stop    chan struct{}
	stopped chan struct{}

	t tracker
}

type tracker struct {
	phase    string
	listed   int64
	compared int64

	planned              bool
	planFiles, planBytes int64

	doneFiles int64
	doneBytes int64 // includes resumed prefixes and skipped bytes
	xferBytes int64 // bytes actually processed; drives the rate

	cur              string
	curSize, curDone int64
	curStart         time.Time
	curResumed       int64

	start   time.Time
	samples []sample
}

type sample struct {
	t time.Time
	n int64
}

// NewClientUI returns a UI that prints to the terminal.
func NewClientUI(o *opt.Options) *UI {
	u := &UI{
		out:       os.Stdout,
		errw:      os.Stderr,
		quiet:     o.Quiet,
		verbose:   o.Verbose,
		fileLines: o.Progress && !o.Quiet,
		human:     o.Human,
		itemize:   o.Itemize > 0,
	}
	u.t.start = time.Now()
	u.fd = int(os.Stdout.Fd())
	if !o.Quiet && !o.NoProgress && term.IsTerminal(u.fd) {
		u.live, u.tty = true, true
		u.stop = make(chan struct{})
		u.stopped = make(chan struct{})
		go u.ticker()
	}
	return u
}

// NewRemoteUI returns a UI that forwards messages over conn.
func NewRemoteUI(conn *proto.Conn, o *opt.Options) *UI {
	return &UI{conn: conn, quiet: o.Quiet, verbose: o.Verbose}
}

// Client reports whether this UI prints locally.
func (u *UI) Client() bool { return u.conn == nil }

func (u *UI) ticker() {
	defer close(u.stopped)
	tk := time.NewTicker(120 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-u.stop:
			return
		case <-tk.C:
			u.mu.Lock()
			u.sampleLocked()
			u.drawLocked()
			u.mu.Unlock()
		}
	}
}

// Close stops the live display and clears it.
func (u *UI) Close() {
	if u.live {
		close(u.stop)
		<-u.stopped
		u.mu.Lock()
		u.clearLocked()
		u.live = false
		u.mu.Unlock()
	}
}

// Info prints an informational message.
func (u *UI) Info(format string, args ...any) {
	if u.quiet {
		return
	}
	u.emit(fmt.Sprintf(format, args...), false)
}

// Error prints an error message.
func (u *UI) Error(format string, args ...any) {
	msg := "xt: " + fmt.Sprintf(format, args...)
	if u.conn != nil {
		msg = "xt (remote): " + fmt.Sprintf(format, args...)
	}
	u.emit(msg, true)
}

// Relay prints a message received from the peer.
func (u *UI) Relay(l *proto.Log) {
	if !l.Err && u.quiet {
		return
	}
	u.emit(l.Text, l.Err)
}

func (u *UI) emit(text string, isErr bool) {
	if u.conn != nil {
		u.conn.SendAsync(&proto.Msg{Log: &proto.Log{Text: text, Err: isErr}})
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
	w := u.out
	if isErr {
		w = u.errw
	}
	fmt.Fprintln(w, text)
	u.drawLocked()
}

// Phase sets the status shown before data starts flowing.
func (u *UI) Phase(p string) {
	u.mu.Lock()
	u.t.phase = p
	u.mu.Unlock()
}

// Listed records the number of files found so far.
func (u *UI) Listed(n int64) {
	u.mu.Lock()
	u.t.listed = n
	u.mu.Unlock()
}

// Compared records how many file list entries have been checked.
func (u *UI) Compared(n int64) {
	u.mu.Lock()
	u.t.compared = n
	u.mu.Unlock()
}

// SetPlan records the total amount of work to do.
func (u *UI) SetPlan(files, bytes int64) {
	u.mu.Lock()
	u.t.planned = true
	u.t.planFiles, u.t.planBytes = files, bytes
	u.t.phase = ""
	u.mu.Unlock()
}

// FileStart marks the beginning of a file transfer. resumed bytes are
// already present on the receiver.
func (u *UI) FileStart(name string, size, resumed int64) {
	if !u.Client() {
		return
	}
	u.mu.Lock()
	u.t.cur, u.t.curSize, u.t.curDone, u.t.curResumed = name, size, resumed, resumed
	u.t.curStart = time.Now()
	u.t.doneBytes += resumed
	u.mu.Unlock()
}

// Write counts n processed bytes of the current file; it lets the UI be used
// as an io.Writer in a TeeReader or MultiWriter.
func (u *UI) Write(p []byte) (int, error) {
	u.mu.Lock()
	u.t.curDone += int64(len(p))
	u.t.doneBytes += int64(len(p))
	u.t.xferBytes += int64(len(p))
	u.mu.Unlock()
	return len(p), nil
}

// FileEnd marks the end of the current file and prints its line.
func (u *UI) FileEnd(st proto.Stats, ok bool) {
	if !u.Client() {
		return
	}
	u.mu.Lock()
	name, size := u.t.cur, u.t.curSize
	// Account for any bytes the size estimate missed (e.g. file changed).
	if rest := size - u.t.curDone; ok && rest > 0 {
		u.t.doneBytes += rest
	}
	u.t.doneFiles++
	elapsed := time.Since(u.t.curStart)
	processed := u.t.curDone - u.t.curResumed
	resumed := u.t.curResumed
	u.t.cur = ""
	u.mu.Unlock()
	if !ok || u.quiet {
		return
	}
	switch {
	case u.fileLines:
		var notes []string
		if resumed > 0 {
			notes = append(notes, "resumed at "+humanBytes(resumed))
		}
		if total := st.Literal + st.Matched; st.Matched > 0 && total > 0 {
			notes = append(notes, fmt.Sprintf("%d%% reused", st.Matched*100/total))
		}
		line := fmt.Sprintf("%s  %s  %s  %s", name, humanBytes(size), humanRate(processed, elapsed), humanDuration(elapsed))
		if len(notes) > 0 {
			line += "  (" + strings.Join(notes, ", ") + ")"
		}
		u.Info("%s", line)
	case u.verbose > 0 && !u.itemize:
		u.Info("%s", name)
	}
}

func (u *UI) sampleLocked() {
	now := time.Now()
	u.t.samples = append(u.t.samples, sample{now, u.t.xferBytes})
	cut := 0
	for cut < len(u.t.samples)-1 && now.Sub(u.t.samples[cut].t) > 5*time.Second {
		cut++
	}
	u.t.samples = u.t.samples[cut:]
}

func (u *UI) rateLocked() float64 {
	s := u.t.samples
	if len(s) < 2 {
		return 0
	}
	dt := s[len(s)-1].t.Sub(s[0].t).Seconds()
	if dt < 0.3 {
		return 0
	}
	return float64(s[len(s)-1].n-s[0].n) / dt
}

func (u *UI) clearLocked() {
	if u.lineLen > 0 {
		fmt.Fprint(u.out, "\r"+strings.Repeat(" ", u.lineLen)+"\r")
		u.lineLen = 0
	}
}

var spinFrames = func() []string {
	if runtime.GOOS == "windows" {
		return []string{"|", "/", "-", `\`}
	}
	return []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
}()

func (u *UI) drawLocked() {
	if !u.live {
		return
	}
	width := 80
	if w, _, err := term.GetSize(u.fd); err == nil && w > 20 {
		width = w
	}
	line := u.statusLocked(width - 1)
	n := utf8.RuneCountInString(line)
	pad := ""
	if u.lineLen > n {
		pad = strings.Repeat(" ", u.lineLen-n)
	}
	fmt.Fprint(u.out, "\r"+line+pad)
	if n == 0 {
		fmt.Fprint(u.out, "\r")
	}
	u.lineLen = n
}

func (u *UI) statusLocked(width int) string {
	t := &u.t
	spin := spinFrames[int(time.Since(t.start)/(120*time.Millisecond))%len(spinFrames)]
	if !t.planned {
		var s string
		switch t.phase {
		case "":
			return ""
		case "comparing":
			s = fmt.Sprintf("%s comparing files… %s/%s", spin, commas(t.compared), commas(t.listed))
		case "remote-compare":
			s = fmt.Sprintf("%s %s files listed, comparing on remote…", spin, commas(t.listed))
		default:
			s = fmt.Sprintf("%s %s… %s files", spin, t.phase, commas(t.listed))
		}
		return truncRight(s, width)
	}
	rate := u.rateLocked()
	pct := 100
	if t.planBytes > 0 {
		pct = int(min(100, t.doneBytes*100/t.planBytes))
	}
	eta := "--"
	if rate > 0 && t.planBytes > t.doneBytes {
		eta = humanDuration(max(time.Second, time.Duration(float64(t.planBytes-t.doneBytes)/rate*float64(time.Second))))
	} else if t.planBytes <= t.doneBytes {
		eta = "0s"
	}
	files := min(t.doneFiles+1, t.planFiles)
	if t.cur == "" {
		files = t.doneFiles
	}
	stats := fmt.Sprintf(" %3d%% %s  %s/%s  %s/s  ETA %s  [%s/%s]",
		pct, "%BAR%", humanBytes(t.doneBytes), humanBytes(t.planBytes),
		humanBytes(int64(rate)), eta, commas(files), commas(t.planFiles))
	barW := 24
	fixed := utf8.RuneCountInString(stats) - len("%BAR%")
	if fixed+barW > width {
		barW = max(0, width-fixed)
	}
	stats = strings.Replace(stats, "%BAR%", bar(pct, barW), 1)
	if rest := width - utf8.RuneCountInString(stats) - 2; rest > 8 && t.cur != "" {
		stats += "  " + truncLeft(t.cur, rest)
	}
	return truncRight(stats, width)
}

func bar(pct, w int) string {
	if w <= 0 {
		return ""
	}
	full := pct * w / 100
	return strings.Repeat("█", full) + strings.Repeat("░", w-full)
}

func truncRight(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	if w <= 1 {
		return string(r[:max(w, 0)])
	}
	return string(r[:w-1]) + "…"
}

func truncLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		f /= unit
		if f < unit || u == "PiB" {
			if f < 10 {
				return fmt.Sprintf("%.2f %s", f, u)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func humanRate(n int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return humanBytes(int64(float64(n)/d.Seconds())) + "/s"
}

func humanDuration(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
	}
}
