// Package sshtest is an in-process SSH server for testing Gerrit SSH
// clients.
package sshtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Handler runs one exec request. ctx is cancelled when the client closes
// the session. The return value is the exit status.
type Handler func(ctx context.Context, cmd string, stdout, stderr io.Writer) int

// Server is a test SSH server on 127.0.0.1.
type Server struct {
	Addr string
	// ClientSigner is the key the server accepts.
	ClientSigner ssh.Signer
	// KnownHostsFile lists the server's host key for Addr.
	KnownHostsFile string

	handler Handler
	ln      net.Listener

	mu sync.Mutex
	// ignoreKeepalives makes the server read but never answer global
	// requests, like a hung server.
	ignoreKeepalives bool
	conns            []net.Conn
	Execs            []string
}

// New starts a server that runs handler for every exec request. It is shut
// down when the test ends.
func New(t testing.TB, handler Handler) *Server {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Addr:         ln.Addr().String(),
		ClientSigner: clientSigner,
		handler:      handler,
		ln:           ln,
	}
	s.KnownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{s.Addr}, hostSigner.PublicKey()) + "\n"
	if err := os.WriteFile(s.KnownHostsFile, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	want := clientSigner.PublicKey().Marshal()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) != string(want) {
				return nil, errUnknownKey
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	go s.serve(cfg)
	t.Cleanup(s.Close)
	return s
}

// IgnoreKeepalives makes the server stop answering global requests.
func (s *Server) IgnoreKeepalives() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ignoreKeepalives = true
}

// Close stops the listener and drops every connection.
func (s *Server) Close() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
}

// ExecCount returns how many exec requests the server has received.
func (s *Server) ExecCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Execs)
}

func (s *Server) serve(cfg *ssh.ServerConfig) {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		go s.handleConn(nc, cfg)
	}
}

func (s *Server) handleConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer sc.Close()
	go func() {
		for r := range reqs {
			s.mu.Lock()
			ignore := s.ignoreKeepalives
			s.mu.Unlock()
			if !ignore && r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, creqs)
	}
}

func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for r := range reqs {
		if r.Type != "exec" || len(r.Payload) < 4 {
			r.Reply(false, nil)
			continue
		}
		n := binary.BigEndian.Uint32(r.Payload)
		cmd := string(r.Payload[4 : 4+n])
		r.Reply(true, nil)
		s.mu.Lock()
		s.Execs = append(s.Execs, cmd)
		s.mu.Unlock()
		// Further requests (and the channel closing) end the command.
		go func() {
			for range reqs {
			}
			cancel()
		}()
		status := s.handler(ctx, cmd, ch, ch.Stderr())
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(status))
		ch.SendRequest("exit-status", false, b[:])
		ch.Close()
		return
	}
}

type sshError string

func (e sshError) Error() string { return string(e) }

const errUnknownKey = sshError("unknown client key")
