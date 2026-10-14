package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/app"
	"github.com/pg-cashflow/pg-go/internal/config"
)

func main() {
	_ = godotenv.Load()
	flag.Parse()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatal(err)
	}
	for _, w := range cfg.ValidateWarnings() {
		slog.Warn(w)
	}
	if cfg.WebhookSecretSource != "" {
		slog.Info("cashfree pg webhook secret configured", "webhook_secret_source", cfg.WebhookSecretSource)
	}
	if cfg.AppEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	ctx := context.Background()
	built, err := app.Build(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer built.Cleanup()

	bgCtx, stopBg := context.WithCancel(ctx)
	defer stopBg()
	built.RunBackground(bgCtx)

	logger := slog.Default()
	srv := &http.Server{
		Addr:              built.HTTPAddr,
		Handler:           built.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", built.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopBg()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
