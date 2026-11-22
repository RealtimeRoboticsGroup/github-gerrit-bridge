// Package config loads the bridge's YAML configuration.
//
// The file holds deployment settings only: where Gerrit and GitHub are and
// where to find credentials. Secrets are never inline; each one is read from
// the file named by a *_file field. Mapping data that changes at run time
// (identity map, label overrides) lives in refs/bridge/config instead.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/ldap"
)

// Config is the top-level configuration.
type Config struct {
	// Listen is the address of the HTTP server for webhooks, /checks and
	// /healthz. Default 127.0.0.1:8080: put a TLS proxy in front.
	Listen string `yaml:"listen"`
	// StatusListen, if set, serves a status page (the work queue, the
	// last Gerrit event, GitHub's rate budget) at / on its own listener,
	// which must be a loopback address: it is for operators, never the
	// internet. Default off.
	StatusListen string `yaml:"status_listen"`
	// StatusResync adds a "Force resync" button to the status page: a
	// POST from the page itself queues a full resync. Default off;
	// needs status_listen.
	StatusResync bool `yaml:"status_resync"`
	Workers      int  `yaml:"workers"`
	// ResyncInterval queues a full resync periodically, so missed events
	// (or webhooks that cannot reach the bridge, or were refused while it
	// was busy) heal. Default 30m; zero disables.
	ResyncInterval time.Duration `yaml:"resync_interval"`
	// BranchOwner is the side whose target branches (main, releases) are
	// the truth, and where changes are merged: "gerrit" (the default:
	// Gerrit submits, and its branches are replicated to GitHub) or
	// "github" (PRs are merged on GitHub, and the bridge copies GitHub's
	// branches into Gerrit, which marks the changes merged; design §3.1).
	BranchOwner string   `yaml:"branch_owner"`
	Gerrit      Gerrit   `yaml:"gerrit"`
	GitHub      GitHub   `yaml:"github"`
	Identity    Identity `yaml:"identity"`
	CI          CI       `yaml:"ci"`
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
	// WebhookAllowedNetworks, if set, are the only addresses (CIDR) webhook
	// deliveries are accepted from: GitHub's "hooks" ranges from
	// https://api.github.com/meta. Only useful if the bridge sees GitHub's
	// addresses, not a proxy's.
	WebhookAllowedNetworks []string `yaml:"webhook_allowed_networks"`
	// WebhookTrustedProxies are the reverse proxies (CIDR) in front of the
	// bridge: their X-Forwarded-For says who sent a delivery, for the
	// allowlist and the per-address limits. Have them buffer request
	// bodies, so slow senders tie up the proxy, not the bridge.
	WebhookTrustedProxies []string `yaml:"webhook_trusted_proxies"`
	// The API token comes from exactly one of these. TokenCommand is run
	// (e.g. [gh, auth, token]) and its output cached for a few minutes.
	TokenFile    string   `yaml:"token_file"`
	TokenCommand []string `yaml:"token_command"`
	// ImportCreatedAfter limits importing GitHub-born PRs to those opened
	// after this time, so turning the bridge on does not sweep up every
	// existing PR. Zero imports all.
	ImportCreatedAfter time.Time `yaml:"import_created_after"`
	// CommitterName and CommitterEmail are the committer of the commits
	// the bridge writes for GitHub ("Patchset N"). Default: the
	// bridge's Gerrit account.
	CommitterName  string `yaml:"committer_name"`
	CommitterEmail string `yaml:"committer_email"`
	// UserApp, if set, lets people link their GitHub account at
	// /github/link, so their Gerrit comments, replies and Code-Review
	// votes are posted on GitHub as them (design §7.2).
	UserApp *UserApp `yaml:"user_app"`
}

// UserApp is the GitHub App people authorize to post as them: Pull
// requests and Issues read & write, installed on the repository.
type UserApp struct {
	ClientID         string `yaml:"client_id"`
	ClientSecretFile string `yaml:"client_secret_file"`
	// PublicURL is the bridge as browsers reach it (https, or a loopback
	// address): PublicURL/github/link/callback must be one of the App's
	// callback URLs.
	PublicURL string `yaml:"public_url"`
	// TokensFile keeps the linked people's tokens, encrypted with the key
	// in KeyFile (32 bytes, or 64 hex digits: openssl rand -hex 32). Keep
	// the key apart from the tokens' backups.
	TokensFile string `yaml:"tokens_file"`
	KeyFile    string `yaml:"key_file"`
}

// Identity maps GitHub users to Gerrit accounts (design §7).
type Identity struct {
	// GitHubToGerrit maps a GitHub login to a Gerrit username, overriding
	// MatchUsernames.
	GitHubToGerrit map[string]string `yaml:"github_to_gerrit"`
	// MatchUsernames maps a GitHub login to the active Gerrit account with
	// the same username, which is how Gerrit's github plugin names the
	// accounts it creates. Default true; nil means unset.
	MatchUsernames *bool `yaml:"match_usernames"`
	// NeverGroups are Gerrit groups whose members MatchUsernames never
	// matches: service accounts, and administrators, whose GitHub logins
	// must be listed in GitHubToGerrit by hand. The bridge must be able to
	// see each one (Gerrit hides groups otherwise, e.g. Administrators
	// unless "visible to all"); it will not start otherwise.
	NeverGroups []string `yaml:"never_groups"`
	// LDAP, if set, is a directory of GitHub accounts (design §7), and
	// then the only mapping: by numeric GitHub id, as trusted as an
	// override, and whoever it does not list is nobody (GitHubToGerrit
	// and MatchUsernames may not be set with it). Off by default.
	LDAP *LDAP `yaml:"ldap"`
}

// LDAP is a directory saying whose GitHub account is whose: entries of
// ObjectClass under BaseDN, each a Gerrit username (uid) with a
// githubLogin and a githubId, written only by its administrators.
type LDAP struct {
	// URL is ldaps://host[:port]; binds are anonymous.
	URL string `yaml:"url"`
	// CAFile is the PEM certificates to trust for its certificate (the
	// system's if empty). Verification is never turned off.
	CAFile string `yaml:"ca_file"`
	BaseDN string `yaml:"base_dn"`
	// ObjectClass is the entries' auxiliary class (githubAccount).
	ObjectClass string `yaml:"object_class"`
	// Refresh is how often it is read whole (5m): someone removed stops
	// being matched within it. A read that fails, or none for two
	// intervals, matches nobody by it.
	Refresh time.Duration `yaml:"refresh_interval"`
}

// GitHubOwned reports whether GitHub owns the target branches.
func (c *Config) GitHubOwned() bool { return c.BranchOwner == OwnerGitHub }

// The sides BranchOwner names.
const (
	OwnerGerrit = "gerrit"
	OwnerGitHub = "github"
)

// MatchesUsernames reports whether MatchUsernames is on: the default,
// but never with LDAP, which is then the only mapping.
func (i Identity) MatchesUsernames() bool {
	return i.LDAP == nil && (i.MatchUsernames == nil || *i.MatchUsernames)
}

// CI configures how Gerrit CI is mirrored to GitHub (design §6.5).
type CI struct {
	// TrustedGroup is the Gerrit group whose uploads CI builds
	// automatically. The bridge account must not be a member.
	TrustedGroup string `yaml:"trusted_group"`
	// Labels are voted by CI: shown on GitHub only as statuses, never in
	// the votes comment.
	Labels []string `yaml:"labels"`
	// Account is CI's Gerrit username. Without Buildkite, only its
	// messages count as CI runs (anyone can post "Build Succeeded:
	// <url>"); unset, none do.
	Account string `yaml:"account"`
	// MessagePattern matches CI messages; group 1 must be Started,
	// Succeeded or Failed and group 2 the build URL. Used only without
	// Buildkite.
	MessagePattern string `yaml:"message_pattern"`
	// Buildkite, if set, is where CI results are read from: for the
	// gerrit/Verified status on GitHub and for Gerrit's Checks tab.
	Buildkite *Buildkite `yaml:"buildkite"`
	// ChecksURL is the bridge's URL as Gerrit users' browsers reach it.
	// The bridge serves /checks there for the bridge-checks Gerrit
	// plugin (bridge --checks_plugin writes the plugin): Buildkite's
	// builds, or, where GitHub owns the branches, GitHub's checks, which
	// it shows only to those signed in to Gerrit who may see the change.
	// For that, it must be on Gerrit's own origin (a path under
	// gerrit.url's host), so browsers send their Gerrit session along.
	ChecksURL string `yaml:"checks_url"`
}

// Buildkite is the pipeline gerrit-buildkite triggers.
type Buildkite struct {
	// APIURL defaults to https://api.buildkite.com.
	APIURL       string `yaml:"api_url"`
	Organization string `yaml:"organization"`
	Pipeline     string `yaml:"pipeline"`
	// TokenFile holds an API token with read_builds.
	TokenFile string `yaml:"token_file"`
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
		Listen:         "127.0.0.1:8080",
		BranchOwner:    OwnerGerrit,
		Workers:        4,
		ResyncInterval: 30 * time.Minute,
		Gerrit: Gerrit{SSH: SSH{
			KeepaliveInterval: 15 * time.Second,
			KeepaliveCountMax: 3,
		}},
		// Identity.NeverGroups' default, as main set it: before decoding,
		// so an empty never_groups: (every entry commented out) is none
		// (fifteenth review); left as it is, it is dropped beside
		// identity.ldap, where setting it is an error.
		Identity: Identity{NeverGroups: defaultNeverGroups},
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
	if g := c.Identity.NeverGroups; len(g) > 0 && &g[0] == &defaultNeverGroups[0] {
		// Not in the file: the default, but none beside identity.ldap.
		c.Identity.NeverGroups = slices.Clone(g)
		if c.Identity.LDAP != nil {
			c.Identity.NeverGroups = nil
		}
	}
	if l := c.Identity.LDAP; l != nil {
		l.ObjectClass = cmp.Or(l.ObjectClass, "githubAccount")
		if l.Refresh == 0 {
			l.Refresh = 5 * time.Minute
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// defaultNeverGroups is identity.never_groups when the file does not
// say (decoding replaces it, so it is told apart by its backing array).
var defaultNeverGroups = []string{"Service Users", "Administrators"}

func (c *Config) validate() error {
	var errs []error
	req := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	req("gerrit.url", c.Gerrit.URL)
	if c.StatusListen != "" {
		if host, _, err := net.SplitHostPort(c.StatusListen); err != nil || !loopback(host) {
			errs = append(errs, fmt.Errorf("status_listen %q must be a loopback address and port, e.g. 127.0.0.1:8081: the status page is not for the internet", c.StatusListen))
		}
	}
	if c.StatusResync && c.StatusListen == "" {
		errs = append(errs, fmt.Errorf("status_resync needs status_listen: the button is on the status page"))
	}
	req("gerrit.project", c.Gerrit.Project)
	if l := c.Identity.LDAP; l != nil {
		// The directory is the only mapping: everyone must be in it, so
		// nothing else may say who someone is (the owner's call).
		if len(c.Identity.GitHubToGerrit) > 0 {
			errs = append(errs, errors.New("identity.github_to_gerrit with identity.ldap: the directory is the only mapping; list them in LDAP"))
		}
		if c.Identity.MatchUsernames != nil && *c.Identity.MatchUsernames {
			errs = append(errs, errors.New("identity.match_usernames with identity.ldap: the directory is the only mapping"))
		}
		if len(c.Identity.NeverGroups) > 0 {
			// It would do nothing: an entry names its account outright,
			// administrators included (design §7).
			errs = append(errs, errors.New("identity.never_groups with identity.ldap: directory entries name their accounts outright, never_groups would not apply; leave someone out of LDAP instead"))
		}
		req("identity.ldap.url", l.URL)
		req("identity.ldap.base_dn", l.BaseDN)
		if l.URL != "" {
			if _, err := ldap.ParseServer(l.URL, ""); err != nil { // its CA file is read at startup
				errs = append(errs, fmt.Errorf("identity.ldap: %w", err))
			}
		}
		if l.Refresh < 10*time.Second || l.Refresh > time.Hour {
			// Someone removed from LDAP is acted as until the next read.
			errs = append(errs, fmt.Errorf("identity.ldap.refresh_interval %s: from 10s to 1h", l.Refresh))
		}
	}
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
	for k, v := range map[string]string{"github.committer_name": c.GitHub.CommitterName, "github.committer_email": c.GitHub.CommitterEmail} {
		if strings.ContainsAny(v, "<>\n\x00") {
			errs = append(errs, fmt.Errorf("%s: no <, > or newlines", k))
		}
	}
	// CI's trusted group: the bridge must check it is not in it (design
	// §6.5), so there is no turning that off.
	req("ci.trusted_group", c.CI.TrustedGroup)
	if _, err := regexp.Compile(c.CI.MessagePattern); err != nil {
		errs = append(errs, fmt.Errorf("ci.message_pattern: %w", err))
	}
	if b := c.CI.Buildkite; b != nil {
		req("ci.buildkite.organization", b.Organization)
		req("ci.buildkite.pipeline", b.Pipeline)
		req("ci.buildkite.token_file", b.TokenFile)
		// Its token goes with every request.
		if b.APIURL != "" {
			if err := secureURL(b.APIURL); err != nil {
				errs = append(errs, fmt.Errorf("ci.buildkite.api_url %w", err))
			}
		}
	}
	if a := c.GitHub.UserApp; a != nil {
		req("github.user_app.client_id", a.ClientID)
		req("github.user_app.client_secret_file", a.ClientSecretFile)
		req("github.user_app.public_url", a.PublicURL)
		req("github.user_app.tokens_file", a.TokensFile)
		req("github.user_app.key_file", a.KeyFile)
		// The callback carries codes that become tokens.
		if a.PublicURL != "" {
			if err := secureURL(a.PublicURL); err != nil {
				errs = append(errs, fmt.Errorf("github.user_app.public_url %w", err))
			}
		}
	}
	switch c.BranchOwner {
	case OwnerGerrit:
		if c.CI.ChecksURL != "" && c.CI.Buildkite == nil {
			errs = append(errs, errors.New("ci.checks_url needs ci.buildkite"))
		}
	case OwnerGitHub:
		// Gerrit-born changes reach GitHub only from people whose linked
		// GitHub accounts may push there themselves (design §3.1).
		if c.GitHub.UserApp == nil {
			errs = append(errs, errors.New("branch_owner github needs github.user_app: Gerrit-born changes are mirrored only for linked GitHub accounts"))
		}
		if c.CI.Buildkite != nil {
			errs = append(errs, errors.New("ci.buildkite is for Gerrit-owned branches: with branch_owner github, CI is GitHub's"))
		}
		if c.CI.ChecksURL != "" && c.Gerrit.URL != "" && !underGerrit(c.CI.ChecksURL, c.Gerrit.URL) {
			errs = append(errs, fmt.Errorf("ci.checks_url %q must be a path under gerrit.url, so browsers send their Gerrit session (its cookie is for Gerrit's path): with branch_owner github, checks are shown only to who may see the change", c.CI.ChecksURL))
		}
	default:
		errs = append(errs, fmt.Errorf("branch_owner %q: must be %q or %q", c.BranchOwner, OwnerGerrit, OwnerGitHub))
	}
	// Browsers fetch from it, with the plugin every Gerrit user loads.
	if c.CI.ChecksURL != "" {
		if err := secureURL(c.CI.ChecksURL); err != nil {
			errs = append(errs, fmt.Errorf("ci.checks_url %w", err))
		}
	}
	for _, n := range c.GitHub.WebhookAllowedNetworks {
		if _, _, err := net.ParseCIDR(n); err != nil {
			errs = append(errs, fmt.Errorf("github.webhook_allowed_networks: %w", err))
		}
	}
	for _, n := range c.GitHub.WebhookTrustedProxies {
		if _, _, err := net.ParseCIDR(n); err != nil {
			errs = append(errs, fmt.Errorf("github.webhook_trusted_proxies: %w", err))
		}
	}
	// Basic auth goes with every request.
	if c.Gerrit.URL != "" {
		if err := secureURL(c.Gerrit.URL); err != nil {
			errs = append(errs, fmt.Errorf("gerrit.url %w", err))
		}
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

// secureURL checks a base URL credentials are sent to: https (or a
// loopback address), and only a base. Gerrit's is in every link the
// bridge posts publicly, so a password in it would be too (ninth audit).
func secureURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return errors.New("is not a URL")
	case u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())):
		return errors.New("must be https (or a loopback address)")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("must not have credentials, a query or a fragment")
	}
	return nil
}

// underGerrit reports whether URL u is a path under Gerrit's base URL
// gerrit: the same scheme and host (with port), and a path that is
// Gerrit's or below it, so a browser sends Gerrit's session cookie, which
// is for Gerrit's path, there too ("/gerrit-bridge" is not under
// "/gerrit").
func underGerrit(u, gerrit string) bool {
	uu, err1 := url.Parse(u)
	ug, err2 := url.Parse(gerrit)
	if err1 != nil || err2 != nil || uu.Host == "" || !strings.EqualFold(uu.Scheme, ug.Scheme) || !strings.EqualFold(uu.Host, ug.Host) {
		return false
	}
	base := strings.TrimSuffix(ug.Path, "/")
	return base == "" || uu.Path == base || strings.HasPrefix(uu.Path, base+"/")
}

// loopback reports whether host is a loopback address. Literals only: a
// name ("localhost") is looked up, and may not be loopback where
// /etc/hosts lacks it (ninth audit).
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LooseSecret reports whether the file at path (a secret or key) can be
// read by users other than its owner. The bridge warns rather than
// refuses: Kubernetes mounts secrets 0644 unless told otherwise.
func LooseSecret(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Perm()&0o077 != 0
}

// MinWebhookSecret is the shortest webhook secret accepted. It also keys
// the Change-Ids derived for PRs, which anyone can see, so a guessable
// one could be found offline and webhooks forged with it (ninth audit).
const MinWebhookSecret = 32

// ReadWebhookSecret reads the webhook secret from path: at least
// MinWebhookSecret characters (openssl rand -hex 32).
func ReadWebhookSecret(path string) (string, error) {
	s, err := ReadSecret(path)
	if err == nil && len(s) < MinWebhookSecret {
		err = fmt.Errorf("%s: the webhook secret must be at least %d characters (openssl rand -hex 32)", path, MinWebhookSecret)
	}
	return s, err
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
