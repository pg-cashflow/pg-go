//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type TokenOutput struct {
	OwnerToken  string `json:"owner_token"`
	TenantToken string `json:"tenant_token"`
	PropertyID  string `json:"property_id"`
	DueID       string `json:"due_id,omitempty"`
	DueCode     string `json:"due_code,omitempty"`
}

func main() {
	_ = godotenv.Load()

	flag.Parse()
	dsn := os.Getenv("DATABASE_URL")
	for _, arg := range flag.Args() {
		if arg != "" {
			dsn = arg
			break
		}
	}
	if dsn == "" {
		dsn = "postgres://postgres:password@localhost:5432/pg_test?sslmode=disable"
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "ci_test_jwt_secret_must_be_32_characters_long!!"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(ctx)

	// 1. Get or create Property
	var propertyID uuid.UUID
	err = conn.QueryRow(ctx, `SELECT id FROM properties LIMIT 1`).Scan(&propertyID)
	if err != nil {
		propertyID = uuid.New()
		_, err = conn.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode)
			VALUES ($1, 'LoadTest PG', '123 Test St', '+919999999999', 'loadtest@upi', 'Load Owner', 'owner@loadtest.com', 'LOAD01', 'cashfree');
		`, propertyID)
		if err != nil {
			log.Fatalf("Failed to create loadtest property: %v", err)
		}
	}

	// 2. Get or create Owner User
	var ownerUser domain.User
	var ownerPropID *uuid.UUID
	err = conn.QueryRow(ctx, `
		SELECT id, phone, role, property_id, token_version 
		FROM users 
		WHERE role = 'owner' AND property_id = $1 
		LIMIT 1
	`, propertyID).Scan(&ownerUser.ID, &ownerUser.Phone, &ownerUser.Role, &ownerPropID, &ownerUser.TokenVersion)
	if err != nil {
		ownerUser = domain.User{
			ID:           uuid.New(),
			Phone:        "+919999999999",
			Role:         domain.RoleOwner,
			PropertyID:   &propertyID,
			TokenVersion: 1,
		}
		_, err = conn.Exec(ctx, `
			INSERT INTO users (id, phone, role, property_id, token_version)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (phone) DO UPDATE SET property_id = EXCLUDED.property_id, role = EXCLUDED.role
			RETURNING id;
		`, ownerUser.ID, ownerUser.Phone, ownerUser.Role, ownerUser.PropertyID, ownerUser.TokenVersion)
		if err != nil {
			log.Fatalf("Failed to create loadtest owner user: %v", err)
		}
	} else {
		ownerUser.PropertyID = ownerPropID
	}

	// 3. Get or create Tenant & User
	var tenantID uuid.UUID
	var tenantPhone string
	err = conn.QueryRow(ctx, `
		SELECT id, phone FROM tenants WHERE property_id = $1 AND status = 'active' LIMIT 1
	`, propertyID).Scan(&tenantID, &tenantPhone)
	if err != nil {
		tenantID = uuid.New()
		tenantPhone = "+919888888888"
		room := "101"
		_, err = conn.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
			VALUES ($1, $2, 'Load Tenant', $3, $4, 1500000, 5, 'active')
			ON CONFLICT (phone) DO NOTHING;
		`, tenantID, propertyID, tenantPhone, room)
		if err != nil {
			log.Fatalf("Failed to create loadtest tenant: %v", err)
		}
	}

	var tenantUser domain.User
	var tTenantID, tPropID *uuid.UUID
	err = conn.QueryRow(ctx, `
		SELECT id, phone, role, tenant_id, property_id, token_version 
		FROM users 
		WHERE role = 'tenant' AND tenant_id = $1 
		LIMIT 1
	`, tenantID).Scan(&tenantUser.ID, &tenantUser.Phone, &tenantUser.Role, &tTenantID, &tPropID, &tenantUser.TokenVersion)
	if err != nil {
		tenantUser = domain.User{
			ID:           uuid.New(),
			Phone:        tenantPhone,
			Role:         domain.RoleTenant,
			TenantID:     &tenantID,
			PropertyID:   &propertyID,
			TokenVersion: 1,
		}
		_, err = conn.Exec(ctx, `
			INSERT INTO users (id, phone, role, tenant_id, property_id, token_version)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (phone) DO UPDATE SET tenant_id = EXCLUDED.tenant_id, property_id = EXCLUDED.property_id
			RETURNING id;
		`, tenantUser.ID, tenantUser.Phone, tenantUser.Role, tenantUser.TenantID, tenantUser.PropertyID, tenantUser.TokenVersion)
		if err != nil {
			log.Fatalf("Failed to create loadtest tenant user: %v", err)
		}
	} else {
		tenantUser.TenantID = tTenantID
		tenantUser.PropertyID = tPropID
	}

	// 4. Get or create pending Due
	var dueID uuid.UUID
	var dueCode string
	err = conn.QueryRow(ctx, `
		SELECT id, due_code FROM dues WHERE tenant_id = $1 AND status = 'pending' LIMIT 1
	`, tenantID).Scan(&dueID, &dueCode)
	if err != nil {
		dueID = uuid.New()
		dueCode = fmt.Sprintf("LT%06d", time.Now().Unix()%1000000)
		_, _ = conn.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, status)
			VALUES ($1, $2, $3, $4, 'rent', 1500000, 1500000, 'pending')
			ON CONFLICT DO NOTHING;
		`, dueID, dueCode, tenantID, propertyID)
	}

	// 5. Mint JWT Tokens
	ownerToken, err := auth.IssueToken(jwtSecret, &ownerUser)
	if err != nil {
		log.Fatalf("Failed to mint owner token: %v", err)
	}

	tenantToken, err := auth.IssueToken(jwtSecret, &tenantUser)
	if err != nil {
		log.Fatalf("Failed to mint tenant token: %v", err)
	}

	out := TokenOutput{
		OwnerToken:  ownerToken,
		TenantToken: tenantToken,
		PropertyID:  propertyID.String(),
		DueID:       dueID.String(),
		DueCode:     dueCode,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
