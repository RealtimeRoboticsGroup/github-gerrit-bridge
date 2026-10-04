// Package gerritssh runs commands on Gerrit's SSH port (stream-events,
// ls-members, and later suexec) with golang.org/x/crypto/ssh.
//
// Compared with shelling out to ssh, this needs no ~/.ssh setup on the host,
// verifies the host key against an explicit known_hosts file, sends
// protocol-level keepalives, and closes everything when a context is
// cancelled.
package gerritssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Config says how to reach Gerrit's SSH daemon.
type Config struct {
	Address string // host:port
	User    string
	Signer  ssh.Signer
	// HostKeyCallback verifies the server; see KnownHosts.
	HostKeyCallback ssh.HostKeyCallback
	// HostKeyAlgorithms restricts negotiation to the key types we know for
	// the host, so the server cannot offer a type we have no entry for.
	HostKeyAlgorithms []string

	// KeepaliveInterval is how often to send a keepalive request; zero
	// disables keepalives. After KeepaliveCountMax unanswered keepalives the
	// connection is closed.
	KeepaliveInterval time.Duration
	KeepaliveCountMax int

	Logger *slog.Logger
}

// LoadSigner reads an unencrypted private key file.
func LoadSigner(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// KnownHosts returns a host key callback for the known_hosts file, and the
// host key algorithms that file lists for address.
//
// Without the algorithm list, x/crypto/ssh may negotiate a key type (say
// ecdsa) when the file only has the host's ed25519 key, and then reject the
// host as unknown.
func KnownHosts(path, address string) (ssh.HostKeyCallback, []string, error) {
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, nil, err
	}
	// Ask the callback about a key that cannot match; the KeyError lists
	// the keys it does know for this host.
	probe, err := probeKey()
	if err != nil {
		return nil, nil, err
	}
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		// Matching uses the hostname; the remote address only matters for
		// IP-pattern lines, so a dummy is fine if resolution fails here.
		addr = &net.TCPAddr{IP: net.IPv4zero}
	}
	var algos []string
	var ke *knownhosts.KeyError
	if err := cb(address, addr, probe); errors.As(err, &ke) {
		for _, k := range ke.Want {
			algos = append(algos, algorithmsFor(k.Key.Type())...)
		}
	}
	if len(algos) == 0 {
		return nil, nil, fmt.Errorf("%s has no host key for %s", path, address)
	}
	return cb, algos, nil
}

func algorithmsFor(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		// The key type is ssh-rsa, but the signature algorithms are these.
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// probeKey is a fixed ed25519 public key nobody has the private half of.
func probeKey() (ssh.PublicKey, error) {
	return ssh.NewPublicKey(ed25519Probe)
}

// Client is one SSH connection to Gerrit.
type Client struct {
	conn   *ssh.Client
	logger *slog.Logger
	done   chan struct{}
	once   sync.Once
}

// Dial connects to Gerrit. The connection is closed when ctx is cancelled
// or Close is called.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cc := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(cfg.Signer)},
		HostKeyCallback:   cfg.HostKeyCallback,
		HostKeyAlgorithms: cfg.HostKeyAlgorithms,
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, err
	}
	// Bound the handshake by ctx as well as by a deadline.
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(nc, cfg.Address, cc)
	nc.SetDeadline(time.Time{})
	if !stop() {
		if err == nil {
			sc.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil {
		nc.Close()
		return nil, err
	}
	c := &Client{conn: ssh.NewClient(sc, chans, reqs), logger: logger, done: make(chan struct{})}
	context.AfterFunc(ctx, func() { c.Close() })
	go func() {
		c.conn.Wait()
		c.Close()
	}()
	if cfg.KeepaliveInterval > 0 {
		go c.keepalive(cfg.KeepaliveInterval, max(cfg.KeepaliveCountMax, 1))
	}
	return c, nil
}

// keepalive sends keepalive@openssh.com requests. Any reply, including a
// "not supported" failure, proves the server is alive.
func (c *Client) keepalive(interval time.Duration, countMax int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		reply := make(chan error, 1)
		go func() {
			_, _, err := c.conn.SendRequest("keepalive@openssh.com", true, nil)
			reply <- err
		}()
		select {
		case <-c.done:
			return
		case err := <-reply:
			if err != nil {
				c.logger.Warn("ssh keepalive failed; closing", "err", err)
				c.Close()
				return
			}
			missed = 0
		case <-time.After(interval):
			missed++
			if missed >= countMax {
				c.logger.Warn("ssh keepalives unanswered; closing", "missed", missed)
				c.Close()
				return
			}
		}
	}
}

// Close closes the connection. It is safe to call more than once.
func (c *Client) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		err = c.conn.Close()
	})
	return err
}

// Done is closed when the connection is closed.
func (c *Client) Done() <-chan struct{} { return c.done }

// Run runs cmd and returns its stdout. A non-zero exit includes stderr in
// the error.
func (c *Client) Run(ctx context.Context, cmd string) ([]byte, error) {
	s, err := c.conn.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	var stdout, stderr bytes.Buffer
	s.Stdout = &stdout
	s.Stderr = &stderr
	if err := s.Run(cmd); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %w: %s", firstWord(cmd), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Stream starts cmd and returns its stdout. Closing the returned reader
// ends the command. After stdout reaches EOF, Read returns the command's
// exit error (with its stderr) if it failed, instead of io.EOF.
func (c *Client) Stream(cmd string) (io.ReadCloser, error) {
	s, err := c.conn.NewSession()
	if err != nil {
		return nil, err
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		s.Close()
		return nil, err
	}
	st := &stream{s: s, stdout: stdout, cmd: cmd}
	s.Stderr = &st.stderr
	if err := s.Start(cmd); err != nil {
		s.Close()
		return nil, err
	}
	return st, nil
}

type stream struct {
	s      *ssh.Session
	stdout io.Reader
	stderr limitedBuffer
	cmd    string
}

func (st *stream) Read(p []byte) (int, error) {
	n, err := st.stdout.Read(p)
	if err == io.EOF {
		if werr := st.s.Wait(); werr != nil {
			return n, fmt.Errorf("%s: %w: %s", firstWord(st.cmd), werr, strings.TrimSpace(st.stderr.String()))
		}
	}
	return n, err
}

func (st *stream) Close() error { return st.s.Close() }

// limitedBuffer keeps the first 4 KiB written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 4096 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func firstWord(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) >= 2 && f[0] == "gerrit" {
		return f[0] + " " + f[1]
	}
	if len(f) > 0 {
		return f[0]
	}
	return cmd
}

// ed25519Probe is an arbitrary 32-byte ed25519 public key.
var ed25519Probe = ed25519.PublicKey{
	0x3d, 0x40, 0x17, 0xc3, 0xe8, 0x43, 0x89, 0x5a, 0x92, 0xb7, 0x0a, 0xa7, 0x4d, 0x1b, 0x7e, 0xbc,
	0x9c, 0x98, 0x2c, 0xcf, 0x2e, 0xc4, 0x96, 0x8c, 0xc0, 0xcd, 0x55, 0xf1, 0x2a, 0xf4, 0x66, 0x0c,
}
