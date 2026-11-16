// Command server runs the Model Relay gateway.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/config"
	"github.com/big91987/model-relay-by-dshsa/internal/gateway"
	"github.com/big91987/model-relay-by-dshsa/internal/secretbox"
	"github.com/big91987/model-relay-by-dshsa/internal/server"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(version)
	if err != nil {
		logger.Error("load config failed", "error", err)
		os.Exit(1)
	}
	if cfg.UsingDefaultSecret {
		logger.Warn("SECRET_KEY is not set; using an insecure development key. Set SECRET_KEY before exposing this instance.")
	}
	if cfg.TestMode {
		logger.Warn("TEST_MODE=1：已启用本地测试环境，初始管理员账号为 admin/admin。该模式仅用于本地验收，请勿在生产环境启用。",
			"host", cfg.Host)
		if !cfg.BindsLoopback() {
			logger.Warn("TEST_MODE=1 uses admin/admin but HOST binds beyond loopback; use HOST=127.0.0.1 unless remote access is explicitly required.",
				"host", cfg.Host)
		}
	} else if cfg.UsingDefaultPassword {
		logger.Warn("ADMIN_PASSWORD is not set; the default development password is active. Set ADMIN_PASSWORD before exposing this instance.",
			"username", cfg.AdminUsername)
	}

	var st *store.Store
	if cfg.UsingPostgres() {
		st, err = store.OpenPostgres(cfg.DatabaseURL)
		if err != nil {
			logger.Error("open postgres database failed", "error", err)
			os.Exit(1)
		}
	} else {
		st, err = store.Open(cfg.DBPath)
		if err != nil {
			logger.Error("open database failed", "error", err)
			os.Exit(1)
		}
	}
	defer st.Close()

	// A single-node gateway cannot still be serving requests from a previous
	// process. Pending reservations are therefore recovered at startup and
	// charged at their reserved upper bound, marked as unverified.
	recovered, err := st.RecoverPendingReservations()
	if err != nil {
		logger.Error("recover in-flight quota reservations failed", "error", err)
	}
	if len(recovered) > 0 {
		var amount float64
		for _, res := range recovered {
			amount += res.SettledCost
		}
		logger.Warn("recovered in-flight quota reservations after restart",
			"count", len(recovered), "charged", amount)
	}

	var created bool
	if cfg.TestMode {
		created, err = st.EnsureTestAdmin(cfg.AdminUsername, cfg.AdminPassword)
	} else {
		created, err = st.EnsureAdmin(cfg.AdminUsername, cfg.AdminPassword)
	}
	if err != nil {
		logger.Error("seed admin user failed", "error", err)
		os.Exit(1)
	}
	if created {
		logger.Info("created initial admin user", "username", cfg.AdminUsername)
	}

	secrets, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		logger.Error("initialize secret cipher failed", "error", err)
		os.Exit(1)
	}
	gw := gateway.New(cfg, st, secrets, logger)
	api := server.New(cfg, st, secrets, gw, logger)
	gw.ResumeVideoTasks()
	gw.ResumeSpeechJobs()
	gw.ResumeMusicTasks()

	httpServer := &http.Server{
		Addr:              cfg.ListenAddress(),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("model relay started", "addr", httpServer.Addr, "version", cfg.Version, "data_dir", cfg.DataDir, "database", st.Backend())
		errCh <- httpServer.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	case sig := <-stop:
		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}
}
