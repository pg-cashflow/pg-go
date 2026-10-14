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

func TestValidateWorkerRoleConfig(t *testing.T) {
	validWorker := RoleInfo{
		RoleName:     "pgapp_worker",
		IsSuperuser:  false,
		BypassRLS:    false,
		IsTableOwner: false,
	}

	tests := []struct {
		name       string
		info       RoleInfo
		appEnv     string
		wantErr    bool
		wantErrSub string
	}{
		{
			name:    "valid pgapp_worker in production passes",
			info:    validWorker,
			appEnv:  "production",
			wantErr: false,
		},
		{
			name: "rejects wrong role name in production",
			info: RoleInfo{
				RoleName: "postgres",
			},
			appEnv:     "production",
			wantErr:    true,
			wantErrSub: "requires 'pgapp_worker'",
		},
		{
			name: "rejects pgapp_maint as worker in production",
			info: RoleInfo{
				RoleName: "pgapp_maint",
			},
			appEnv:     "production",
			wantErr:    true,
			wantErrSub: "requires 'pgapp_worker'",
		},
		{
			name: "rejects superuser in production",
			info: RoleInfo{
				RoleName:    "pgapp_worker",
				IsSuperuser: true,
			},
			appEnv:     "production",
			wantErr:    true,
			wantErrSub: "superuser",
		},
		{
			name: "rejects BYPASSRLS in production",
			info: RoleInfo{
				RoleName:  "pgapp_worker",
				BypassRLS: true,
			},
			appEnv:     "production",
			wantErr:    true,
			wantErrSub: "BYPASSRLS",
		},
		{
			name: "rejects table owner in production",
			info: RoleInfo{
				RoleName:     "pgapp_worker",
				IsTableOwner: true,
			},
			appEnv:     "production",
			wantErr:    true,
			wantErrSub: "table owner",
		},
		{
			name: "rejects pgapp_app in non-production",
			info: RoleInfo{
				RoleName: "pgapp_app",
			},
			appEnv:     "development",
			wantErr:    true,
			wantErrSub: "restricted role 'pgapp_app'",
		},
		{
			name: "allows postgres/pgapp_maint in development",
			info: RoleInfo{
				RoleName:    "postgres",
				IsSuperuser: true,
			},
			appEnv:  "development",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkerRoleConfig(tt.info, tt.appEnv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErrSub, err)
				}
			} else if err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
		})
	}
}
