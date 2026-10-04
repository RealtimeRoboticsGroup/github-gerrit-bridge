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
	if !dryRun {
		return errors.New("only --dry_run is implemented so far")
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

	gitCfg, gitEnv := gitmirror.HTTPCredentials(cfg.Gerrit.HTTPUser, gerritPassword)
	mirror := &gitmirror.Repo{
		Dir:    cfg.Gerrit.MirrorDir,
		Remote: cfg.Gerrit.URL + "/a/" + cfg.Gerrit.Project,
		Config: gitCfg,
		Env:    gitEnv,
		Name:   "GitHub Bridge",
		Email:  cfg.Gerrit.HTTPUser + "@bridge.invalid",
	}
	if err := mirror.Init(ctx); err != nil {
		return err
	}
	store := &state.Store{Repo: mirror}
	if err := store.Fetch(ctx); err != nil {
		return fmt.Errorf("fetching refs/bridge/*: %w", err)
	}

	q := queue.New[reconcile.Key]()
	router := reconcile.NewRouter(cfg.Gerrit.Project, q, logger)
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
		fmt.Fprintf(w, "ok\nlast_gerrit_event %s\nqueue_depth %d\n", last, q.Len())
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	logger.Info("gerrit-github-bridge starting",
		"dry_run", dryRun, "gerrit", cfg.Gerrit.URL, "project", cfg.Gerrit.Project,
		"github", repo, "listen", cfg.Listen)

	var wg sync.WaitGroup
	errc := make(chan error, 1)
	wg.Go(func() { stream.Run(ctx) })
	wg.Go(func() { router.RunResync(ctx, openChanges{rest, cfg.Gerrit.Project}) })
	wg.Go(func() { reconcile.Work(ctx, q, reconcile.DryRun{Store: store, Logger: logger}, cfg.Workers, logger) })
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

// openChanges lists the project's open changes for a full resync.
type openChanges struct {
	c       *gerrit.Client
	project string
}

func (o openChanges) OpenChanges(ctx context.Context) ([]int, error) {
	changes, err := o.c.QueryChanges(ctx, "project:"+gerrit.QueryString(o.project)+" status:open")
	if err != nil {
		return nil, err
	}
	nums := make([]int, len(changes))
	for i, ch := range changes {
		nums[i] = ch.Number
	}
	return nums, nil
}
