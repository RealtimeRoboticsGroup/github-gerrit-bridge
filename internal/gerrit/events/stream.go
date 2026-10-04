package events

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Types is the default set of event types the bridge subscribes to.
var Types = []string{
	"patchset-created",
	"comment-added",
	"change-abandoned",
	"change-restored",
	"change-merged",
	"change-deleted",
	"wip-state-changed",
	"private-state-changed",
	"reviewer-added",
	"reviewer-deleted",
	"vote-deleted",
	"hashtags-changed",
	"topic-changed",
	"ref-updated",
	"dropped-output",
}

// Command is the stream-events command line subscribing to types.
func Command(types []string) string {
	var b strings.Builder
	b.WriteString("gerrit stream-events")
	for _, t := range types {
		b.WriteString(" -s ")
		b.WriteString(t)
	}
	return b.String()
}

// Source opens one stream-events session. The returned reader yields one
// JSON event per line; it is closed by the Stream.
type Source interface {
	Open(ctx context.Context) (io.ReadCloser, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context) (io.ReadCloser, error)

// Open calls f.
func (f SourceFunc) Open(ctx context.Context) (io.ReadCloser, error) { return f(ctx) }

// Handler receives events. Its methods are called from the read loop and
// must not block: they should only queue work.
type Handler interface {
	// Event is called for every decoded event, including partially
	// decoded ones.
	Event(e *Event)
	// Resync is called when events may have been missed: after every
	// (re)connect, and when Gerrit reports dropped-output.
	Resync(reason string)
}

// Stream keeps a stream-events session open, reconnecting with backoff.
type Stream struct {
	Source  Source
	Handler Handler
	Logger  *slog.Logger

	// MinBackoff and MaxBackoff bound the delay between reconnects. The
	// backoff resets once a session has stayed up for MaxBackoff.
	MinBackoff, MaxBackoff time.Duration
	// Watchdog reconnects if no line arrives for this long. Zero disables.
	Watchdog time.Duration
	// MaxLineBytes skips (and logs) events longer than this.
	MaxLineBytes int

	mu       sync.Mutex
	lastLine time.Time
}

// LastEvent is when the last line was read, for health checks.
func (s *Stream) LastEvent() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastLine
}

// Run reads events until ctx is cancelled. It always returns ctx.Err().
func (s *Stream) Run(ctx context.Context) error {
	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB < minB {
		maxB = max(time.Minute, minB)
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	backoff := minB
	for {
		start := time.Now()
		err := s.session(ctx, logger)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) >= maxB {
			backoff = minB
		}
		// Full jitter in [backoff/2, backoff).
		d := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		logger.Warn("stream-events disconnected; reconnecting", "err", err, "in", d.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		backoff = min(backoff*2, maxB)
	}
}

var errWatchdog = errors.New("watchdog: no events")

func (s *Stream) session(ctx context.Context, logger *slog.Logger) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rc, err := s.Source.Open(ctx)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer rc.Close()
	// Closing the reader is the only way to interrupt a blocked Read.
	stop := context.AfterFunc(ctx, func() { rc.Close() })
	defer stop()

	logger.Info("stream-events connected")
	// Anything that happened while we were disconnected was not streamed
	// to us. Resync after opening, so nothing falls in the gap.
	s.Handler.Resync("connected")

	var watchdog *time.Timer
	if s.Watchdog > 0 {
		watchdog = time.AfterFunc(s.Watchdog, func() { cancel(errWatchdog) })
		defer watchdog.Stop()
	}

	maxLine := s.MaxLineBytes
	if maxLine <= 0 {
		maxLine = 64 << 20
	}
	r := bufio.NewReaderSize(rc, 64<<10)
	for {
		line, tooLong, err := readLine(r, maxLine)
		if len(line) > 0 || tooLong {
			s.mu.Lock()
			s.lastLine = time.Now()
			s.mu.Unlock()
			if watchdog != nil {
				watchdog.Reset(s.Watchdog)
			}
		}
		if tooLong {
			logger.Error("skipping oversized event", "limit", maxLine)
			// We may have lost a change event; catch up.
			s.Handler.Resync("oversized event")
		} else if len(line) > 0 {
			s.dispatch(line, logger)
		}
		if err != nil {
			if c := context.Cause(ctx); c != nil {
				return c
			}
			if err == io.EOF {
				return errors.New("stream closed by server")
			}
			return err
		}
	}
}

func (s *Stream) dispatch(line []byte, logger *slog.Logger) {
	e, err := Parse(line)
	if e == nil {
		logger.Warn("ignoring undecodable line", "err", err, "line", truncate(line, 200))
		return
	}
	if err != nil {
		logger.Warn("event decoded partially", "err", err, "line", truncate(line, 500))
	}
	if e.Type == "dropped-output" {
		logger.Warn("gerrit dropped stream-events output; resyncing")
		s.Handler.Resync("dropped-output")
		return
	}
	s.Handler.Event(e)
}

// readLine reads one '\n'-terminated line without the newline. If the line
// is longer than max, it is consumed and discarded and tooLong is true. A
// final unterminated line is returned with the error.
func readLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		frag, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(frag) > max+1 {
				tooLong, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
		}
		return line, tooLong, err
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
