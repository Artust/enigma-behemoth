// Command server runs the Behemoth World Boss Event Service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"behemoth/internal/api"
	"behemoth/internal/boss"
	"behemoth/internal/config"
	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Bound startup dependency dialing so we fail fast if infra is unavailable.
	dialCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pg, err := store.NewPostgresStore(dialCtx, cfg.PostgresDSN, int32(cfg.PGMaxConns))
	if err != nil {
		return err
	}
	defer pg.Close()

	rdb, err := store.NewRedisStore(dialCtx, cfg.RedisAddr, cfg.RedisPassword)
	if err != nil {
		return err
	}
	defer rdb.Close()

	writer := store.NewWriter(pg, store.WriterConfig{
		QueueSize:   cfg.WriterQueueSize,
		MaxBatch:    cfg.BatchMaxSize,
		MaxWait:     cfg.BatchMaxWait,
		TxTimeout:   cfg.BatchTxTimeout,
		Concurrency: cfg.WriterConcurrency,
	}, log)
	writer.Start()

	rehydr := recovery.New(rdb, pg, log)
	svc := boss.New(rdb, pg, writer, rehydr, cfg.MaxDamagePerHit, log)

	// Rehydrate the cache from durable state BEFORE serving traffic.
	var ready atomic.Bool
	if err := rehydr.RehydrateAll(dialCtx); err != nil {
		return err
	}
	ready.Store(true)

	metrics := api.NewMetrics()
	server := api.NewServer(svc, &ready, log, metrics)
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Serve until a signal arrives.
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Graceful shutdown: stop accepting, let in-flight handlers finish (they may
	// be blocked on the writer), THEN flush the writer so no acked-but-unwritten
	// events remain, then close datastores.
	ready.Store(false)
	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	writer.Stop()
	log.Info("shutdown complete")
	return nil
}
