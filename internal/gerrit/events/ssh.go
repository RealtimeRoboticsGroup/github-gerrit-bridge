package events

import (
	"context"
	"io"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh"
)

// SSHSource opens each session on a fresh SSH connection, so a reconnect
// never reuses a connection that has gone bad.
func SSHSource(cfg gerritssh.Config, types []string) Source {
	cmd := Command(types)
	return SourceFunc(func(ctx context.Context) (io.ReadCloser, error) {
		c, err := gerritssh.Dial(ctx, cfg)
		if err != nil {
			return nil, err
		}
		r, err := c.Stream(cmd)
		if err != nil {
			c.Close()
			return nil, err
		}
		return &sshStream{ReadCloser: r, c: c}, nil
	})
}

type sshStream struct {
	io.ReadCloser
	c *gerritssh.Client
}

func (s *sshStream) Close() error {
	s.ReadCloser.Close()
	return s.c.Close()
}
