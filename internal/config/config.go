package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL                       string
	DatabaseMaintURL                  string
	DatabaseWorkerURL                 string
	JWTSecret                         string
	OTPHMACSecret                     string
	MagicLinkHMACSecret               string
	MagicLinkBaseURL                  string
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
	KYCIdentitySecret string
	// KYCIdentitySecrets holds versioned HMAC-SHA256 keys for identity_hash rotation
	// (version number â†’ secret string). Populated from KYC_IDENTITY_SECRETS env var,
	// format identical to PAYOUT_ENCRYPTION_KEYS: "1:secret-v1,2:secret-v2,...".
	// When set, the highest version is the active key; the service must verify against
	// any version still stored on a row (via hash_key_version). The legacy
	// KYCIdentitySecret (single key, version 1) is used as a fallback when this map
	// is empty, so existing deployments are unaffected.
	KYCIdentitySecrets map[int16]string
	// KYCDocCryptoKey is the hex-encoded 32-byte AES-256 key used to encrypt
	// Aadhaar document bytes (document_bytes, attested_photo_bytes) at rest.
	// Set via KYC_DOC_CRYPTO_KEY. Empty = encryption disabled (dev/CI safe).
	KYCDocCryptoKey string
	CashfreePayoutClientID            string
	CashfreePayoutClientSecret        string
	CashfreePayoutAPIVersion          string
	CashfreePayoutFundsourceID        string
	CashfreePayoutWebhookSecret       string
	CashfreePayoutEnv                 string
	CashfreePayoutAutoDispatchEnabled bool
	CashfreePayoutAuthMode            string // "signature" | "ip"
	CashfreePayoutPublicKeyPEM        string // PEM-encoded RSA public key or X.509 certificate for 2FA signature
	FinanceEnabled                    bool
	IntelligenceEnabled               bool
	AdminEmail                        string
	AdminPhone                        string
	TrustedProxies                    []string
	// PayoutExportChecksumSecret signs the HMAC on CSV payout batch exports.
	// Must be set independently via PAYOUT_CHECKSUM_SECRET or CF_PAYOUT_EXPORT_SECRET.
	// In production this field is required; the server refuses to start without it (REQ-SEC-008).
	PayoutExportChecksumSecret string
	// PayoutApprovalSecret is the HMAC-SHA256 secret dedicated to signing immutable payout approval snapshots.
	PayoutApprovalSecret string
	// PayoutApprovalKeys holds versioned approval signing keys for rotation (version -> secret).
	PayoutApprovalKeys map[string]string
	// PayoutEncryptionSecret is dedicated to encrypting sensitive beneficiary bank details.
	// In production, must be independently set and must not fall back to JWTSecret.
	PayoutEncryptionSecret string
	// PayoutEncryptionKeys holds versioned keys for rotation (version -> secret).
	PayoutEncryptionKeys map[byte]string
	// CashfreePGBaseURL optionally overrides the gateway endpoint in non-production environments.
	CashfreePGBaseURL string
	// MetricsSecret protects /metrics via X-Metrics-Token header.
	MetricsSecret string
	// WebhookSecretSource indicates the source of CashfreeWebhookSecret ("pg_secret_key" or "override").
	WebhookSecretSource string
}

func Load() (*Config, error) {
	cfg := &Config{
		AdminEmail:                        envFirst("ADMIN_EMAIL", "ALERT_EMAIL", "SMTP_FROM"),
		AdminPhone:                        envFirst("ADMIN_PHONE", "ALERT_PHONE"),
		DatabaseURL:                       os.Getenv("DATABASE_URL"),
		DatabaseMaintURL:                  envFirst("DATABASE_MAINT_URL", "DATABASE_URL"),
		DatabaseWorkerURL:                 envFirst("DATABASE_WORKER_URL", "DATABASE_MAINT_URL", "DATABASE_URL"),
		JWTSecret:                         os.Getenv("JWT_SECRET"),
		OTPHMACSecret:                     os.Getenv("OTP_HMAC_SECRET"),
		MagicLinkHMACSecret:               os.Getenv("MAGIC_LINK_HMAC_SECRET"),
		MagicLinkBaseURL:                  os.Getenv("MAGIC_LINK_BASE_URL"),
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
		FrontendURL:                       envFirst("PUBLIC_APP_URL", "FRONTEND_URL"),
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
		CashfreePayoutAuthMode:            strings.ToLower(strings.TrimSpace(os.Getenv("CF_PAYOUT_AUTH_MODE"))),
		CashfreePayoutPublicKeyPEM:        loadPayoutPublicKeyPEM(),
		WebhookTimestampToleranceSec:      envIntOr("WEBHOOK_TIMESTAMP_TOLERANCE_SEC", 300),
		OrderExpiryDuration:               envDurationOr("ORDER_EXPIRY_DURATION", 30*time.Minute),
		OrderPollerBufferDuration:         envDurationOr("ORDER_POLLER_BUFFER_DURATION", 2*time.Hour),
		WebhookAPIVersion:                 envOr("CASHFREE_API_VERSION", "2025-01-01"),
		AadhaarQRPublicKeyPEM:             os.Getenv("AADHAAR_QR_PUBLIC_KEY_PEM"),
		KYCIdentitySecret:                 os.Getenv("KYC_IDENTITY_SECRET"),
		KYCDocCryptoKey:                   strings.TrimSpace(os.Getenv("KYC_DOC_CRYPTO_KEY")),
		FinanceEnabled:                    envBoolDefaultTrue("FINANCE_ENABLED"),
		IntelligenceEnabled:               envBoolDefaultTrue("INTELLIGENCE_ENABLED"),
		PayoutExportChecksumSecret:        envFirst("PAYOUT_CHECKSUM_SECRET", "CF_PAYOUT_EXPORT_SECRET"),
		PayoutApprovalSecret:              envFirst("PAYOUT_APPROVAL_SECRET", "CF_PAYOUT_APPROVAL_SECRET"),
		PayoutEncryptionSecret:            envFirst("PAYOUT_ENCRYPTION_SECRET", "PAYOUT_ENCRYPTION_KEY"),
		CashfreePGBaseURL:                 strings.TrimSpace(os.Getenv("CASHFREE_PG_BASE_URL")),
		MetricsSecret:                     strings.TrimSpace(os.Getenv("METRICS_SECRET")),
	}
	if keysStr := os.Getenv("PAYOUT_ENCRYPTION_KEYS"); keysStr != "" {
		cfg.PayoutEncryptionKeys = parseVersionedKeys(keysStr)
	}
	if keysStr := os.Getenv("PAYOUT_APPROVAL_KEYS"); keysStr != "" {
		cfg.PayoutApprovalKeys = parseVersionedApprovalKeys(keysStr)
	}
	if keysStr := os.Getenv("KYC_IDENTITY_SECRETS"); keysStr != "" {
		cfg.KYCIdentitySecrets = parseVersionedSecrets16(keysStr)
	}
	cfg.CashfreeAppID = cfg.CashfreePGAppID
	cfg.CashfreeSecretKey = cfg.CashfreePGSecretKey
	if cfg.CashfreeWebhookSecret != "" {
		cfg.WebhookSecretSource = "override"
	} else if cfg.CashfreePGSecretKey != "" {
		cfg.CashfreeWebhookSecret = cfg.CashfreePGSecretKey
		cfg.WebhookSecretSource = "pg_secret_key"
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
		// REQ-SEC-008: checksum secret must be independently keyed â€” never fall through to JWTSecret.
		if strings.TrimSpace(cfg.PayoutExportChecksumSecret) == "" {
			return nil, fmt.Errorf("PAYOUT_CHECKSUM_SECRET is required in production (set PAYOUT_CHECKSUM_SECRET or CF_PAYOUT_EXPORT_SECRET)")
		}
		// Step-up OTP (payout_approval) is email-only by architectural contract
		// (docs/specs/auth-frontend-backend-contract.md Â§1.2). Without SMTP,
		// auth.Service silently falls back to mailer.NoopMailer, which reports
		// success while sending nothing. Fail at boot, not at an owner's
		// payout-approval request.
		if strings.TrimSpace(cfg.SMTPHost) == "" {
			return nil, fmt.Errorf("SMTP_HOST is required in production for payout step-up OTP delivery")
		}
		// TRUSTED_PROXIES must be set in production. cloudflared connects from
		// 127.0.0.1; without a trusted-proxy list c.ClientIP() returns 127.0.0.1
		// for every request, collapsing all users into one IP rate-limit bucket
		// and recording 127.0.0.1 in every audit and consent log.
		if len(cfg.TrustedProxies) == 0 {
			return nil, fmt.Errorf("TRUSTED_PROXIES must be set in production (set to the cloudflared/load-balancer IP or CIDR, e.g. '127.0.0.1')")
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
		return nil, fmt.Errorf("CASHFREE_PG_SECRET_KEY is required whenever Cashfree PG is enabled (any environment)")
	}
	if cfg.WebhookTimestampToleranceSec <= 0 {
		return nil, fmt.Errorf("WEBHOOK_TIMESTAMP_TOLERANCE_SEC must be greater than 0 (got %d)", cfg.WebhookTimestampToleranceSec)
	}
	if cfg.CashfreePGBaseURL != "" {
		if strings.EqualFold(strings.TrimSpace(cfg.AppEnv), "production") ||
			strings.EqualFold(strings.TrimSpace(cfg.CashfreeEnv), "production") {
			return nil, fmt.Errorf("CASHFREE_PG_BASE_URL override is prohibited in production")
		}
		parsed, err := url.Parse(cfg.CashfreePGBaseURL)
		if err != nil {
			return nil, fmt.Errorf("invalid CASHFREE_PG_BASE_URL: %w", err)
		}
		if parsed.Scheme != "http" {
			return nil, fmt.Errorf("CASHFREE_PG_BASE_URL scheme must be http (got %q)", parsed.Scheme)
		}
		hostname := parsed.Hostname()
		if hostname == "" {
			return nil, fmt.Errorf("CASHFREE_PG_BASE_URL must specify a host")
		}
		if !isLoopbackHost(hostname) {
			return nil, fmt.Errorf("CASHFREE_PG_BASE_URL must target a loopback host (got %q)", hostname)
		}
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

// parseVersionedSecrets16 parses a comma-separated "version:secret" string into a
// map[int16]string. The key type is int16 to match the hash_key_version column in
// kyc_verification and the hashKeyVersion parameters throughout the KYC repo and
// service layers. Valid version range: 1â€“32767 (int16 max; version 0 is reserved).
// Malformed pairs and out-of-range version numbers are silently skipped â€” the
// caller (validate.go) reports them after Load() returns.
func parseVersionedSecrets16(s string) map[int16]string {
	out := make(map[int16]string)
	for _, p := range strings.Split(s, ",") {
		parts := strings.SplitN(strings.TrimSpace(p), ":", 2)
		if len(parts) != 2 {
			continue
		}
		ver, err := strconv.Atoi(parts[0])
		if err != nil || ver < 1 || ver > 32767 {
			continue
		}
		out[int16(ver)] = parts[1]
	}
	return out
}

func parseVersionedApprovalKeys(s string) map[string]string {
	out := make(map[string]string)
	for _, p := range strings.Split(s, ",") {
		parts := strings.SplitN(strings.TrimSpace(p), ":", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			if k != "" && v != "" {
				out[k] = v
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

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadPayoutPublicKeyPEM() string {
	if raw := strings.TrimSpace(os.Getenv("CF_PAYOUT_PUBLIC_KEY")); raw != "" {
		return raw
	}
	if path := strings.TrimSpace(os.Getenv("CF_PAYOUT_PUBLIC_KEY_PATH")); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}
