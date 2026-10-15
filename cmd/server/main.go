package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/api"
	"github.com/ordinary-s/rc_ordinary-s/internal/config"
	"github.com/ordinary-s/rc_ordinary-s/internal/delivery"
	"github.com/ordinary-s/rc_ordinary-s/internal/mq"
	"github.com/ordinary-s/rc_ordinary-s/internal/notification"
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
	notificationService, err := notification.NewService(repository, cfg.AllowedTarget)
	if err != nil {
		logger.Error("destination allowlist configuration invalid", "error", err)
		stop()
		os.Exit(1)
	}
	engine := delivery.New(cfg, repository, logger)
	process := func(ctx context.Context, event mq.IngressEvent, reconsume int) error {
		expiresAt, err := time.Parse(time.RFC3339Nano, event.ExpiresAt)
		if err != nil {
			return &mq.ProtocolError{Reason: "MESSAGE_EXPIRY_INVALID"}
		}
		n, _, err := notificationService.Ingest(ctx, cfg.MQTrustedClientID, event.EventID, event.TraceID, expiresAt,
			notification.Input{DestinationID: event.DestinationID, Method: event.Method, Headers: event.Headers, Body: event.Body},
			cfg.MaxBodyBytes, cfg.MaxHeaderBytes, cfg.TaskTTL)
		if err != nil {
			return err
		}
		return engine.Process(ctx, n.ID, n.TraceID, reconsume)
	}
	if err := mqClient.Start(runCtx, process); err != nil {
		logger.Error("RocketMQ consumer startup failed", "error", err)
		os.Exit(1)
	}
	apiServer := api.New(cfg, repository, logger, func(ctx context.Context) error {
		return mqClient.Ready(ctx, cfg.BrokerAddress)
	})
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		logger.Error("TLS certificate loading failed", "error_type", fmt.Sprintf("%T", err))
		stop()
		_ = mqClient.Shutdown()
		os.Exit(1)
	}
	server := &http.Server{
		Addr: cfg.HTTPSAddr, Handler: apiServer.Handler(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}},
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("HTTPS server started", "addr", cfg.HTTPSAddr, "tls_min", "1.3")
		serverErrors <- server.ListenAndServeTLS("", "")
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
	if err := mqClient.Shutdown(); err != nil {
		logger.Error("RocketMQ shutdown failed", "error", err)
	}
	logger.Info("server stopped")
}
