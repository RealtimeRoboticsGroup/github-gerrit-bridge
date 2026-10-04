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
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	// Listen is the address of the HTTP server for webhooks and /healthz.
	Listen  string `yaml:"listen"`
	Workers int    `yaml:"workers"`
	// ResyncInterval queues a full resync periodically, so missed events
	// (or webhooks that cannot reach the bridge) heal. Zero disables.
	ResyncInterval time.Duration `yaml:"resync_interval"`
	Gerrit         Gerrit        `yaml:"gerrit"`
	GitHub         GitHub        `yaml:"github"`
	Identity       Identity      `yaml:"identity"`
	CI             CI            `yaml:"ci"`
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
	// MirrorDir is the bridge's bare mirror of the project, created if
	// missing. It is a cache: deleting it loses nothing.
	MirrorDir string `yaml:"mirror_dir"`
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
	// PeerKeyFile is a key listed in Gerrit's etc/peer_keys. With it,
	// imports from GitHub are pushed as the mapped Gerrit user (suexec),
	// so that user owns the change. Without it, the bridge owns them.
	PeerKeyFile string `yaml:"peer_key_file"`
	// Watchdog reconnects (and resyncs) if no event arrives for this long.
	// Zero disables it; quiet servers can legitimately send nothing.
	Watchdog time.Duration `yaml:"watchdog"`
}

// GitHub is the GitHub side of the repo pair.
type GitHub struct {
	Owner             string `yaml:"owner"`
	Repo              string `yaml:"repo"`
	WebhookSecretFile string `yaml:"webhook_secret_file"`
	// The API token comes from exactly one of these. TokenCommand is run
	// (e.g. [gh, auth, token]) and its output cached for a few minutes.
	TokenFile    string   `yaml:"token_file"`
	TokenCommand []string `yaml:"token_command"`
	// ImportCreatedAfter limits importing GitHub-born PRs to those opened
	// after this time, so turning the bridge on does not sweep up every
	// existing PR. Zero imports all.
	ImportCreatedAfter time.Time `yaml:"import_created_after"`
}

// Identity maps GitHub users to Gerrit accounts (design §7).
type Identity struct {
	// GitHubToGerrit maps a GitHub login to a Gerrit username.
	GitHubToGerrit map[string]string `yaml:"github_to_gerrit"`
}

// CI configures how Gerrit CI is mirrored to GitHub (design §6.5).
type CI struct {
	// TrustedGroup is the Gerrit group whose uploads CI builds
	// automatically. The bridge account must not be a member.
	TrustedGroup string `yaml:"trusted_group"`
	// Labels are voted by CI: shown on GitHub only as statuses, never in
	// the votes comment.
	Labels []string `yaml:"labels"`
	// MessagePattern matches CI messages; group 1 must be Started,
	// Succeeded or Failed and group 2 the build URL.
	MessagePattern string `yaml:"message_pattern"`
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
		CI: CI{
			TrustedGroup:   "Verified Users",
			Labels:         []string{"Verified"},
			MessagePattern: `Build (Started|Succeeded|Failed): (\S+)`,
		},
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
	req("gerrit.mirror_dir", c.Gerrit.MirrorDir)
	req("gerrit.ssh.address", c.Gerrit.SSH.Address)
	req("gerrit.ssh.user", c.Gerrit.SSH.User)
	req("gerrit.ssh.key_file", c.Gerrit.SSH.KeyFile)
	req("gerrit.ssh.known_hosts_file", c.Gerrit.SSH.KnownHostsFile)
	req("github.owner", c.GitHub.Owner)
	req("github.repo", c.GitHub.Repo)
	req("github.webhook_secret_file", c.GitHub.WebhookSecretFile)
	if (c.GitHub.TokenFile == "") == (len(c.GitHub.TokenCommand) == 0) {
		errs = append(errs, errors.New("exactly one of github.token_file and github.token_command is required"))
	}
	if _, err := regexp.Compile(c.CI.MessagePattern); err != nil {
		errs = append(errs, fmt.Errorf("ci.message_pattern: %w", err))
	}
	if c.ResyncInterval < 0 {
		errs = append(errs, errors.New("resync_interval must not be negative"))
	}
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
