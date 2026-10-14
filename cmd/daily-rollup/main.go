package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func main() {
	flag.Parse()
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseWorkerURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := postgres.ValidateWorkerRole(ctx, pool, cfg.AppEnv); err != nil {
		log.Fatal(err)
	}

	propertyRepo := postgres.NewPropertyRepo(pool)
	financeRepo := postgres.NewFinanceRepo(pool)

	job := jobs.NewDailyRollupJob(propertyRepo, financeRepo, financeRepo, slog.Default())

	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	nowIST := time.Now().In(loc)

	release, acquired, err := jobs.AcquireJobLock(ctx, pool, "daily_rollup")
	if err != nil {
		log.Fatalf("failed to acquire job lock: %v", err)
	}
	if !acquired {
		slog.Default().Info("another daily rollup job is currently running, exiting gracefully")
		os.Exit(0)
	}
	defer release(context.Background())

	if err := job.Run(ctx, nowIST); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
