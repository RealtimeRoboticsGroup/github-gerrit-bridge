package events

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh/sshtest"
)

// recorder is a Handler that records calls in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
	ch    chan string
}

func newRecorder() *recorder { return &recorder{ch: make(chan string, 100)} }

func (r *recorder) Event(e *Event) {
	n := 0
	if e.Change != nil {
		n = e.Change.Number
	}
	r.add(fmt.Sprintf("%s/%d", e.Type, n))
}

func (r *recorder) Resync(reason string) { r.add("resync:" + reason) }

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
	r.ch <- s
}

// expect waits for the next calls, in order.
func (r *recorder) expect(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-r.ch:
			if got != w {
				t.Fatalf("got call %q, want %q", got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", w)
		}
	}
}

func ev(typ string, change int) string {
	return fmt.Sprintf(`{"type":%q,"change":{"project":"p","number":%d}}`+"\n", typ, change)
}

// pipeSource hands out one io.Pipe per Open, so the test can write events
// to the current session and end it.
type pipeSource struct {
	sessions chan *io.PipeWriter
	opens    int
	mu       sync.Mutex
	failNext int
}

func newPipeSource() *pipeSource { return &pipeSource{sessions: make(chan *io.PipeWriter, 10)} }

func (p *pipeSource) Open(ctx context.Context) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opens++
	if p.failNext > 0 {
		p.failNext--
		return nil, errors.New("connection refused")
	}
	r, w := io.Pipe()
	p.sessions <- w
	return r, nil
}

func (p *pipeSource) next(t *testing.T) *io.PipeWriter {
	t.Helper()
	select {
	case w := <-p.sessions:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("no session opened")
		return nil
	}
}

func run(t *testing.T, s *Stream) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

func TestReconnectResyncs(t *testing.T) {
	src := newPipeSource()
	src.failNext = 2
	rec := newRecorder()
	run(t, &Stream{Source: src, Handler: rec, MinBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond})

	w := src.next(t)
	rec.expect(t, "resync:connected")
	io.WriteString(w, ev("patchset-created", 1)+ev("comment-added", 2))
	rec.expect(t, "patchset-created/1", "comment-added/2")

	// Server hangs up: reconnect, and resync again.
	w.Close()
	w = src.next(t)
	rec.expect(t, "resync:connected")
	io.WriteString(w, ev("change-merged", 3))
	rec.expect(t, "change-merged/3")
	if src.opens != 4 {
		t.Errorf("opens = %d, want 4 (two failures, two sessions)", src.opens)
	}
}

func TestDroppedOutputResyncs(t *testing.T) {
	src := newPipeSource()
	rec := newRecorder()
	run(t, &Stream{Source: src, Handler: rec})
	w := src.next(t)
	rec.expect(t, "resync:connected")
	io.WriteString(w, `{"type":"dropped-output","eventCreatedOn":5}`+"\n"+ev("patchset-created", 9))
	rec.expect(t, "resync:dropped-output", "patchset-created/9")
}

func TestBadLinesSkipped(t *testing.T) {
	src := newPipeSource()
	rec := newRecorder()
	run(t, &Stream{Source: src, Handler: rec, MaxLineBytes: 100})
	w := src.next(t)
	rec.expect(t, "resync:connected")
	big := `{"type":"comment-added","comment":"` + strings.Repeat("x", 500) + `"}` + "\n"
	io.WriteString(w, "garbage\n\n"+big+ev("patchset-created", 4))
	// Garbage and blank lines are ignored; the oversized event forces a
	// resync; the stream keeps going.
	rec.expect(t, "resync:oversized event", "patchset-created/4")
}

func TestLargeLineUnderLimit(t *testing.T) {
	// Larger than bufio's buffer and gerrit-buildkite's 1 MiB Scanner limit.
	src := newPipeSource()
	rec := newRecorder()
	run(t, &Stream{Source: src, Handler: rec})
	w := src.next(t)
	rec.expect(t, "resync:connected")
	big := `{"type":"comment-added","change":{"number":8},"comment":"` + strings.Repeat("y", 3<<20) + `"}` + "\n"
	go io.WriteString(w, big)
	rec.expect(t, "comment-added/8")
}

func TestWatchdogReconnects(t *testing.T) {
	src := newPipeSource()
	rec := newRecorder()
	run(t, &Stream{Source: src, Handler: rec, Watchdog: 30 * time.Millisecond, MinBackoff: time.Millisecond})
	src.next(t)
	rec.expect(t, "resync:connected")
	// Say nothing: the watchdog must close the session and reconnect.
	src.next(t)
	rec.expect(t, "resync:connected")
}

func TestReadLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("short\n"+strings.Repeat("z", 50)+"\nok\ntail"), 16)
	for _, want := range []struct {
		line    string
		tooLong bool
		err     error
	}{
		{"short", false, nil},
		{"", true, nil},
		{"ok", false, nil},
		{"tail", false, io.EOF},
	} {
		line, tooLong, err := readLine(r, 20)
		if string(line) != want.line || tooLong != want.tooLong || err != want.err {
			t.Errorf("readLine = %q, %v, %v; want %q, %v, %v", line, tooLong, err, want.line, want.tooLong, want.err)
		}
	}
}

func TestSSHSourceEndToEnd(t *testing.T) {
	lines := make(chan string, 10)
	srv := sshtest.New(t, func(ctx context.Context, cmd string, stdout, _ io.Writer) int {
		for {
			select {
			case <-ctx.Done():
				return 0
			case l, ok := <-lines:
				if !ok {
					return 0
				}
				io.WriteString(stdout, l)
			}
		}
	})
	cb, algos, err := gerritssh.KnownHosts(srv.KnownHostsFile, srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	src := SSHSource(gerritssh.Config{
		Address: srv.Addr, User: "bridge", Signer: srv.ClientSigner,
		HostKeyCallback: cb, HostKeyAlgorithms: algos,
	}, []string{"patchset-created", "dropped-output"})
	rec := newRecorder()
	run(t, &Stream{Source: src, Handler: rec})
	rec.expect(t, "resync:connected")
	lines <- ev("patchset-created", 42)
	rec.expect(t, "patchset-created/42")
	if got := srv.Execs[0]; got != "gerrit stream-events -s patchset-created -s dropped-output" {
		t.Errorf("exec = %q", got)
	}
}

// Events handed on are counted by type; dropped-output, which only asks
// for a resync, is not one.
func TestStreamCountsEvents(t *testing.T) {
	src := newPipeSource()
	rec := newRecorder()
	s := &Stream{Source: src, Handler: rec}
	run(t, s)
	w := src.next(t)
	rec.expect(t, "resync:connected")
	io.WriteString(w, ev("patchset-created", 1)+ev("comment-added", 2)+ev("comment-added", 3)+`{"type":"dropped-output"}`+"\n")
	rec.expect(t, "patchset-created/1", "comment-added/2", "comment-added/3", "resync:dropped-output")
	if got := s.Counts(); !maps.Equal(got, map[string]int{"patchset-created": 1, "comment-added": 2}) {
		t.Errorf("counts %v", got)
	}
}
