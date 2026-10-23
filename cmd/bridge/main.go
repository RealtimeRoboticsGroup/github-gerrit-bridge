// Command bridge mirrors code review between Gerrit and GitHub.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/buildkite"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/events"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerritchecks"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/httplog"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/reconcile"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/status"
)

func main() {
	configPath := flag.String("config", "", "Path to the YAML config file.")
	dryRun := flag.Bool("dry_run", true, "Log intended actions instead of performing them.")
	verbose := flag.Bool("v", false, "Log debug messages.")
	checksPlugin := flag.Bool("checks_plugin", false, "Print the Gerrit plugin that shows CI in the Checks tab "+
		"(install it as $site/plugins/bridge-checks.js), and exit.")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *checksPlugin {
		if err := printChecksPlugin(*configPath); err != nil {
			logger.Error("exiting", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*configPath, *dryRun, logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func printChecksPlugin(configPath string) error {
	if configPath == "" {
		return errors.New("--config is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.CI.ChecksURL == "" {
		return errors.New("ci.checks_url is not set")
	}
	_, err = os.Stdout.Write(gerritchecks.Plugin(cfg.CI.ChecksURL, cfg.Gerrit.Project))
	return err
}

func run(configPath string, dryRun bool, logger *slog.Logger) error {
	if configPath == "" {
		return errors.New("--config is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	for _, f := range secretFiles(cfg) {
		if f != "" && config.LooseSecret(f) {
			logger.Warn("secret file is readable by others; make it 0600 (the peer key can act as anyone in Gerrit)", "file", f)
		}
	}
	gerritPassword, err := config.ReadSecret(cfg.Gerrit.HTTPPasswordFile)
	if err != nil {
		return err
	}
	webhookSecret, err := config.ReadSecret(cfg.GitHub.WebhookSecretFile)
	if err != nil {
		return err
	}
	signer, err := gerritssh.LoadSigner(cfg.Gerrit.SSH.KeyFile)
	if err != nil {
		return err
	}
	hostKeys, hostKeyAlgos, err := gerritssh.KnownHosts(cfg.Gerrit.SSH.KnownHostsFile, cfg.Gerrit.SSH.Address)
	if err != nil {
		return err
	}
	sshCfg := gerritssh.Config{
		Address:           cfg.Gerrit.SSH.Address,
		User:              cfg.Gerrit.SSH.User,
		Signer:            signer,
		HostKeyCallback:   hostKeys,
		HostKeyAlgorithms: hostKeyAlgos,
		KeepaliveInterval: cfg.Gerrit.SSH.KeepaliveInterval,
		KeepaliveCountMax: cfg.Gerrit.SSH.KeepaliveCountMax,
		Logger:            logger,
	}
	rest := &gerrit.Client{
		BaseURL:  cfg.Gerrit.URL,
		User:     cfg.Gerrit.HTTPUser,
		Password: gerritPassword,
		HTTP:     &http.Client{Timeout: time.Minute, Transport: &httplog.Transport{Name: "gerrit", Logger: logger}, CheckRedirect: noRedirects},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Gerrit only accepts commits (here, refs/bridge state) authored by
	// the pushing account, so use its real name and email.
	var self gerrit.AccountInfo
	if err := rest.Do(ctx, http.MethodGet, "accounts/self", nil, &self); err != nil {
		return fmt.Errorf("checking Gerrit REST access: %w", err)
	}
	if self.Email == "" {
		return fmt.Errorf("gerrit account %s has no preferred email; state commits need one", cfg.Gerrit.HTTPUser)
	}
	gitCfg, gitEnv := gitmirror.HTTPCredentials(cfg.Gerrit.URL, cfg.Gerrit.HTTPUser, gerritPassword)
	mirror := &gitmirror.Repo{
		Dir:    cfg.Gerrit.MirrorDir,
		Remote: cfg.Gerrit.URL + "/a/" + cfg.Gerrit.Project,
		// GitHub pushes get the token per command (it can rotate).
		Config: append(gitCfg, gitmirror.CredentialHelper("https://github.com")),
		Env:    gitEnv,
		Name:   self.Name,
		Email:  self.Email,
		Logger: logger,
	}
	if err := mirror.Init(ctx); err != nil {
		return err
	}
	var tokens github.TokenSource
	if cfg.GitHub.TokenFile != "" {
		tok, err := config.ReadSecret(cfg.GitHub.TokenFile)
		if err != nil {
			return err
		}
		tokens = github.StaticToken(tok)
	} else {
		tokens = &github.CommandToken{Command: cfg.GitHub.TokenCommand}
	}
	gh := &github.Client{
		Owner:  cfg.GitHub.Owner,
		Repo:   cfg.GitHub.Repo,
		Tokens: tokens,
		HTTP:   &http.Client{Timeout: time.Minute, Transport: &httplog.Transport{Name: "github", Logger: logger}, CheckRedirect: noRedirects},
	}
	ghRepo, err := gh.GetRepo(ctx)
	if err != nil {
		return fmt.Errorf("checking GitHub access: %w", err)
	}
	login, err := gh.Login(ctx)
	if err != nil {
		return fmt.Errorf("checking GitHub identity: %w", err)
	}
	logger.Info("github ok", "repo", ghRepo.FullName, "as", login, "default_branch", ghRepo.DefaultBranch,
		"rate_remaining", gh.RateLimit().Remaining)

	var builds *buildkite.Client
	if b := cfg.CI.Buildkite; b != nil {
		tok, err := config.ReadSecret(b.TokenFile)
		if err != nil {
			return err
		}
		builds = &buildkite.Client{BaseURL: b.APIURL, Org: b.Organization, Pipeline: b.Pipeline, Token: tok,
			HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirects}}
	}

	store := &state.Store{Repo: mirror}
	if err := store.Fetch(ctx); err != nil {
		return fmt.Errorf("fetching refs/bridge/*: %w", err)
	}

	q := queue.New[reconcile.Key]()
	// One key's events (a stranger commenting non-stop, on either side)
	// run it at most this often, so they cannot spend the whole GitHub
	// budget. Not a merge's: it must reach GitHub within replication's
	// delay (design §6.7).
	q.MinInterval = 10 * time.Second
	q.Throttled = reconcile.Throttled
	q.Urgent = reconcile.Urgent
	router := reconcile.NewRouter(cfg.Gerrit.Project, q, logger)
	router.Pushers = &reconcile.Pushers{}
	stream := &events.Stream{
		Source:   events.SSHSource(sshCfg, events.Types),
		Handler:  router,
		Logger:   logger,
		Watchdog: cfg.Gerrit.SSH.Watchdog,
	}
	repo := cfg.GitHub.Owner + "/" + cfg.GitHub.Repo

	mux := http.NewServeMux()
	cidrs := func(ns []string) []*net.IPNet {
		var out []*net.IPNet
		for _, n := range ns {
			_, ipn, _ := net.ParseCIDR(n) // validated by config
			out = append(out, ipn)
		}
		return out
	}
	mux.Handle("POST /webhook", &github.WebhookHandler{
		Secret:         []byte(webhookSecret),
		Repo:           repo,
		Deliver:        router.Delivery,
		Logger:         logger,
		Allowed:        cidrs(cfg.GitHub.WebhookAllowedNetworks),
		TrustedProxies: cidrs(cfg.GitHub.WebhookTrustedProxies),
	})
	if builds != nil {
		// Public, for the bridge-checks plugin in Gerrit users' browsers.
		origin := ""
		if u, err := url.Parse(cfg.Gerrit.URL); err == nil {
			origin = u.Scheme + "://" + u.Host
		}
		label := ""
		if len(cfg.CI.Labels) > 0 {
			label = cfg.CI.Labels[0]
		}
		mux.Handle("GET /checks/{change}/{patchset}", &gerritchecks.Handler{
			// As the bridge: AOS lets no anonymous user read changes. The
			// handler answers only for what the bridge publishes anyway.
			Project: cfg.Gerrit.Project, Gerrit: rest, Builds: builds, Label: label,
			AllowOrigin: origin, Logger: logger,
			ClientAddr: func(r *http.Request) string { return github.ClientAddr(r, cidrs(cfg.GitHub.WebhookTrustedProxies)) },
		})
	}
	// Internal: the deployment exposes only /webhook and /checks/ (the
	// rate budget and queue depth would tell an attacker when the bridge
	// is starved).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		last := "never"
		if t := stream.LastEvent(); !t.IsZero() {
			last = t.UTC().Format(time.RFC3339)
		}
		rl := gh.RateLimit()
		fmt.Fprintf(w, "ok\nlast_gerrit_event %s\nqueue_depth %d\ngithub_rate_remaining %d\n", last, q.Len(), rl.Remaining)
	})
	// Timeouts so slow senders cannot hold connections (and buffers) open.
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 64 << 10}

	var statusSrv *http.Server
	if cfg.StatusListen != "" {
		started := time.Now()
		statusSrv = &http.Server{Addr: cfg.StatusListen, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
			Handler: &status.Handler[reconcile.Key]{
				Queue: q,
				Name:  reconcile.Key.String,
				Link: func(k reconcile.Key) string {
					switch {
					case k.Comments != 0:
						return rest.ChangeURL(cfg.Gerrit.Project, k.Comments)
					case k.Merged != 0:
						return rest.ChangeURL(cfg.Gerrit.Project, k.Merged)
					case k.PR != 0:
						return fmt.Sprintf("https://github.com/%s/pull/%d", repo, k.PR)
					case k.PRComments != 0:
						return fmt.Sprintf("https://github.com/%s/pull/%d", repo, k.PRComments)
					}
					return ""
				},
				Facts: func() []status.Fact {
					last := "never"
					if t := stream.LastEvent(); !t.IsZero() {
						last = time.Since(t).Round(time.Second).String() + " ago"
					}
					rl := gh.RateLimit()
					budget := "not known yet"
					if rl.Limit > 0 {
						budget = fmt.Sprintf("%d of %d left, resets in %v", rl.Remaining, rl.Limit, time.Until(rl.Reset).Round(time.Second))
					}
					return []status.Fact{
						{Name: "Up", Value: time.Since(started).Round(time.Second).String()},
						{Name: "Project", Value: cfg.Gerrit.Project + " ↔ " + repo},
						{Name: "Workers", Value: fmt.Sprint(cfg.Workers, " (+1 for merges)")},
						{Name: "Last Gerrit event", Value: last},
						{Name: "GitHub API", Value: budget},
						{Name: "Dry run", Value: fmt.Sprint(dryRun)},
					}
				},
			}}
	}

	logger.Info("gerrit-github-bridge starting",
		"dry_run", dryRun, "gerrit", cfg.Gerrit.URL, "project", cfg.Gerrit.Project,
		"github", repo, "listen", cfg.Listen, "status_listen", cfg.StatusListen)

	var wg sync.WaitGroup
	errc := make(chan error, 1)
	wg.Go(func() { stream.Run(ctx) })
	wg.Go(func() {
		router.RunResync(ctx, reconcile.BranchLister{Gerrit: rest, Project: cfg.Gerrit.Project, Store: store})
	})
	if cfg.ResyncInterval > 0 {
		wg.Go(func() {
			t := time.NewTicker(cfg.ResyncInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					router.Resync("periodic")
				}
			}
		})
	}
	code := &reconcile.Code{
		Project:       cfg.Gerrit.Project,
		Gerrit:        rest,
		GitHub:        gh,
		GitHubURL:     "https://github.com/" + repo + ".git",
		Mirror:        mirror,
		Store:         store,
		DefaultBranch: ghRepo.DefaultBranch,
		DryRun:        dryRun,
		Logger:        logger,
	}
	// Never act as the bridge itself, a service account (CI, say) or an
	// administrator by matching usernames, even if someone registers that
	// login on GitHub; nor as anyone for the bridge's own GitHub login.
	users := &reconcile.Users{Overrides: cfg.Identity.GitHubToGerrit, Logger: logger,
		Never: []string{self.Username}, NeverGroups: cfg.Identity.NeverGroups, NeverLogins: []string{login},
		// A Gerrit account follows the GitHub account (numeric id) it was
		// first matched to, not whoever holds the login later.
		Pins: store, GitHub: gh, DryRun: dryRun}
	if cfg.Identity.MatchesUsernames() {
		users.Gerrit = rest
	}
	users.Active = rest
	if err := users.CheckGroups(ctx); err != nil {
		return err
	}
	code.Identity, code.BridgeLogin, code.BridgeUser, code.BridgeAccount = users, login, self.Username, self.AccountID
	var uploader reconcile.Uploader = reconcile.HTTPUploader{Mirror: mirror}
	if cfg.Gerrit.SSH.PeerKeyFile != "" {
		uploader = reconcile.SuexecUploader{
			Mirror: mirror, Address: cfg.Gerrit.SSH.Address, Project: cfg.Gerrit.Project,
			PeerKeyFile: cfg.Gerrit.SSH.PeerKeyFile, KnownHostsFile: cfg.Gerrit.SSH.KnownHostsFile,
			Bridge: uploader,
		}
	}
	code.Importer = &reconcile.Importer{
		Project:       cfg.Gerrit.Project,
		Gerrit:        rest,
		GitHub:        gh,
		GitHubURL:     code.GitHubURL,
		Mirror:        mirror,
		Store:         store,
		Uploader:      uploader,
		Identity:      users,
		Bridge:        self,
		DefaultBranch: ghRepo.DefaultBranch,
		CreatedAfter:  cfg.GitHub.ImportCreatedAfter,
		Pushers:       router.Pushers,
		DryRun:        dryRun,
		Logger:        logger,
	}
	comments := &reconcile.Comments{
		BridgeLogin:   login,
		Project:       cfg.Gerrit.Project,
		Gerrit:        rest,
		GitHub:        gh,
		Store:         store,
		BridgeAccount: self.AccountID,
		// GitHub users' comments and votes are posted as their Gerrit
		// accounts (needs the Run As capability).
		Users:     users,
		GerritAs:  func(user string) reconcile.GerritAPI { return rest.As(user) },
		CIMessage: regexp.MustCompile(cfg.CI.MessagePattern),
		DryRun:    dryRun,
		Logger:    logger,
	}
	checks := &reconcile.Checks{
		Project:      cfg.Gerrit.Project,
		Gerrit:       rest,
		GitHub:       gh,
		Store:        store,
		CILabels:     cfg.CI.Labels,
		TrustedGroup: cfg.CI.TrustedGroup,
		CIMessage:    regexp.MustCompile(cfg.CI.MessagePattern),
		CIAccount:    cfg.CI.Account,
		DryRun:       dryRun,
		Logger:       logger,
	}
	if builds != nil {
		checks.Builds = builds
	}
	// Statuses need the patch set on GitHub; refresh them as soon as the
	// code mirror puts it there.
	code.OnExported = checks.Sync
	if err := checks.CheckUntrusted(ctx, self.AccountID); err != nil {
		return err
	}
	code.Trusted, code.Importer.Trusted = checks.IsTrusted, checks.IsTrusted
	code.Importer.ChangeIDKey = []byte("change-id\x00" + webhookSecret)
	// Anyone can register on Gerrit; each new PR costs ~15 GitHub writes,
	// and GitHub stalls a token past ~80 a minute or ~500 an hour.
	code.PRBudget = &reconcile.Budget{N: 20, Per: time.Hour, Trusted: checks.IsTrusted}
	comments.PostBudget = &reconcile.Budget{N: 120, Per: time.Hour, Trusted: checks.IsTrusted}
	checks.StatusBudget, checks.BridgeAccount = &reconcile.Budget{N: 120, Per: time.Hour, Trusted: checks.IsTrusted}, self.AccountID
	code.TextBudget = &reconcile.Budget{N: 60, Per: time.Hour, Trusted: checks.IsTrusted}
	code.LinksBudget = &reconcile.Budget{N: 12, Per: time.Hour} // per PR
	commands := &reconcile.Commands{
		GitHub:       gh,
		Submitter:    reconcile.RunAsSubmitter{Gerrit: rest},
		Store:        store,
		Identity:     users,
		BeforeSubmit: code.PrepareSubmit,
		BridgeLogin:  login,
		DryRun:       dryRun,
		Logger:       logger,
	}
	// Keyed by GitHub user id: /submit is answered at most 20 times an
	// hour per commenter.
	commands.Replies = &reconcile.Budget{N: 20, Per: time.Hour}
	dispatch := &reconcile.Dispatcher{Code: code, Comments: comments, Commands: commands, Checks: checks, Store: store, Logger: logger,
		Enqueue: q.Add}
	wg.Go(func() { reconcile.Work(ctx, q, dispatch, cfg.Workers, logger) })
	wg.Go(func() {
		// Stop if the bridge's account joins CI's trusted group.
		if err := checks.WatchUntrusted(ctx, self.AccountID, time.Minute); err != nil {
			select {
			case errc <- err:
			default:
			}
		}
	})
	wg.Go(func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	})
	if statusSrv != nil {
		wg.Go(func() {
			if err := statusSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				select {
				case errc <- err:
				default:
				}
			}
		})
	}

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	logger.Info("shutting down")
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	if statusSrv != nil {
		statusSrv.Shutdown(shutdownCtx)
	}
	q.ShutDown()
	wg.Wait()
	return err
}

// noRedirects keeps the bridge's credentials going only where it sent
// them: a redirect is answered as it is, and fails as a non-2xx.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// secretFiles are cfg's secret and key files.
func secretFiles(cfg *config.Config) []string {
	secrets := []string{cfg.Gerrit.HTTPPasswordFile, cfg.GitHub.WebhookSecretFile, cfg.GitHub.TokenFile,
		cfg.Gerrit.SSH.KeyFile, cfg.Gerrit.SSH.PeerKeyFile}
	if cfg.CI.Buildkite != nil {
		secrets = append(secrets, cfg.CI.Buildkite.TokenFile)
	}
	return secrets
}
