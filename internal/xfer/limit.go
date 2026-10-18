package xfer

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/gurupras/xt/internal/proto"
)

// rateWriter limits the rate of writes to w (--bwlimit). The rate may be
// set after the writer is in use; 0 means unlimited. It is used by one
// goroutine at a time.
type rateWriter struct {
	w     io.Writer
	rate  atomic.Int64 // bytes per second
	start time.Time
	n     int64 // bytes written since start
}

func (l *rateWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		rate := l.rate.Load()
		if rate <= 0 {
			n, err := l.w.Write(p)
			return written + n, err
		}
		chunk := p[:min(len(p), int(max(rate/20, 512)))]
		n, err := l.w.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
		now := time.Now()
		if l.start.IsZero() {
			l.start = now
		}
		l.n += int64(n)
		ahead := time.Duration(float64(l.n)/float64(rate)*float64(time.Second)) - now.Sub(l.start)
		switch {
		case ahead > 0:
			time.Sleep(ahead)
		case ahead < -time.Second:
			// Idle for a while: don't let the saved-up allowance burst.
			l.start, l.n = now, 0
		}
	}
	return written, nil
}

// watchdog implements --timeout: it sends keepalives so the peer sees
// traffic, and calls expired if nothing arrives from the peer for timeout.
// The returned function stops it.
func watchdog(conn *proto.Conn, recvd *proto.Counter, timeout time.Duration, expired func()) func() {
	stop := make(chan struct{})
	tick := min(timeout/4, time.Second)
	go func() {
		tk := time.NewTicker(tick)
		defer tk.Stop()
		last, lastAt := recvd.N(), time.Now()
		sent := time.Now()
		for {
			select {
			case <-stop:
				return
			case now := <-tk.C:
				if now.Sub(sent) >= timeout/4 {
					if conn.SendAsync(&proto.Msg{Keepalive: true}) != nil {
						return
					}
					sent = now
				}
				if n := recvd.N(); n != last {
					last, lastAt = n, now
				} else if now.Sub(lastAt) >= timeout {
					expired()
					return
				}
			}
		}
	}()
	var once atomic.Bool
	return func() {
		if !once.Swap(true) {
			close(stop)
		}
	}
}
