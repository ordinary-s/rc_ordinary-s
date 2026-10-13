package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/api"
	"github.com/ordinary-s/rc_ordinary-s/internal/config"
	"github.com/ordinary-s/rc_ordinary-s/internal/delivery"
	"github.com/ordinary-s/rc_ordinary-s/internal/mq"
	"github.com/ordinary-s/rc_ordinary-s/internal/notification"
	"github.com/ordinary-s/rc_ordinary-s/internal/outbox"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()
	repository, err := store.Open(startupCtx, cfg.DBDSN)
	if err != nil {
		logger.Error("mysql startup failed", "error", err)
		os.Exit(1)
	}
	defer repository.Close()

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mqClient, err := mq.New(cfg, logger)
	if err != nil {
		logger.Error("RocketMQ client initialization failed", "error", err)
		os.Exit(1)
	}
	engine := delivery.New(cfg, repository, logger)
	if err := mqClient.Start(runCtx, engine.Process); err != nil {
		logger.Error("RocketMQ startup failed", "error", err)
		os.Exit(1)
	}
	publisher := outbox.New(repository, mqClient, logger, cfg.OutboxPoll)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		publisher.Run(runCtx)
	}()

	notificationService, err := notification.NewService(repository, cfg.AllowedTarget)
	if err != nil {
		logger.Error("destination allowlist configuration invalid", "error", err)
		stop()
		_ = mqClient.Shutdown()
		os.Exit(1)
	}
	apiServer := api.New(cfg, repository, notificationService, logger, func(ctx context.Context) error {
		return mqClient.Ready(ctx, cfg.BrokerAddress)
	})
	server := &http.Server{
		Addr: cfg.HTTPAddr, Handler: apiServer.Handler(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("HTTP server started", "addr", cfg.HTTPAddr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-runCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped unexpectedly", "error", err)
			stop()
		}
	}
	stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP graceful shutdown failed", "error", err)
	}
	workers.Wait()
	if err := mqClient.Shutdown(); err != nil {
		logger.Error("RocketMQ shutdown failed", "error", err)
	}
	logger.Info("server stopped")
}
