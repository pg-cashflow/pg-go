package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL           string
	JWTSecret             string
	OTPHMACSecret         string
	MagicLinkHMACSecret   string
	MagicLinkBaseURL      string
	SMSPrimaryURL         string
	SMSPrimaryAPIKey      string
	SMSFallbackURL        string
	SMSFallbackAPIKey     string
	SMTPHost              string
	SMTPPort              string
	SMTPUsername          string
	SMTPPassword          string
	SMTPFrom              string
	VAPIDPublicKey        string
	VAPIDPrivateKey       string
	VAPIDSubject          string
	HTTPAddr              string
	AppEnv                string
	CORSAllowedOrigins    []string
	FrontendURL           string
	FirebaseProjectID     string
	FirebaseCredentials   string
	CashfreeAppID         string
	CashfreeSecretKey     string
	CashfreeEnv           string
	CashfreeWebhookSecret string
	AadhaarQRPublicKeyPEM string
	FinanceEnabled        bool
	IntelligenceEnabled   bool
}

func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		JWTSecret:             os.Getenv("JWT_SECRET"),
		OTPHMACSecret:         os.Getenv("OTP_HMAC_SECRET"),
		MagicLinkHMACSecret:   os.Getenv("MAGIC_LINK_HMAC_SECRET"),
		MagicLinkBaseURL:      os.Getenv("MAGIC_LINK_BASE_URL"),
		SMSPrimaryURL:         os.Getenv("SMS_PRIMARY_URL"),
		SMSPrimaryAPIKey:      os.Getenv("SMS_PRIMARY_API_KEY"),
		SMSFallbackURL:        os.Getenv("SMS_FALLBACK_URL"),
		SMSFallbackAPIKey:     os.Getenv("SMS_FALLBACK_API_KEY"),
		SMTPHost:              os.Getenv("SMTP_HOST"),
		SMTPPort:              envOr("SMTP_PORT", "587"),
		SMTPUsername:          os.Getenv("SMTP_USERNAME"),
		SMTPPassword:          os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:              os.Getenv("SMTP_FROM"),
		VAPIDPublicKey:        os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivateKey:       os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject:          os.Getenv("VAPID_SUBJECT"),
		HTTPAddr:              envOr("HTTP_ADDR", ":8080"),
		AppEnv:                envOr("APP_ENV", "development"),
		CORSAllowedOrigins:    splitOrigins(os.Getenv("CORS_ALLOWED_ORIGINS")),
		FrontendURL:           os.Getenv("FRONTEND_URL"),
		FirebaseProjectID:     os.Getenv("FIREBASE_PROJECT_ID"),
		FirebaseCredentials:   os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		CashfreeAppID:         os.Getenv("CASHFREE_APP_ID"),
		CashfreeSecretKey:     os.Getenv("CASHFREE_SECRET_KEY"),
		CashfreeEnv:           envOr("CASHFREE_ENV", "sandbox"),
		CashfreeWebhookSecret: os.Getenv("CASHFREE_WEBHOOK_SECRET"),
		AadhaarQRPublicKeyPEM: os.Getenv("AADHAAR_QR_PUBLIC_KEY_PEM"),
		FinanceEnabled:        envBoolDefaultTrue("FINANCE_ENABLED"),
		IntelligenceEnabled:   envBoolDefaultTrue("INTELLIGENCE_ENABLED"),
	}
	if cfg.CashfreeWebhookSecret == "" {
		cfg.CashfreeWebhookSecret = cfg.CashfreeSecretKey
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
	if cfg.AppEnv == "production" && cfg.FirebaseProjectID == "" {
		return nil, fmt.Errorf("FIREBASE_PROJECT_ID is required in production")
	}
	cashfreeOn := strings.TrimSpace(cfg.CashfreeAppID) != "" && strings.TrimSpace(cfg.CashfreeSecretKey) != ""
	if cashfreeOn && strings.EqualFold(cfg.AppEnv, "production") && strings.TrimSpace(cfg.CashfreeWebhookSecret) == "" {
		return nil, fmt.Errorf("CASHFREE_WEBHOOK_SECRET is required in production when Cashfree is enabled")
	}
	return cfg, nil
}

func envBoolDefaultTrue(k string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no"
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
