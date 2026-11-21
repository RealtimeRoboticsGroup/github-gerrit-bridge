// Command bridge mirrors code review between Gerrit and GitHub.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
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
	"github.com/AustinSchuh/github-gerrit-bridge/internal/usertokens"
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
	_, err = os.Stdout.Write(gerritchecks.Plugin(cfg.CI.ChecksURL, cfg.Gerrit.Project, !cfg.GitHubOwned()))
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

	if err := checkSecrets(cfg, logger); err != nil {
		return err
	}
	gerritPassword, err := config.ReadSecret(cfg.Gerrit.HTTPPasswordFile)
	if err != nil {
		return err
	}
	webhookSecret, err := config.ReadWebhookSecret(cfg.GitHub.WebhookSecretFile)
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
	if err := checkCapabilities(ctx, cfg, rest); err != nil {
		return err
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
	if cfg.GitHubOwned() {
		for _, p := range mergeSettingProblems(ghRepo) {
			logger.Warn("GitHub's merge settings: "+p, "repo", ghRepo.FullName)
		}
	}

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
	router.FollowGitHub = cfg.GitHubOwned()
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
	if cfg.CI.ChecksURL != "" && cfg.GitHubOwned() {
		// GitHub's checks, for the bridge-checks plugin, shown only to
		// those signed in to Gerrit who may see the change: the browser
		// sends its Gerrit session, the bridge being on Gerrit's origin
		// (config checks it), and Gerrit is asked as them each time.
		mux.Handle("GET /checks/{change}/{patchset}", &gerritchecks.Handler{
			Project: cfg.Gerrit.Project, GitHub: reconcile.GitHubChecks{Store: store, Client: gh},
			Viewer:     func(session string) gerritchecks.ViewerChanges { return rest.Viewer(session) },
			Logger:     logger,
			ClientAddr: func(r *http.Request) string { return github.ClientAddr(r, cidrs(cfg.GitHub.WebhookTrustedProxies)) },
		})
	}
	// Internal: the deployment exposes only /webhook, /checks/ and
	// /github/link (the rate budget and queue depth would tell an
	// attacker when the bridge is starved).
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

	tally := reconcile.NewResyncTally()
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
				Resyncs: func() status.Resyncs[reconcile.Key] {
					rs := status.Resyncs[reconcile.Key]{Since: tally.Since()}
					for k, c := range tally.Counts() {
						rs.Counts = append(rs.Counts, status.ResyncCount{Reason: k.Reason, Key: k.Key, Runs: c.Runs, Caught: c.Caught, Failed: c.Failed})
					}
					slices.SortFunc(rs.Counts, func(a, b status.ResyncCount) int {
						return cmp.Or(strings.Compare(a.Reason, b.Reason), strings.Compare(a.Key, b.Key))
					})
					for _, c := range tally.Caught() {
						rs.Caught = append(rs.Caught, status.Caught[reconcile.Key]{Key: c.Key, At: c.At, Reason: c.Reason, Writes: c.Writes, N: c.N})
					}
					return rs
				},
				Facts: func() []status.Fact {
					rl := gh.RateLimit()
					budget := "not known yet"
					if rl.Limit > 0 {
						budget = fmt.Sprintf("%d of %d left, resets in %v", rl.Remaining, rl.Limit, time.Until(rl.Reset).Round(time.Second))
					}
					return []status.Fact{
						{Name: "Up", Value: time.Since(started).Round(time.Second).String()},
						{Name: "Project", Value: cfg.Gerrit.Project + " ↔ " + repo},
						{Name: "Workers", Value: fmt.Sprint(cfg.Workers, " (+1 for merges)")},
						{Name: "Gerrit events", Value: gerritEvents(stream.Counts(), stream.LastEvent(), time.Now())},
						{Name: "GitHub API", Value: budget},
					}
				},
			}}
	}

	logger.Info("gerrit-github-bridge starting",
		"dry_run", dryRun, "branch_owner", cfg.BranchOwner, "gerrit", cfg.Gerrit.URL, "project", cfg.Gerrit.Project,
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
		Project:        cfg.Gerrit.Project,
		Gerrit:         rest,
		GitHub:         gh,
		GitHubURL:      "https://github.com/" + repo + ".git",
		Mirror:         mirror,
		CommitterName:  cfg.GitHub.CommitterName,
		CommitterEmail: cfg.GitHub.CommitterEmail,
		Store:          store,
		DefaultBranch:  ghRepo.DefaultBranch,
		GitHubOwned:    cfg.GitHubOwned(),
		DryRun:         dryRun,
		Logger:         logger,
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
		GitHubOwned:   cfg.GitHubOwned(),
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
	gerritAccount := func(ctx context.Context, user string) (int, error) {
		var acct gerrit.AccountInfo
		err := rest.Do(ctx, http.MethodGet, "accounts/"+url.PathEscape(user), nil, &acct)
		return acct.AccountID, err
	}
	var merger reconcile.GitHubMerger
	if a := cfg.GitHub.UserApp; a != nil {
		secret, err := config.ReadSecret(a.ClientSecretFile)
		if err != nil {
			return err
		}
		key, err := usertokens.ReadKey(a.KeyFile)
		if err != nil {
			return err
		}
		linked := &usertokens.Store{Path: a.TokensFile, Key: key}
		if err := linked.Load(); err != nil {
			return err
		}
		app := &github.UserApp{ClientID: a.ClientID, ClientSecret: secret,
			HTTP: &http.Client{Timeout: time.Minute, Transport: &httplog.Transport{Name: "github-app", Logger: logger}, CheckRedirect: noRedirects}}
		tokens := &usertokens.Tokens{Store: linked, App: app, Logger: logger,
			Check: func(ctx context.Context, gh github.User, user string) bool {
				u, ok := users.GerritUserFor(ctx, gh)
				return ok && strings.EqualFold(u, user)
			}}
		comments.Authors = linkedAuthors{tokens: tokens, gh: gh}
		if cfg.GitHubOwned() {
			// Only they have Gerrit patch sets pushed to GitHub (design
			// §3.1).
			pub := publishable(tokens.Linked, gh.CanPush, logger)
			code.Publishable, code.Importer.Publishable = pub, pub
			code.LinkURL = strings.TrimSuffix(a.PublicURL, "/") + "/github/link"
			// /submit merges on GitHub, as whoever asked.
			merger = githubMerger{gerritUser: users.GerritUserFor, account: gerritAccount, tokenFor: tokens.For, source: tokens.Source, gh: gh}
		}
		// Public: people link their GitHub account here (design §7.2).
		(&usertokens.Linker{App: app, Tokens: tokens, PublicURL: a.PublicURL, GerritURL: cfg.Gerrit.URL, Logger: logger,
			GerritUser: users.GerritUserFor, GerritAccount: gerritAccount}).Register(mux)
		logger.Info("people can link their GitHub accounts", "url", strings.TrimSuffix(a.PublicURL, "/")+"/github/link", "linked", len(linked.All()))
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
		GitHubOwned:  cfg.GitHubOwned(),
		DryRun:       dryRun,
		Logger:       logger,
	}
	if builds != nil {
		checks.Builds = builds
	}
	// Statuses need the patch set on GitHub; refresh them as soon as the
	// code mirror puts it there.
	// The new head's statuses come from the change's comment sync, which
	// holds the branch only shared: posted inline, they held it alone,
	// most of a busy reconcile (about 0.8s each, several per PR).
	code.OnExported = func(_ context.Context, n int) error {
		q.Add(reconcile.Key{Comments: n})
		return nil
	}
	if err := checks.CheckUntrusted(ctx, self.AccountID); err != nil {
		return err
	}
	code.Trusted, code.Importer.Trusted = checks.IsTrusted, checks.IsTrusted
	code.Importer.ChangeIDKey = []byte("change-id\x00" + webhookSecret)
	// Anyone can register on Gerrit; each new PR costs ~15 GitHub writes,
	// and GitHub stalls a token past ~80 a minute or ~500 an hour.
	// Exempt from the budgets: whoever may have their code pushed to
	// GitHub in this mode (CI's trusted group, or linked writers), the
	// same rule as everywhere else (twelfth review).
	exempt := code.PushTrust()
	code.PRBudget = &reconcile.Budget{N: 20, Per: time.Hour, Trusted: exempt}
	comments.PostBudget = &reconcile.Budget{N: 120, Per: time.Hour, Trusted: exempt}
	checks.StatusBudget, checks.BridgeAccount = &reconcile.Budget{N: 120, Per: time.Hour, Trusted: exempt}, self.AccountID
	code.TextBudget = &reconcile.Budget{N: 60, Per: time.Hour, Trusted: exempt}
	code.LinksBudget = &reconcile.Budget{N: 12, Per: time.Hour} // per PR
	commands := &reconcile.Commands{
		GitHub:       gh,
		Submitter:    reconcile.RunAsSubmitter{Gerrit: rest},
		Store:        store,
		Identity:     users,
		BeforeSubmit: code.PrepareSubmit,
		Merger:       merger,
		GitHubOwned:  cfg.GitHubOwned(),
		LinkURL:      code.LinkURL,
		BridgeLogin:  login,
		DryRun:       dryRun,
		Logger:       logger,
	}
	// Keyed by GitHub user id: /submit is answered at most 20 times an
	// hour per commenter.
	commands.Replies = &reconcile.Budget{N: 20, Per: time.Hour}
	dispatch := &reconcile.Dispatcher{Code: code, Comments: comments, Commands: commands, Checks: checks, Store: store, Logger: logger,
		Enqueue: q.Add}
	wg.Go(func() { reconcile.Work(ctx, q, dispatch, cfg.Workers, tally, logger) })
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

// publishable is who may have Gerrit patch sets pushed to GitHub where
// GitHub owns the branches (Code.Publishable): a Gerrit account linked to
// a GitHub account (linked, which checks the link still stands) that may
// push to the repository (canPush). known is false if GitHub could not
// say.
func publishable(linked func(context.Context, int) (github.User, bool), canPush func(context.Context, github.User) (bool, error),
	logger *slog.Logger) func(context.Context, int) (bool, bool) {
	return func(ctx context.Context, account int) (bool, bool) {
		u, ok := linked(ctx, account)
		if !ok {
			return false, true
		}
		may, err := canPush(ctx, u)
		if err != nil {
			logger.Warn("asking GitHub whether a linked account may push", "account", account, "github_login", u.Login, "err", err)
			return false, false
		}
		return may, true
	}
}

// mergeSettingProblems says what in r's merge settings keeps GitHub's
// merges from closing their Gerrit changes as merged, where GitHub owns
// the branches (design §3.1): Gerrit closes a change when a commit with
// its Change-Id reaches its branch, and the bridge writes that trailer
// only into PR descriptions. A change whose merge commit lacks it is
// abandoned, with a note saying where it merged.
func mergeSettingProblems(r *github.Repository) []string {
	if r.AllowSquashMerge == nil && r.AllowMergeCommit == nil && r.AllowRebaseMerge == nil {
		return []string{"cannot read them (the bridge's token may not administer the repository): make the default message of squash " +
			"and merge commits the PR's title and description, or merged changes are abandoned in Gerrit rather than merged"}
	}
	var out []string
	if r.AllowSquashMerge != nil && *r.AllowSquashMerge && r.SquashMergeCommitMessage != "PR_BODY" {
		out = append(out, "squash commits' default message is not the PR's title and description: their changes are abandoned in Gerrit rather than merged")
	}
	if r.AllowMergeCommit != nil && *r.AllowMergeCommit && r.MergeCommitMessage != "PR_BODY" {
		out = append(out, "merge commits' default message is not the PR's title and description: their changes are abandoned in Gerrit rather than merged")
	}
	if r.AllowRebaseMerge != nil && *r.AllowRebaseMerge {
		out = append(out, "rebase merges are allowed, and keep the PR's commits' messages, without its Change-Id: their changes are abandoned in Gerrit rather than merged")
	}
	return out
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
	if a := cfg.GitHub.UserApp; a != nil {
		secrets = append(secrets, a.ClientSecretFile, a.KeyFile, a.TokensFile)
	}
	return secrets
}

// checkSecrets refuses keys readable by others: the peer key acts as
// anyone in Gerrit, the SSH key as the bridge, and the token key opens
// every linked user's GitHub token (tenth audit). Other secrets readable
// by others are warned about.
func checkSecrets(cfg *config.Config, logger *slog.Logger) error {
	keys := []string{cfg.Gerrit.SSH.KeyFile, cfg.Gerrit.SSH.PeerKeyFile}
	if a := cfg.GitHub.UserApp; a != nil {
		keys = append(keys, a.KeyFile)
	}
	for _, f := range secretFiles(cfg) {
		if f == "" || !config.LooseSecret(f) {
			continue
		}
		if slices.Contains(keys, f) {
			return fmt.Errorf("%s is readable by others: make it 0600", f)
		}
		logger.Warn("secret file is readable by others; make it 0600", "file", f)
	}
	return nil
}

// checkCapabilities refuses to start a bridge whose Gerrit account lacks a
// global capability its mode needs. Where GitHub owns the branches
// (design §3.1) that is Maintain Server: GitHub's merge button leaves the
// Change-Id out of the commit's last paragraph whenever the commits are
// signed off, and only check-and-fix then shows the change merged, not
// abandoned. Where Gerrit owns them it is never needed, nor asked about.
func checkCapabilities(ctx context.Context, cfg *config.Config, rest *gerrit.Client) error {
	if !cfg.GitHubOwned() {
		return nil
	}
	var caps map[string]any
	if err := rest.Do(ctx, http.MethodGet, "accounts/self/capabilities?q=maintainServer", nil, &caps); err != nil {
		return fmt.Errorf("reading the Gerrit account's capabilities: %w", err)
	}
	if _, ok := caps["maintainServer"]; !ok {
		return fmt.Errorf("gerrit account %s lacks the global Maintain Server capability, which branch_owner github needs "+
			"to show changes merged on GitHub as merged (design §3.1): grant it to the bridge's group in All-Projects", cfg.Gerrit.HTTPUser)
	}
	return nil
}

// githubMerger merges PRs on GitHub as whoever asked (/submit where GitHub
// owns the branches, design §3.1), with the token they linked: GitHub's
// rules for them decide, as for their own button press. Their GitHub
// account must still be the one their Gerrit account is matched to, and
// the one they linked.
type githubMerger struct {
	gerritUser func(context.Context, github.User) (string, bool)
	account    func(context.Context, string) (int, error)
	tokenFor   func(context.Context, int) (string, github.User, bool, error)
	source     func(int64, string) github.TokenSource
	gh         *github.Client // where, and how; and the bot's reads
}

func (m githubMerger) Merge(ctx context.Context, u github.User, pr int, head string, in github.MergeInput) (string, bool, error) {
	user, ok := m.gerritUser(ctx, u)
	if !ok {
		return "", false, nil
	}
	account, err := m.account(ctx, user)
	if err != nil {
		return "", true, err
	}
	tok, linked, ok, err := m.tokenFor(ctx, account)
	if err != nil {
		return "", true, err
	}
	if !ok || linked.ID != u.ID {
		return "", false, nil
	}
	in.SHA = head
	as := &github.Client{BaseURL: m.gh.BaseURL, Owner: m.gh.Owner, Repo: m.gh.Repo, Tokens: m.source(linked.ID, tok), HTTP: m.gh.HTTP}
	sha, err := as.MergePull(ctx, pr, in)
	return sha, true, err
}

// HasSubtree reports whether PR pr pulls in a subtree as git subtree
// --squash does: a root commit naming its directory, merged in. Its
// author can write any message, so a merge commit (which brings every
// commit's message, and a Change-Id footer there closes that change in
// Gerrit) is made only then, and only if no commit of it carries a
// Change-Id (twelfth review).
func (m githubMerger) HasSubtree(ctx context.Context, pr int) (bool, error) {
	commits, err := m.gh.PullCommits(ctx, pr)
	if err != nil || slices.ContainsFunc(commits, func(c github.PullCommit) bool { return changeIDLine.MatchString(c.Commit.Message) }) {
		return false, err
	}
	return slices.ContainsFunc(commits, func(c github.PullCommit) bool {
		return len(c.Parents) == 0 && subtreeDir.MatchString(c.Commit.Message)
	}), nil
}

var (
	// subtreeDir is git subtree's trailer naming the directory a commit
	// pulls a subtree into.
	subtreeDir = regexp.MustCompile(`(?m)^git-subtree-dir: \S`)
	// changeIDLine is a Change-Id line, as Gerrit would read one in a
	// footer (and more: anywhere in the message).
	changeIDLine = regexp.MustCompile(`(?mi)^\s*change-id:`)
)

// linkedAuthors posts linked users' content with their own token
// (design §7.2).
type linkedAuthors struct {
	tokens *usertokens.Tokens
	gh     *github.Client // for where, and how
}

func (a linkedAuthors) Linked(ctx context.Context, account int) (github.User, bool) {
	return a.tokens.Linked(ctx, account)
}

func (a linkedAuthors) Client(ctx context.Context, account int) (reconcile.UserGitHub, github.User, bool, error) {
	tok, gh, ok, err := a.tokens.For(ctx, account)
	if !ok || err != nil {
		return nil, github.User{}, false, err
	}
	return &github.Client{BaseURL: a.gh.BaseURL, Owner: a.gh.Owner, Repo: a.gh.Repo, Tokens: a.tokens.Source(gh.ID, tok), HTTP: a.gh.HTTP}, gh, true, nil
}

// gerritEvents sums up the Gerrit events seen since start, by type, and
// how long ago the last came.
func gerritEvents(counts map[string]int, last, now time.Time) string {
	total := 0
	types := slices.Collect(maps.Keys(counts))
	slices.SortFunc(types, func(a, b string) int { return cmp.Or(counts[b]-counts[a], strings.Compare(a, b)) })
	var each []string
	for _, t := range types {
		total += counts[t]
		each = append(each, fmt.Sprintf("%s %d", t, counts[t]))
	}
	events := "none since start"
	if total > 0 {
		events = fmt.Sprintf("%d since start (%s)", total, strings.Join(each, ", "))
	}
	if !last.IsZero() {
		events += ", the last " + now.Sub(last).Round(time.Second).String() + " ago"
	}
	return events
}
