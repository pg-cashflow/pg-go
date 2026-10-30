package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// ValidateForRealDeployment enforces security invariants that must hold
// whenever real credentials are present â€” regardless of APP_ENV.
//
// Rationale (ADR-2, Batch 1): checks keyed on APP_ENV == "production" create
// a class of bugs where staging/dev servers with real credentials bypass guards.
// The correct invariant is "real credentials present", not "env label says production".
func (c *Config) ValidateForRealDeployment() error {
	var errs []string

	// --- HMAC/JWT secret quality ---
	type secretCheck struct {
		name string
		val  string
	}
	secrets := []secretCheck{
		{"JWT_SECRET", c.JWTSecret},
		{"OTP_HMAC_SECRET", c.OTPHMACSecret},
		{"MAGIC_LINK_HMAC_SECRET", c.MagicLinkHMACSecret},
	}
	if strings.TrimSpace(c.KYCIdentitySecret) != "" {
		secrets = append(secrets, secretCheck{"KYC_IDENTITY_SECRET", c.KYCIdentitySecret})
	}
	if strings.TrimSpace(c.PayoutEncryptionSecret) != "" {
		secrets = append(secrets, secretCheck{"PAYOUT_ENCRYPTION_SECRET", c.PayoutEncryptionSecret})
	}
	if strings.TrimSpace(c.PayoutExportChecksumSecret) != "" {
		secrets = append(secrets, secretCheck{"PAYOUT_CHECKSUM_SECRET", c.PayoutExportChecksumSecret})
	}
	if strings.TrimSpace(c.CashfreePayoutWebhookSecret) != "" {
		secrets = append(secrets, secretCheck{"CF_PAYOUT_WEBHOOK_SECRET", c.CashfreePayoutWebhookSecret})
	}
	if strings.TrimSpace(c.MetricsSecret) != "" {
		secrets = append(secrets, secretCheck{"METRICS_SECRET", c.MetricsSecret})
	}
	for ver, sec := range c.PayoutEncryptionKeys {
		secrets = append(secrets, secretCheck{fmt.Sprintf("PAYOUT_ENCRYPTION_KEYS[%d]", ver), sec})
	}
	for ver, sec := range c.KYCIdentitySecrets {
		secrets = append(secrets, secretCheck{fmt.Sprintf("KYC_IDENTITY_SECRETS[%d]", ver), sec})
	}
	for _, s := range secrets {
		if strings.HasPrefix(s.val, "change-me") {
			errs = append(errs, fmt.Sprintf("%s must not be a placeholder value (starts with 'change-me')", s.name))
		} else if len(s.val) < 32 && s.name != "METRICS_SECRET" && s.name != "CF_PAYOUT_WEBHOOK_SECRET" {
			errs = append(errs, fmt.Sprintf("%s must be at least 32 characters (got %d)", s.name, len(s.val)))
		} else if len(s.val) < 16 && s.name == "METRICS_SECRET" {
			errs = append(errs, fmt.Sprintf("%s must be at least 16 characters (got %d)", s.name, len(s.val)))
		}
	}

	// Invariant: key separation â€” distinct components must not share HMAC keys
	if c.JWTSecret != "" && c.OTPHMACSecret != "" && c.JWTSecret == c.OTPHMACSecret {
		errs = append(errs, "JWT_SECRET and OTP_HMAC_SECRET must be distinct secrets")
	}
	if c.JWTSecret != "" && c.MagicLinkHMACSecret != "" && c.JWTSecret == c.MagicLinkHMACSecret {
		errs = append(errs, "JWT_SECRET and MAGIC_LINK_HMAC_SECRET must be distinct secrets")
	}
	if c.OTPHMACSecret != "" && c.MagicLinkHMACSecret != "" && c.OTPHMACSecret == c.MagicLinkHMACSecret {
		errs = append(errs, "OTP_HMAC_SECRET and MAGIC_LINK_HMAC_SECRET must be distinct secrets")
	}

	// Invariant: no two versioned KYC identity secrets may share the same value.
	// A copy-paste error (same secret under two version numbers) defeats rotation.
	if len(c.KYCIdentitySecrets) > 1 {
		seen := make(map[string]int16, len(c.KYCIdentitySecrets))
		for ver, sec := range c.KYCIdentitySecrets {
			if prev, dup := seen[sec]; dup {
				errs = append(errs, fmt.Sprintf(
					"KYC_IDENTITY_SECRETS[%d] and KYC_IDENTITY_SECRETS[%d] must be distinct secrets (rotation requires unique keys per version)",
					prev, ver,
				))
			} else {
				seen[sec] = ver
			}
		}
	}

	// --- Cashfree PG and KYC key pairs must be fully provided if either is set ---
	pgAppID := strings.TrimSpace(c.CashfreePGAppID)
	if pgAppID == "" {
		pgAppID = strings.TrimSpace(c.CashfreeAppID)
	}
	pgSecret := strings.TrimSpace(c.CashfreePGSecretKey)
	if pgSecret == "" {
		pgSecret = strings.TrimSpace(c.CashfreeSecretKey)
	}
	if (pgAppID != "" && pgSecret == "") || (pgAppID == "" && pgSecret != "") {
		errs = append(errs, "both CASHFREE_PG_APP_ID and CASHFREE_PG_SECRET_KEY must be provided")
	}
	kycAppID := strings.TrimSpace(c.CashfreeKYCAppID)
	kycSecret := strings.TrimSpace(c.CashfreeKYCSecretKey)
	if (kycAppID != "" && kycSecret == "") || (kycAppID == "" && kycSecret != "") {
		errs = append(errs, "both CASHFREE_KYC_APP_ID and CASHFREE_KYC_SECRET_KEY must be provided")
	}

	// --- Cashfree Payout key pairs, fundsource, and webhook secret ---
	payoutClientID := strings.TrimSpace(c.CashfreePayoutClientID)
	payoutClientSecret := strings.TrimSpace(c.CashfreePayoutClientSecret)
	if (payoutClientID != "" && payoutClientSecret == "") || (payoutClientID == "" && payoutClientSecret != "") {
		errs = append(errs, "both CF_PAYOUT_CLIENT_ID and CF_PAYOUT_CLIENT_SECRET must be provided")
	}
	if payoutClientID != "" {
		if strings.TrimSpace(c.PayoutEncryptionSecret) == "" {
			errs = append(errs, "PAYOUT_ENCRYPTION_SECRET is required whenever CF_PAYOUT_CLIENT_ID is set (must not fall back to JWT_SECRET)")
		}
		if strings.TrimSpace(c.PayoutExportChecksumSecret) == "" {
			errs = append(errs, "PAYOUT_CHECKSUM_SECRET is required whenever CF_PAYOUT_CLIENT_ID is set (must not fall back to JWT_SECRET)")
		}
		if strings.TrimSpace(c.CashfreePayoutWebhookSecret) == "" {
			errs = append(errs, "CF_PAYOUT_WEBHOOK_SECRET is required whenever CF_PAYOUT_CLIENT_ID is set (any environment)")
		} else if strings.HasPrefix(c.CashfreePayoutWebhookSecret, "change-me") {
			errs = append(errs, "CF_PAYOUT_WEBHOOK_SECRET must not be a placeholder value (starts with 'change-me')")
		}
		if strings.TrimSpace(c.CashfreePayoutFundsourceID) == "" {
			errs = append(errs, "CF_PAYOUT_FUNDSOURCE_ID is required whenever CF_PAYOUT_CLIENT_ID is set (Cashfree Transfers V2 requires debit fund source)")
		}
		authMode := strings.ToLower(strings.TrimSpace(c.CashfreePayoutAuthMode))
		if authMode == "" {
			errs = append(errs, "CF_PAYOUT_AUTH_MODE is required whenever CF_PAYOUT_CLIENT_ID is set (must be 'signature' or 'ip')")
		} else if authMode != "signature" && authMode != "ip" {
			errs = append(errs, fmt.Sprintf("CF_PAYOUT_AUTH_MODE must be either 'signature' or 'ip' (got %q)", c.CashfreePayoutAuthMode))
		} else if authMode == "signature" {
			if strings.TrimSpace(c.CashfreePayoutPublicKeyPEM) == "" {
				errs = append(errs, "CF_PAYOUT_PUBLIC_KEY is required when CF_PAYOUT_AUTH_MODE is 'signature'")
			} else {
				if _, err := cashfree.ParseRSAPublicKeyOrCertificate([]byte(c.CashfreePayoutPublicKeyPEM)); err != nil {
					errs = append(errs, fmt.Sprintf("CF_PAYOUT_PUBLIC_KEY must be a valid PEM-encoded RSA public key or certificate: %v", err))
				}
			}
		}
	}

	// PG app id and secret pairing is checked above.
	// Load() copies CASHFREE_PG_SECRET_KEY into CashfreeWebhookSecret when no override is set.
	// Do not require a separate webhook secret here (ADR-031).
	cashfreeEnabled := pgAppID != "" || pgSecret != "" || payoutClientID != ""

	if c.AppEnv == "production" {
		if strings.TrimSpace(c.MetricsSecret) == "" {
			errs = append(errs, "METRICS_SECRET is required in production")
		}
	}

	// --- Frontend / Public App URL validation (Finding 5) ---
	frontendURL := strings.TrimSpace(c.FrontendURL)
	if c.AppEnv == "production" {
		if frontendURL == "" {
			errs = append(errs, "PUBLIC_APP_URL (or FRONTEND_URL) is required in production")
		} else {
			u, err := url.ParseRequestURI(frontendURL)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				errs = append(errs, fmt.Sprintf("PUBLIC_APP_URL (or FRONTEND_URL) must be a valid absolute HTTPS URL in production (got %q)", c.FrontendURL))
			}
		}
	} else if frontendURL != "" {
		u, err := url.ParseRequestURI(frontendURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Sprintf("PUBLIC_APP_URL (or FRONTEND_URL) must be a valid absolute HTTP or HTTPS URL (got %q)", c.FrontendURL))
		}
	}

	// --- Admin email required for critical operator alerts (outbox dead-letters, financial desynchronization) ---
	if (c.AppEnv == "production" || cashfreeEnabled) && strings.TrimSpace(c.AdminEmail) == "" {
		errs = append(errs, "ADMIN_EMAIL (or SMTP_FROM) is required for critical operator alerts when in production or when payment gateways are configured")
	}

	// --- AUTO_MIGRATE safety ---
	if (os.Getenv("AUTO_MIGRATE") == "1" || os.Getenv("AUTO_MIGRATE") == "true") && c.AppEnv == "production" {
		errs = append(errs, "AUTO_MIGRATE=1 is disallowed in production (APP_ENV=production); use cmd/migrate instead")
	}

	// In production, background jobs require dedicated DATABASE_WORKER_URL (ADR-030).
	if c.AppEnv == "production" {
		if strings.TrimSpace(c.DatabaseWorkerURL) == "" {
			errs = append(errs, "DATABASE_WORKER_URL is required in production")
		} else {
			if c.DatabaseWorkerURL == c.DatabaseURL {
				errs = append(errs, "DATABASE_WORKER_URL must differ from DATABASE_URL in production")
			}
			if c.DatabaseWorkerURL == c.DatabaseMaintURL {
				errs = append(errs, "DATABASE_WORKER_URL must differ from DATABASE_MAINT_URL in production")
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("startup validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	// --- Non-fatal operational warnings (logged by caller) ---
	// L3: sandbox mode with live keys
	// These are returned via WarnStrings() for the caller to log.
	return nil
}

// ValidateWarnings returns non-fatal startup warnings (config smells that are
// not errors but indicate potential misconfiguration).
func (c *Config) ValidateWarnings() []string {
	var warns []string

	cashfreeEnabled := strings.TrimSpace(c.CashfreeAppID) != "" || strings.TrimSpace(c.CashfreeSecretKey) != ""

	// L3: sandbox mode with Cashfree keys active
	if cashfreeEnabled && !strings.EqualFold(c.CashfreeEnv, "production") {
		warns = append(warns, "Cashfree is in sandbox mode (CASHFREE_ENV != production) - payments are not real")
	}

	// KYC enabled without Cashfree (Aadhaar OTP unavailable)
	if strings.TrimSpace(c.KYCIdentitySecret) != "" && !cashfreeEnabled {
		warns = append(warns, "KYC_IDENTITY_SECRET is set but Cashfree is not configured - Aadhaar OTP verification will return 503")
	}

	// Admin email unset warning in non-production
	if strings.TrimSpace(c.AdminEmail) == "" {
		warns = append(warns, "ADMIN_EMAIL is not configured â€” outbox dead-letter alerts and operator failure notices will only be logged locally")
	}

	// Non-production worker URL fallback warning (ADR-030)
	if c.AppEnv != "production" && strings.TrimSpace(os.Getenv("DATABASE_WORKER_URL")) == "" && c.DatabaseWorkerURL != "" && c.DatabaseWorkerURL == c.DatabaseMaintURL {
		warns = append(warns, "DATABASE_WORKER_URL not set; falling back to DATABASE_MAINT_URL in non-production")
	}

	return warns
}
