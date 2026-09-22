package config

import (
	"fmt"
	"os"
	"strings"
)

// ValidateForRealDeployment enforces security invariants that must hold
// whenever real credentials are present — regardless of APP_ENV.
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
	for _, s := range secrets {
		if strings.HasPrefix(s.val, "change-me") {
			errs = append(errs, fmt.Sprintf("%s must not be a placeholder value (starts with 'change-me')", s.name))
		} else if len(s.val) < 32 {
			errs = append(errs, fmt.Sprintf("%s must be at least 32 characters (got %d)", s.name, len(s.val)))
		}
	}

	// --- Cashfree webhook secret required whenever Cashfree keys are set ---
	cashfreeEnabled := strings.TrimSpace(c.CashfreeAppID) != "" || strings.TrimSpace(c.CashfreeSecretKey) != ""
	if cashfreeEnabled && strings.TrimSpace(c.CashfreeWebhookSecret) == "" {
		errs = append(errs, "CASHFREE_WEBHOOK_SECRET is required whenever CASHFREE_APP_ID or CASHFREE_SECRET_KEY is set (any environment)")
	}

	// --- AUTO_MIGRATE safety ---
	if (os.Getenv("AUTO_MIGRATE") == "1" || os.Getenv("AUTO_MIGRATE") == "true") && c.AppEnv == "production" {
		errs = append(errs, "AUTO_MIGRATE=1 is disallowed in production (APP_ENV=production); use cmd/migrate instead")
	}

	if len(errs) > 0 {
		return fmt.Errorf("startup validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	// --- Non-fatal operational warnings (logged by caller) ---
	// L1: CASHFREE_WEBHOOK_SECRET fallback
	// L3: sandbox mode with live keys
	// These are returned via WarnStrings() for the caller to log.
	return nil
}

// ValidateWarnings returns non-fatal startup warnings (config smells that are
// not errors but indicate potential misconfiguration).
func (c *Config) ValidateWarnings() []string {
	var warns []string

	cashfreeEnabled := strings.TrimSpace(c.CashfreeAppID) != "" || strings.TrimSpace(c.CashfreeSecretKey) != ""

	// L1: webhook secret fell back to API secret key
	if cashfreeEnabled && os.Getenv("CASHFREE_WEBHOOK_SECRET") == "" && c.CashfreeWebhookSecret != "" {
		warns = append(warns, "CASHFREE_WEBHOOK_SECRET not set; falling back to CASHFREE_SECRET_KEY — update if Cashfree webhook and API secrets differ")
	}

	// L3: sandbox mode with Cashfree keys active
	if cashfreeEnabled && !strings.EqualFold(c.CashfreeEnv, "production") {
		warns = append(warns, "Cashfree is in sandbox mode (CASHFREE_ENV != production) — payments are not real")
	}

	// KYC enabled without Cashfree (DigiLocker unavailable, QR-only mode)
	if strings.TrimSpace(c.KYCIdentitySecret) != "" && !cashfreeEnabled {
		warns = append(warns, "KYC_IDENTITY_SECRET is set but Cashfree is not configured — KYC is active in QR-only mode; DigiLocker initiation will return 503")
	}

	return warns
}
