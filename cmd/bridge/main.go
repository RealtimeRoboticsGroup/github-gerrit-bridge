// Command bridge mirrors code review between Gerrit and GitHub.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/events"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/reconcile"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

func main() {
	configPath := flag.String("config", "", "Path to the YAML config file.")
	dryRun := flag.Bool("dry_run", true, "Log intended actions instead of performing them.")
	verbose := flag.Bool("v", false, "Log debug messages.")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := run(*configPath, *dryRun, logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, dryRun bool, logger *slog.Logger) error {
	if configPath == "" {
		return errors.New("--config is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
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
		HTTP:     &http.Client{Timeout: time.Minute},
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
		HTTP:   &http.Client{Timeout: time.Minute},
	}
	ghRepo, err := gh.GetRepo(ctx)
	if err != nil {
		return fmt.Errorf("checking GitHub access: %w", err)
	}
	logger.Info("github ok", "repo", ghRepo.FullName, "default_branch", ghRepo.DefaultBranch,
		"rate_remaining", gh.RateLimit().Remaining)

	store := &state.Store{Repo: mirror}
	if err := store.Fetch(ctx); err != nil {
		return fmt.Errorf("fetching refs/bridge/*: %w", err)
	}

	q := queue.New[reconcile.Key]()
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
	mux.Handle("POST /webhook", &github.WebhookHandler{
		Secret:  []byte(webhookSecret),
		Repo:    repo,
		Deliver: router.Delivery,
		Logger:  logger,
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		last := "never"
		if t := stream.LastEvent(); !t.IsZero() {
			last = t.UTC().Format(time.RFC3339)
		}
		rl := gh.RateLimit()
		fmt.Fprintf(w, "ok\nlast_gerrit_event %s\nqueue_depth %d\ngithub_rate_remaining %d\n", last, q.Len(), rl.Remaining)
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	logger.Info("gerrit-github-bridge starting",
		"dry_run", dryRun, "gerrit", cfg.Gerrit.URL, "project", cfg.Gerrit.Project,
		"github", repo, "listen", cfg.Listen)

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
		Identity:      cfg.Identity.GitHubToGerrit,
		Bridge:        self,
		DefaultBranch: ghRepo.DefaultBranch,
		CreatedAfter:  cfg.GitHub.ImportCreatedAfter,
		Pushers:       router.Pushers,
		DryRun:        dryRun,
		Logger:        logger,
	}
	comments := &reconcile.Comments{
		Project:       cfg.Gerrit.Project,
		Gerrit:        rest,
		GitHub:        gh,
		Store:         store,
		BridgeAccount: self.AccountID,
		CIMessage:     regexp.MustCompile(cfg.CI.MessagePattern),
		DryRun:        dryRun,
		Logger:        logger,
	}
	checks := &reconcile.Checks{
		Project:      cfg.Gerrit.Project,
		Gerrit:       rest,
		GitHub:       gh,
		Store:        store,
		CILabels:     cfg.CI.Labels,
		TrustedGroup: cfg.CI.TrustedGroup,
		CIMessage:    regexp.MustCompile(cfg.CI.MessagePattern),
		DryRun:       dryRun,
		Logger:       logger,
	}
	// Statuses need the patch set on GitHub; refresh them as soon as the
	// code mirror puts it there.
	code.OnExported = checks.Sync
	commands := &reconcile.Commands{
		GitHub:    gh,
		Submitter: reconcile.RunAsSubmitter{Gerrit: rest},
		Store:     store,
		Identity:  cfg.Identity.GitHubToGerrit,
		DryRun:    dryRun,
		Logger:    logger,
	}
	dispatch := &reconcile.Dispatcher{Code: code, Comments: comments, Commands: commands, Checks: checks, Store: store, Logger: logger}
	wg.Go(func() { reconcile.Work(ctx, q, dispatch, cfg.Workers, logger) })
	wg.Go(func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	})

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
	q.ShutDown()
	wg.Wait()
	return err
}
