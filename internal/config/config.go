package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL                       string
	DatabaseMaintURL                  string
	JWTSecret                         string
	OTPHMACSecret                     string
	MagicLinkHMACSecret               string
	MagicLinkBaseURL                  string
	SMSPrimaryURL                     string
	SMSPrimaryAPIKey                  string
	SMSFallbackURL                    string
	SMSFallbackAPIKey                 string
	SMTPHost                          string
	SMTPPort                          string
	SMTPUsername                      string
	SMTPPassword                      string
	SMTPFrom                          string
	VAPIDPublicKey                    string
	VAPIDPrivateKey                   string
	VAPIDSubject                      string
	HTTPAddr                          string
	AppEnv                            string
	CORSAllowedOrigins                []string
	FrontendURL                       string
	FirebaseProjectID                 string
	FirebaseCredentials               string
	CashfreeAppID                     string // Deprecated: alias for CashfreePGAppID
	CashfreeSecretKey                 string // Deprecated: alias for CashfreePGSecretKey
	CashfreePGAppID                   string
	CashfreePGSecretKey               string
	CashfreeKYCAppID                  string
	CashfreeKYCSecretKey              string
	CashfreeEnv                       string
	CashfreeWebhookSecret             string
	WebhookTimestampToleranceSec      int
	OrderExpiryDuration               time.Duration
	OrderPollerBufferDuration         time.Duration
	WebhookAPIVersion                 string
	AadhaarQRPublicKeyPEM             string
	KYCIdentitySecret                 string
	KYCDigiLockerRedirectURL          string
	CashfreePayoutClientID            string
	CashfreePayoutClientSecret        string
	CashfreePayoutAPIVersion          string
	CashfreePayoutFundsourceID        string
	CashfreePayoutWebhookSecret       string
	CashfreePayoutEnv                 string
	CashfreePayoutAutoDispatchEnabled bool
	FinanceEnabled                    bool
	IntelligenceEnabled               bool
	AdminEmail                        string
	AdminPhone                        string
	TrustedProxies                    []string
	// PayoutExportChecksumSecret signs the HMAC on CSV payout batch exports.
	// If unset, falls back to JWTSecret (see handlers_payouts.go:getChecksumSecret).
	// Set CF_PAYOUT_EXPORT_SECRET to an independent high-entropy value.
	PayoutExportChecksumSecret string
	// PayoutEncryptionSecret is dedicated to encrypting sensitive beneficiary bank details.
	// In production, must be independently set and must not fall back to JWTSecret.
	PayoutEncryptionSecret string
	// PayoutEncryptionKeys holds versioned keys for rotation (version -> secret).
	PayoutEncryptionKeys map[byte]string
}

func Load() (*Config, error) {
	cfg := &Config{
		AdminEmail:                        envFirst("ADMIN_EMAIL", "ALERT_EMAIL", "SMTP_FROM"),
		AdminPhone:                        envFirst("ADMIN_PHONE", "ALERT_PHONE"),
		DatabaseURL:                       os.Getenv("DATABASE_URL"),
		DatabaseMaintURL:                  envFirst("DATABASE_MAINT_URL", "DATABASE_URL"),
		JWTSecret:                         os.Getenv("JWT_SECRET"),
		OTPHMACSecret:                     os.Getenv("OTP_HMAC_SECRET"),
		MagicLinkHMACSecret:               os.Getenv("MAGIC_LINK_HMAC_SECRET"),
		MagicLinkBaseURL:                  os.Getenv("MAGIC_LINK_BASE_URL"),
		SMSPrimaryURL:                     os.Getenv("SMS_PRIMARY_URL"),
		SMSPrimaryAPIKey:                  os.Getenv("SMS_PRIMARY_API_KEY"),
		SMSFallbackURL:                    os.Getenv("SMS_FALLBACK_URL"),
		SMSFallbackAPIKey:                 os.Getenv("SMS_FALLBACK_API_KEY"),
		SMTPHost:                          os.Getenv("SMTP_HOST"),
		SMTPPort:                          envOr("SMTP_PORT", "587"),
		SMTPUsername:                      os.Getenv("SMTP_USERNAME"),
		SMTPPassword:                      os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:                          os.Getenv("SMTP_FROM"),
		VAPIDPublicKey:                    os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivateKey:                   os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject:                      os.Getenv("VAPID_SUBJECT"),
		HTTPAddr:                          envOr("HTTP_ADDR", ":8080"),
		AppEnv:                            envOr("APP_ENV", "development"),
		CORSAllowedOrigins:                splitOrigins(os.Getenv("CORS_ALLOWED_ORIGINS")),
		TrustedProxies:                    splitOrigins(os.Getenv("TRUSTED_PROXIES")),
		FrontendURL:                       os.Getenv("FRONTEND_URL"),
		FirebaseProjectID:                 os.Getenv("FIREBASE_PROJECT_ID"),
		FirebaseCredentials:               os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		CashfreePGAppID:                   envFirst("CASHFREE_PG_APP_ID", "CASHFREE_APP_ID"),
		CashfreePGSecretKey:               envFirst("CASHFREE_PG_SECRET_KEY", "CASHFREE_SECRET_KEY"),
		CashfreeKYCAppID:                  os.Getenv("CASHFREE_KYC_APP_ID"),
		CashfreeKYCSecretKey:              os.Getenv("CASHFREE_KYC_SECRET_KEY"),
		CashfreeEnv:                       envOr("CASHFREE_ENV", "sandbox"),
		CashfreeWebhookSecret:             envFirst("CASHFREE_PG_WEBHOOK_SECRET", "CASHFREE_WEBHOOK_SECRET"),
		CashfreePayoutClientID:            os.Getenv("CF_PAYOUT_CLIENT_ID"),
		CashfreePayoutClientSecret:        os.Getenv("CF_PAYOUT_CLIENT_SECRET"),
		CashfreePayoutAPIVersion:          envOr("CF_PAYOUT_API_VERSION", "2024-01-01"),
		CashfreePayoutFundsourceID:        os.Getenv("CF_PAYOUT_FUNDSOURCE_ID"),
		CashfreePayoutWebhookSecret:       os.Getenv("CF_PAYOUT_WEBHOOK_SECRET"),
		CashfreePayoutEnv:                 envOr("CF_PAYOUT_ENV", "sandbox"),
		CashfreePayoutAutoDispatchEnabled: envBoolDefaultFalse("CF_PAYOUT_AUTO_DISPATCH_ENABLED"),
		WebhookTimestampToleranceSec:      envIntOr("WEBHOOK_TIMESTAMP_TOLERANCE_SEC", 300),
		OrderExpiryDuration:               envDurationOr("ORDER_EXPIRY_DURATION", 30*time.Minute),
		OrderPollerBufferDuration:         envDurationOr("ORDER_POLLER_BUFFER_DURATION", 2*time.Hour),
		WebhookAPIVersion:                 envOr("CASHFREE_API_VERSION", "2025-01-01"),
		AadhaarQRPublicKeyPEM:             os.Getenv("AADHAAR_QR_PUBLIC_KEY_PEM"),
		KYCIdentitySecret:                 os.Getenv("KYC_IDENTITY_SECRET"),
		KYCDigiLockerRedirectURL:          os.Getenv("KYC_DIGILOCKER_REDIRECT_URL"),
		FinanceEnabled:                    envBoolDefaultTrue("FINANCE_ENABLED"),
		IntelligenceEnabled:               envBoolDefaultTrue("INTELLIGENCE_ENABLED"),
		PayoutExportChecksumSecret:        envFirst("PAYOUT_CHECKSUM_SECRET", "CF_PAYOUT_EXPORT_SECRET"),
		PayoutEncryptionSecret:            envFirst("PAYOUT_ENCRYPTION_SECRET", "PAYOUT_ENCRYPTION_KEY"),
	}
	if keysStr := os.Getenv("PAYOUT_ENCRYPTION_KEYS"); keysStr != "" {
		cfg.PayoutEncryptionKeys = parseVersionedKeys(keysStr)
	}
	cfg.CashfreeAppID = cfg.CashfreePGAppID
	cfg.CashfreeSecretKey = cfg.CashfreePGSecretKey
	if cfg.CashfreeWebhookSecret == "" {
		cfg.CashfreeWebhookSecret = cfg.CashfreePGSecretKey
	}
	if cfg.KYCDigiLockerRedirectURL == "" {
		if cfg.FrontendURL != "" {
			cfg.KYCDigiLockerRedirectURL = strings.TrimRight(cfg.FrontendURL, "/") + "/tenant/kyc/callback"
		} else {
			cfg.KYCDigiLockerRedirectURL = "http://localhost:3000/tenant/kyc/callback"
		}
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	if cfg.OTPHMACSecret == "" {
		return nil, fmt.Errorf("OTP_HMAC_SECRET is required")
	}
	if cfg.MagicLinkHMACSecret == "" {
		return nil, fmt.Errorf("MAGIC_LINK_HMAC_SECRET is required")
	}
	if cfg.AppEnv == "production" {
		if cfg.FirebaseProjectID == "" {
			return nil, fmt.Errorf("FIREBASE_PROJECT_ID is required in production")
		}
		if strings.TrimSpace(cfg.PayoutEncryptionSecret) == "" {
			return nil, fmt.Errorf("PAYOUT_ENCRYPTION_SECRET is required in production (must not reuse JWT_SECRET)")
		}
	}
	cfAppID := strings.TrimSpace(cfg.CashfreePGAppID)
	if cfAppID == "" {
		cfAppID = strings.TrimSpace(cfg.CashfreeAppID)
	}
	cfSecret := strings.TrimSpace(cfg.CashfreePGSecretKey)
	if cfSecret == "" {
		cfSecret = strings.TrimSpace(cfg.CashfreeSecretKey)
	}
	if (cfAppID != "" || cfSecret != "") && strings.TrimSpace(cfg.CashfreeWebhookSecret) == "" {
		return nil, fmt.Errorf("CASHFREE_WEBHOOK_SECRET is required whenever Cashfree PG is enabled (any environment)")
	}
	return cfg, nil
}

func parseVersionedKeys(s string) map[byte]string {
	out := make(map[byte]string)
	pairs := strings.Split(s, ",")
	for _, p := range pairs {
		parts := strings.SplitN(strings.TrimSpace(p), ":", 2)
		if len(parts) == 2 {
			ver, err := strconv.Atoi(parts[0])
			if err == nil && ver >= 0 && ver <= 255 {
				out[byte(ver)] = parts[1]
			}
		}
	}
	return out
}

func envBoolDefaultTrue(k string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no"
}

func envBoolDefaultFalse(k string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return false
	}
	return v == "1" || v == "true" || v == "yes"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			out = append(out, o)
		}
	}
	return out
}

func MustInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envIntOr(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDurationOr(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
