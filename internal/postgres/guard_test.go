package postgres

import (
	"strings"
	"testing"
)

func TestValidateRolesConfig_Production(t *testing.T) {
	validMaint := RoleInfo{
		RoleName:     "pgapp_maint",
		IsSuperuser:  false,
		BypassRLS:    false,
		IsTableOwner: false,
	}
	validApp := RoleInfo{
		RoleName:     "pgapp_app",
		IsSuperuser:  false,
		BypassRLS:    false,
		IsTableOwner: false,
	}
	maintURL := "postgres://pgapp_maint:secret@localhost:5432/pgapp"
	appURL := "postgres://pgapp_app:secret@localhost:5432/pgapp"

	t.Run("valid configuration passes in production", func(t *testing.T) {
		err := ValidateRolesConfig(validMaint, validApp, maintURL, appURL, "production")
		if err != nil {
			t.Fatalf("expected valid config to pass, got: %v", err)
		}
	})

	t.Run("fails when URLs are identical", func(t *testing.T) {
		err := ValidateRolesConfig(validMaint, validApp, maintURL, maintURL, "production")
		if err == nil || !strings.Contains(err.Error(), "must differ in production") {
			t.Fatalf("expected URL difference error, got: %v", err)
		}
	})

	t.Run("fails when maintPool is not pgapp_maint", func(t *testing.T) {
		badMaint := validMaint
		badMaint.RoleName = "postgres"
		err := ValidateRolesConfig(badMaint, validApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "requires 'pgapp_maint'") {
			t.Fatalf("expected maint role error, got: %v", err)
		}
	})

	t.Run("fails when maintPool is superuser", func(t *testing.T) {
		badMaint := validMaint
		badMaint.IsSuperuser = true
		err := ValidateRolesConfig(badMaint, validApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "superuser") {
			t.Fatalf("expected superuser error, got: %v", err)
		}
	})

	t.Run("fails when maintPool has bypassrls", func(t *testing.T) {
		badMaint := validMaint
		badMaint.BypassRLS = true
		err := ValidateRolesConfig(badMaint, validApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "BYPASSRLS") {
			t.Fatalf("expected bypassrls error, got: %v", err)
		}
	})

	t.Run("fails when maintPool is table owner", func(t *testing.T) {
		badMaint := validMaint
		badMaint.IsTableOwner = true
		err := ValidateRolesConfig(badMaint, validApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "table owner") {
			t.Fatalf("expected table owner error, got: %v", err)
		}
	})

	t.Run("fails when appPool is not pgapp_app", func(t *testing.T) {
		badApp := validApp
		badApp.RoleName = "pgapp_maint"
		err := ValidateRolesConfig(validMaint, badApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "requires 'pgapp_app'") {
			t.Fatalf("expected app role error, got: %v", err)
		}
	})

	t.Run("fails when appPool is superuser", func(t *testing.T) {
		badApp := validApp
		badApp.IsSuperuser = true
		err := ValidateRolesConfig(validMaint, badApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "superuser") {
			t.Fatalf("expected superuser error, got: %v", err)
		}
	})

	t.Run("fails when appPool has bypassrls", func(t *testing.T) {
		badApp := validApp
		badApp.BypassRLS = true
		err := ValidateRolesConfig(validMaint, badApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "BYPASSRLS") {
			t.Fatalf("expected bypassrls error, got: %v", err)
		}
	})

	t.Run("fails when appPool is table owner", func(t *testing.T) {
		badApp := validApp
		badApp.IsTableOwner = true
		err := ValidateRolesConfig(validMaint, badApp, maintURL, appURL, "production")
		if err == nil || !strings.Contains(err.Error(), "table owner") {
			t.Fatalf("expected table owner error, got: %v", err)
		}
	})
}

func TestValidateRolesConfig_Development(t *testing.T) {
	devMaint := RoleInfo{RoleName: "postgres", IsSuperuser: true}
	devApp := RoleInfo{RoleName: "postgres", IsSuperuser: true}
	url := "postgres://postgres:secret@localhost:5432/pgapp"

	t.Run("shared superuser pool allowed in development", func(t *testing.T) {
		err := ValidateRolesConfig(devMaint, devApp, url, url, "development")
		if err != nil {
			t.Fatalf("expected dev config to pass, got: %v", err)
		}
	})

	t.Run("maintPool connected as pgapp_app rejected in development", func(t *testing.T) {
		badMaint := RoleInfo{RoleName: "pgapp_app"}
		err := ValidateRolesConfig(badMaint, devApp, url, url, "development")
		if err == nil || !strings.Contains(err.Error(), "restricted role 'pgapp_app'") {
			t.Fatalf("expected maint pgapp_app rejection, got: %v", err)
		}
	})
}
