package gerritssh

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh/sshtest"
)

func dial(t *testing.T, srv *sshtest.Server, mod func(*Config)) *Client {
	t.Helper()
	cb, algos, err := KnownHosts(srv.KnownHostsFile, srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Address:           srv.Addr,
		User:              "bridge",
		Signer:            srv.ClientSigner,
		HostKeyCallback:   cb,
		HostKeyAlgorithms: algos,
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestKnownHostsAlgorithms(t *testing.T) {
	srv := sshtest.New(t, nil)
	_, algos, err := KnownHosts(srv.KnownHostsFile, srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(algos) != 1 || algos[0] != "ssh-ed25519" {
		t.Errorf("algos = %v, want [ssh-ed25519]", algos)
	}
	if _, _, err := KnownHosts(srv.KnownHostsFile, "other.example.com:29418"); err == nil {
		t.Error("KnownHosts accepted a host with no entry")
	}
}

func TestWrongHostKeyRejected(t *testing.T) {
	a := sshtest.New(t, nil)
	b := sshtest.New(t, nil)
	// Trust a's key under b's address.
	line, _ := os.ReadFile(a.KnownHostsFile)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte(strings.Replace(string(line), a.Addr[strings.LastIndex(a.Addr, ":")+1:], b.Addr[strings.LastIndex(b.Addr, ":")+1:], 1)), 0o600)
	cb, algos, err := KnownHosts(kh, b.Addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Dial(context.Background(), Config{
		Address: b.Addr, User: "bridge", Signer: b.ClientSigner,
		HostKeyCallback: cb, HostKeyAlgorithms: algos,
	})
	if err == nil {
		t.Fatal("Dial succeeded with the wrong host key")
	}
}

func TestRun(t *testing.T) {
	srv := sshtest.New(t, func(_ context.Context, cmd string, stdout, stderr io.Writer) int {
		if cmd == "gerrit ls-members fail" {
			fmt.Fprintln(stderr, "fatal: Group not found")
			return 1
		}
		fmt.Fprintf(stdout, "ran %q", cmd)
		return 0
	})
	c := dial(t, srv, nil)
	out, err := c.Run(context.Background(), "gerrit version")
	if err != nil || string(out) != `ran "gerrit version"` {
		t.Errorf("Run = %q, %v", out, err)
	}
	_, err = c.Run(context.Background(), "gerrit ls-members fail")
	if err == nil || !strings.Contains(err.Error(), "Group not found") || !strings.Contains(err.Error(), "gerrit ls-members") {
		t.Errorf("err = %v, want exit error with stderr", err)
	}
}

func TestStreamExitError(t *testing.T) {
	srv := sshtest.New(t, func(_ context.Context, _ string, stdout, stderr io.Writer) int {
		fmt.Fprintln(stdout, "line")
		fmt.Fprintln(stderr, "fatal: not permitted")
		return 1
	})
	c := dial(t, srv, nil)
	r, err := c.Stream("gerrit stream-events")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if string(b) != "line\n" {
		t.Errorf("read %q", b)
	}
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("err = %v, want exit error", err)
	}
}

func TestStreamCloseEndsCommand(t *testing.T) {
	ended := make(chan struct{})
	srv := sshtest.New(t, func(ctx context.Context, _ string, stdout, _ io.Writer) int {
		<-ctx.Done()
		close(ended)
		return 0
	})
	c := dial(t, srv, nil)
	r, err := c.Stream("gerrit stream-events")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	r.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the stream did not end the command")
	}
}

func TestKeepaliveClosesDeadConnection(t *testing.T) {
	srv := sshtest.New(t, nil)
	srv.IgnoreKeepalives()
	c := dial(t, srv, func(cfg *Config) {
		cfg.KeepaliveInterval = 20 * time.Millisecond
		cfg.KeepaliveCountMax = 2
	})
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection to an unresponsive server was not closed")
	}
}

func TestKeepaliveKeepsLiveConnection(t *testing.T) {
	srv := sshtest.New(t, nil)
	c := dial(t, srv, func(cfg *Config) {
		cfg.KeepaliveInterval = 10 * time.Millisecond
		cfg.KeepaliveCountMax = 1
	})
	select {
	case <-c.Done():
		t.Fatal("live connection was closed")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDialContextCancel(t *testing.T) {
	srv := sshtest.New(t, nil)
	cb, algos, _ := KnownHosts(srv.KnownHostsFile, srv.Addr)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := Dial(ctx, Config{Address: srv.Addr, User: "u", Signer: srv.ClientSigner, HostKeyCallback: cb, HostKeyAlgorithms: algos})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling the dial context did not close the client")
	}
}
