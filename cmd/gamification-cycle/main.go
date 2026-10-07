package main

import (
	"context"
	"log"
	"log/slog"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseMaintURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	logger := slog.Default()
	propertyRepo := postgres.NewPropertyRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	gamificationRepo := postgres.NewGamificationRepo(pool)
	pub := events.NewPostgresPublisher(eventRepo)

	svc := gamification.NewService(gamificationRepo, tenantRepo, dueRepo, pub, gamification.NewBlobStore())

	properties, err := propertyRepo.List(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// Month to process: previous calendar month
	now := time.Now().UTC()
	prevMonth := now.AddDate(0, -1, 0).Format("2006-01")
	logger.Info("starting monthly gamification cycle", "month", prevMonth, "properties", len(properties))

	for _, p := range properties {
		logger.Info("processing property gamification cycle", "property_id", p.ID, "name", p.Name)

		// 1. Floor cleanliness multiplier
		awarded, err := svc.EvaluateFloorMultipliers(ctx, p.ID, prevMonth)
		if err != nil {
			logger.Error("failed to evaluate floor multipliers", "property_id", p.ID, "err", err)
		} else {
			logger.Info("floor multipliers evaluated", "property_id", p.ID, "floors_awarded", len(awarded))
		}

		// 2. Zero-incident floor bonuses
		floors, err := gamificationRepo.ListFloors(ctx, p.ID)
		if err == nil {
			for _, fl := range floors {
				// Query if any safety violations occurred on this floor
				tenants, err := tenantRepo.ListByProperty(ctx, p.ID)
				if err != nil {
					continue
				}

				incidentCount := 0
				var floorTenants []domain.Tenant
				for _, t := range tenants {
					if t.RoomID == nil {
						continue
					}
					rm, _ := gamificationRepo.GetRoomByID(ctx, *t.RoomID)
					if rm != nil && rm.FloorId == fl.ID {
						floorTenants = append(floorTenants, t)
						viols, _ := gamificationRepo.ListViolationsByTenant(ctx, t.ID)
						for _, v := range viols {
							if v.Severity == "safety" && v.CreatedAt.Format("2006-01") == prevMonth {
								incidentCount++
							}
						}
					}
				}

				if incidentCount == 0 && len(floorTenants) > 0 {
					refType := "zero_incident"
					refID := fl.ID.String() + "-" + prevMonth
					for _, t := range floorTenants {
						_, _ = svc.AwardPoints(ctx, t.ID, "NO_INCIDENT_MONTH", &refType, &refID, nil)
					}
					logger.Info("zero-incident bonus awarded to floor", "floor_id", fl.ID, "tenants", len(floorTenants))
				}
			}
		}
	}

	logger.Info("monthly gamification cycle completed successfully")
}
