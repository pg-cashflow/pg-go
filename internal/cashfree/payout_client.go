package cashfree

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrPayoutNotConfigured indicates that Cashfree Payouts credentials are not present.
	ErrPayoutNotConfigured = errors.New("cashfree payout: client credentials not configured")
	// ErrDispatchUnknown indicates that a batch transfer call failed ambiguously (5xx or network transport error).
	// Under Cashfree guidelines, the caller must NOT re-initiate, but must poll batch status instead.
	ErrDispatchUnknown = errors.New("cashfree payout: batch dispatch ambiguous (5xx or transport failure)")
	// ErrBeneficiaryNotFound is returned when Cashfree reports that the referenced beneficiary does not exist (404).
	ErrBeneficiaryNotFound = errors.New("cashfree payout: beneficiary not found")
)

// PayoutConfig holds configuration for Cashfree Transfers V2 (Payouts).
type PayoutConfig struct {
	ClientID     string
	ClientSecret string
	APIVersion   string // "2024-01-01"
	FundsourceID string
	Env          string // "sandbox" | "production"
}

// Enabled returns true if client credentials and fundsource_id are configured.
func (c PayoutConfig) Enabled() bool {
	return strings.TrimSpace(c.ClientID) != "" &&
		strings.TrimSpace(c.ClientSecret) != "" &&
		strings.TrimSpace(c.FundsourceID) != ""
}

func (c PayoutConfig) baseURL() string {
	if strings.EqualFold(c.Env, "production") {
		return "https://api.cashfree.com/payout"
	}
	return "https://sandbox.cashfree.com/payout"
}

// PayoutClient executes requests against the Cashfree Transfers V2 API.
type PayoutClient struct {
	cfg          PayoutConfig
	http         *http.Client
	baseOverride string
}

// NewPayoutClient constructs a new PayoutClient with default timeouts and versioning.
func NewPayoutClient(cfg PayoutConfig) *PayoutClient {
	if cfg.APIVersion == "" {
		cfg.APIVersion = "2024-01-01"
	}
	return &PayoutClient{
		cfg:  cfg,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetBaseOverride allows overriding the target endpoint during tests.
func (c *PayoutClient) SetBaseOverride(url string) {
	c.baseOverride = url
}

func (c *PayoutClient) endpoint() string {
	if c.baseOverride != "" {
		return strings.TrimRight(c.baseOverride, "/")
	}
	return c.cfg.baseURL()
}

func (c *PayoutClient) sign(req *http.Request) {
	req.Header.Set("x-client-id", c.cfg.ClientID)
	req.Header.Set("x-client-secret", c.cfg.ClientSecret)
	req.Header.Set("x-api-version", c.cfg.APIVersion)
	req.Header.Set("Content-Type", "application/json")
}

func (c *PayoutClient) checkDeprecation(resp *http.Response) {
	if dep := resp.Header.Get("x-deprecated-at"); dep != "" {
		slog.Warn("CASHFREE PAYOUT API VERSION SCHEDULED FOR DEPRECATION",
			"api_version", c.cfg.APIVersion,
			"deprecated_at", dep,
			"path", resp.Request.URL.Path,
		)
	}
}
