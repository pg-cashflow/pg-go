package cashfree

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
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
	// ErrBatchNotFound is returned when Cashfree reports that the referenced batch transfer does not exist (404).
	ErrBatchNotFound = errors.New("cashfree payout: batch not found")
	// ErrPayoutAuthRejected indicates that Cashfree refused the request due to authentication failure (401 or 403).
	// This signals a configuration, credentials, signature, or IP whitelisting rejection.
	ErrPayoutAuthRejected = errors.New("cashfree payout: authentication rejected (401/403: verify credentials, public key, or IP whitelisting)")
)

// PayoutConfig holds configuration for Cashfree Transfers V2 (Payouts).
type PayoutConfig struct {
	ClientID     string
	ClientSecret string
	APIVersion   string // "2024-01-01"
	FundsourceID string
	Env          string // "sandbox" | "production"
	AuthMode     string // "signature" | "ip"
	PublicKey    *rsa.PublicKey
	Now          func() time.Time // Injectable clock for tests
}

// ParseRSAPublicKeyOrCertificate parses a PEM-encoded block that contains an RSA public key.
// It handles PKIX SubjectPublicKeyInfo, PKCS#1 RSAPublicKey, and X.509 Certificates (matching
// OpenSSL's openssl_pkey_get_public behavior on certificate/public_key.pem).
func ParseRSAPublicKeyOrCertificate(pemData []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("no PEM block found in public key data")
	}

	// 1. Try standard PKIX SubjectPublicKeyInfo
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaPub, ok := pub.(*rsa.PublicKey); ok {
			return rsaPub, nil
		}
		return nil, errors.New("PEM block contains non-RSA public key")
	}

	// 2. Try PKCS#1 RSAPublicKey
	if rsaPub, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return rsaPub, nil
	}

	// 3. Try X.509 Certificate
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		if rsaPub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			return rsaPub, nil
		}
		return nil, errors.New("certificate contains non-RSA public key")
	}

	return nil, errors.New("failed to parse PEM data as PKIX, PKCS#1, or X.509 certificate")
}

// GeneratePayoutSignature creates the Cashfree X-Cf-Signature header value.
// It formats "${clientID}.${unixTimestamp}" and encrypts it using RSA-OAEP with SHA-1,
// matching OpenSSL's OPENSSL_PKCS1_OAEP_PADDING default. The resulting ciphertext is Base64 encoded.
func GeneratePayoutSignature(clientID string, pubKey *rsa.PublicKey, now time.Time) (string, error) {
	if strings.TrimSpace(clientID) == "" {
		return "", errors.New("cashfree payout: client_id required for signature generation")
	}
	if pubKey == nil {
		return "", errors.New("cashfree payout: public key required for signature generation")
	}
	plainText := fmt.Sprintf("%s.%d", clientID, now.Unix())
	cipherBytes, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pubKey, []byte(plainText), nil)
	if err != nil {
		return "", fmt.Errorf("cashfree payout: encrypt signature: %w", err)
	}
	return base64.StdEncoding.EncodeToString(cipherBytes), nil
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

	if strings.EqualFold(c.cfg.AuthMode, "signature") && c.cfg.PublicKey != nil {
		now := time.Now().UTC()
		if c.cfg.Now != nil {
			now = c.cfg.Now()
		}
		sig, err := GeneratePayoutSignature(c.cfg.ClientID, c.cfg.PublicKey, now)
		if err == nil {
			req.Header.Set("X-Cf-Signature", sig)
		} else {
			slog.Error("cashfree payout: failed to generate X-Cf-Signature", "err", err)
		}
	}
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
