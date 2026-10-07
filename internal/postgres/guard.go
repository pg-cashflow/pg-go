package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RoleInfo captures PostgreSQL security attributes for a connected role.
type RoleInfo struct {
	RoleName     string
	IsSuperuser  bool
	BypassRLS    bool
	IsTableOwner bool
}

// QueryRoleInfo inspects the connection role, superuser status, bypassrls status,
// and whether the role owns any tables in the public schema.
func QueryRoleInfo(ctx context.Context, pool *pgxpool.Pool) (RoleInfo, error) {
	var info RoleInfo
	query := `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls,
		       EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user)
		FROM pg_roles r
		WHERE r.rolname = current_user`
	err := pool.QueryRow(ctx, query).Scan(
		&info.RoleName,
		&info.IsSuperuser,
		&info.BypassRLS,
		&info.IsTableOwner,
	)
	if err != nil {
		return RoleInfo{}, fmt.Errorf("inspect connection role: %w", err)
	}
	return info, nil
}

// ValidateStartupRoles enforces security boundaries for dual connection pools.
// In production:
// 1. DATABASE_URL and DATABASE_MAINT_URL must differ.
// 2. maintPool role must be 'pgapp_maint', NOSUPERUSER, NOBYPASSRLS, and not table owner.
// 3. appPool role must be 'pgapp_app', NOSUPERUSER, NOBYPASSRLS, and not table owner.
// In non-production:
// maintPool role cannot be 'pgapp_app' (to avoid 0-row silently failing background tasks).
func ValidateStartupRoles(ctx context.Context, maintPool, appPool *pgxpool.Pool, maintURL, appURL, appEnv string) error {
	maintInfo, err := QueryRoleInfo(ctx, maintPool)
	if err != nil {
		return fmt.Errorf("check maint db role: %w", err)
	}

	appInfo := maintInfo
	if maintPool != appPool {
		appInfo, err = QueryRoleInfo(ctx, appPool)
		if err != nil {
			return fmt.Errorf("check app db role: %w", err)
		}
	}

	return ValidateRolesConfig(maintInfo, appInfo, maintURL, appURL, appEnv)
}

// ValidateRolesConfig validates inspected roles against security requirements.
func ValidateRolesConfig(maintInfo, appInfo RoleInfo, maintURL, appURL, appEnv string) error {
	if appEnv == "production" {
		if maintURL == appURL {
			return errors.New("DATABASE_URL and DATABASE_MAINT_URL must differ in production")
		}

		// Maintenance pool checks
		if maintInfo.RoleName != "pgapp_maint" {
			return fmt.Errorf("maintPool connected as '%s'; production maintenance pool requires 'pgapp_maint'", maintInfo.RoleName)
		}
		if maintInfo.IsSuperuser {
			return errors.New("maintPool role has superuser privileges")
		}
		if maintInfo.BypassRLS {
			return errors.New("maintPool role has BYPASSRLS privilege")
		}
		if maintInfo.IsTableOwner {
			return errors.New("maintPool role owns tables in public schema; table owner sees zero rows under forced RLS")
		}

		// Application pool checks
		if appInfo.RoleName != "pgapp_app" {
			return fmt.Errorf("appPool connected as '%s'; production app pool requires 'pgapp_app'", appInfo.RoleName)
		}
		if appInfo.IsSuperuser {
			return errors.New("appPool role has superuser privileges")
		}
		if appInfo.BypassRLS {
			return errors.New("appPool role has BYPASSRLS privilege")
		}
		if appInfo.IsTableOwner {
			return errors.New("appPool role owns tables in public schema; table owner sees zero rows under forced RLS")
		}

		return nil
	}

	// Non-production checks
	if maintInfo.RoleName == "pgapp_app" {
		return errors.New("maintPool connected as restricted role 'pgapp_app'; maintenance operations require unscoped role")
	}

	return nil
}
