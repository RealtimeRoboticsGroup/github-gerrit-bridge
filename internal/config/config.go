// Package config loads the bridge's YAML configuration.
//
// The file holds deployment settings only: where Gerrit and GitHub are and
// where to find credentials. Secrets are never inline; each one is read from
// the file named by a *_file field. Mapping data that changes at run time
// (identity map, label overrides) lives in refs/bridge/config instead.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	// Listen is the address of the HTTP server for webhooks and /healthz.
	Listen  string `yaml:"listen"`
	Workers int    `yaml:"workers"`
	Gerrit  Gerrit `yaml:"gerrit"`
	GitHub  GitHub `yaml:"github"`
	CI      CI     `yaml:"ci"`
}

// Gerrit is the Gerrit side of the repo pair.
type Gerrit struct {
	// URL is the web/REST base URL, e.g. https://gerrit.example.com.
	URL     string `yaml:"url"`
	Project string `yaml:"project"`
	// HTTPUser and HTTPPasswordFile authenticate REST calls (/a/ endpoints).
	HTTPUser         string `yaml:"http_user"`
	HTTPPasswordFile string `yaml:"http_password_file"`
	SSH              SSH    `yaml:"ssh"`
}

// SSH configures the connection used for stream-events and SSH commands.
type SSH struct {
	// Address is host:port, e.g. gerrit.example.com:29418.
	Address        string `yaml:"address"`
	User           string `yaml:"user"`
	KeyFile        string `yaml:"key_file"`
	KnownHostsFile string `yaml:"known_hosts_file"`
	// KeepaliveInterval and KeepaliveCountMax mirror OpenSSH's
	// ServerAliveInterval and ServerAliveCountMax.
	KeepaliveInterval time.Duration `yaml:"keepalive_interval"`
	KeepaliveCountMax int           `yaml:"keepalive_count_max"`
	// Watchdog reconnects (and resyncs) if no event arrives for this long.
	// Zero disables it; quiet servers can legitimately send nothing.
	Watchdog time.Duration `yaml:"watchdog"`
}

// GitHub is the GitHub side of the repo pair.
type GitHub struct {
	Owner             string `yaml:"owner"`
	Repo              string `yaml:"repo"`
	WebhookSecretFile string `yaml:"webhook_secret_file"`
}

// CI configures how Gerrit CI is mirrored to GitHub (design §6.5).
type CI struct {
	// TrustedGroup is the Gerrit group whose uploads CI builds
	// automatically. The bridge account must not be a member.
	TrustedGroup string `yaml:"trusted_group"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates a config. Unknown keys are an error, so a
// typo does not silently fall back to a default.
func Parse(b []byte) (*Config, error) {
	c := &Config{
		Listen:  ":8080",
		Workers: 4,
		Gerrit: Gerrit{SSH: SSH{
			KeepaliveInterval: 15 * time.Second,
			KeepaliveCountMax: 3,
		}},
		CI: CI{TrustedGroup: "Verified Users"},
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	c.Gerrit.URL = strings.TrimRight(c.Gerrit.URL, "/")
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []error
	req := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	req("gerrit.url", c.Gerrit.URL)
	req("gerrit.project", c.Gerrit.Project)
	req("gerrit.http_user", c.Gerrit.HTTPUser)
	req("gerrit.http_password_file", c.Gerrit.HTTPPasswordFile)
	req("gerrit.ssh.address", c.Gerrit.SSH.Address)
	req("gerrit.ssh.user", c.Gerrit.SSH.User)
	req("gerrit.ssh.key_file", c.Gerrit.SSH.KeyFile)
	req("gerrit.ssh.known_hosts_file", c.Gerrit.SSH.KnownHostsFile)
	req("github.owner", c.GitHub.Owner)
	req("github.repo", c.GitHub.Repo)
	req("github.webhook_secret_file", c.GitHub.WebhookSecretFile)
	if c.Workers < 1 {
		errs = append(errs, errors.New("workers must be at least 1"))
	}
	if c.Gerrit.SSH.KeepaliveInterval < 0 || c.Gerrit.SSH.KeepaliveCountMax < 0 || c.Gerrit.SSH.Watchdog < 0 {
		errs = append(errs, errors.New("gerrit.ssh durations and counts must not be negative"))
	}
	return errors.Join(errs...)
}

// ReadSecret reads a secret from path, trimming surrounding whitespace.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}
