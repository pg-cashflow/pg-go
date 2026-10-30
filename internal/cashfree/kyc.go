package cashfree

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// verificationEndpoint returns the Cashfree Secure ID API base URL, separate
// from the payments endpoint (/pg). Respects the same sandbox/production env
// flag and any baseOverride set for testing.
func (c *Client) verificationEndpoint() string {
	if c.baseOverride != "" {
		return strings.TrimRight(c.baseOverride, "/")
	}
	if strings.EqualFold(c.cfg.Env, "production") {
		return "https://api.cashfree.com/verification"
	}
	return "https://sandbox.cashfree.com/verification"
}


// APIError represents an HTTP error returned by Cashfree with a status code.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cashfree api error (status %d)", e.StatusCode)
}

func (e *APIError) HTTPStatusCode() int {
	return e.StatusCode
}

// IsPermanent returns true if the HTTP status indicates an unrecoverable tenant/session error
// (400 Bad Request, 404 Not Found, 410 Gone, 422 Unprocessable Entity).
//
// 401 Unauthorized, 403 Forbidden, and 429 Too Many Requests are conservatively classified
// as transient (returning false) so the webhook handler responds with 500 and allows
// vendor retries. This avoids falsely attributing merchant credential issues, IP blocks,
// tier restrictions, or rate limits to tenant document failure.
func (e *APIError) IsPermanent() bool {
	if e.StatusCode == http.StatusUnauthorized ||
		e.StatusCode == http.StatusForbidden ||
		e.StatusCode == http.StatusTooManyRequests {
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// CorruptDocumentError represents an unparseable or incomplete response payload from Cashfree.
type CorruptDocumentError struct {
	Reason string
}

func (e *CorruptDocumentError) Error() string {
	return fmt.Sprintf("cashfree corrupt document: %s", e.Reason)
}

// IsPermanent returns true as payload corruption cannot be resolved via retries.
func (e *CorruptDocumentError) IsPermanent() bool {
	return true
}


// SmartOCRResponse contains the extracted verification results from Cashfree Smart OCR (/bharat-ocr).
type SmartOCRResponse struct {
	ReferenceID int64
	Status      string
	MessageCode string
	Name        string
	DOB         string
	Gender      string
	MaskedUID   string
	QRStatus    *string
	PhotoBytes  []byte
}

// UploadAadhaarDocument submits a single Aadhaar file (JPEG/PNG/PDF up to 5MB) to Cashfree Smart OCR.
// Network call: callers must invoke outside database transactions.
func (c *Client) UploadAadhaarDocument(ctx context.Context, fileReader io.Reader, filename string) (*SmartOCRResponse, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("document_type", "AADHAAR"); err != nil {
		return nil, fmt.Errorf("cashfree ocr: write document_type: %w", err)
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("cashfree ocr: create form file: %w", err)
	}
	if _, err := io.Copy(part, fileReader); err != nil {
		return nil, fmt.Errorf("cashfree ocr: copy file data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("cashfree ocr: close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.verificationEndpoint()+"/bharat-ocr", &body)
	if err != nil {
		return nil, err
	}
	if err := c.signKYC(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.kycClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(b),
		}
	}

	var raw struct {
		ReferenceID int64  `json:"reference_id"`
		Status      string `json:"status"`
		MessageCode string `json:"message_code"`
		OCRData     struct {
			Name   string `json:"name"`
			DOB    string `json:"dob"`
			Gender string `json:"gender"`
			UID    string `json:"uid"`
			Image  string `json:"image"`
		} `json:"ocr_data"`
		QRDetails struct {
			Status string `json:"status"`
		} `json:"qr_details"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, &CorruptDocumentError{Reason: fmt.Sprintf("unmarshal json: %v", err)}
	}

	uidClean := strings.ReplaceAll(raw.OCRData.UID, " ", "")
	var maskedUID string
	if len(uidClean) >= 4 {
		maskedUID = uidClean[len(uidClean)-4:]
	}
	if raw.OCRData.Name == "" || maskedUID == "" {
		return nil, &CorruptDocumentError{Reason: "incomplete OCR demographic data (name or uid missing)"}
	}

	var photoBytes []byte
	if raw.OCRData.Image != "" {
		photoBytes, _ = base64.StdEncoding.DecodeString(raw.OCRData.Image)
	}

	var qrStatus *string
	if raw.QRDetails.Status != "" {
		q := strings.ToUpper(raw.QRDetails.Status)
		qrStatus = &q
	}

	return &SmartOCRResponse{
		ReferenceID: raw.ReferenceID,
		Status:      raw.Status,
		MessageCode: raw.MessageCode,
		Name:        raw.OCRData.Name,
		DOB:         raw.OCRData.DOB,
		Gender:      raw.OCRData.Gender,
		MaskedUID:   maskedUID,
		QRStatus:    qrStatus,
		PhotoBytes:  photoBytes,
	}, nil
}

// AadhaarOTPInitResult holds the response from initiating an Aadhaar OTP session.
type AadhaarOTPInitResult struct {
	RefID   string `json:"ref_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// AadhaarOTPVerifyResult holds the demographic data returned after verifying Aadhaar OTP.
type AadhaarOTPVerifyResult struct {
	RefID      string `json:"ref_id"`
	Status     string `json:"status"`
	Name       string `json:"name"`
	DOB        string `json:"dob"`
	Gender     string `json:"gender"`
	MaskedUID  string `json:"masked_uid"`
	PhotoBytes []byte `json:"-"`
	PhotoLink  string `json:"photo_link,omitempty"`
}

// RequestAadhaarOTP initiates an Aadhaar OTP verification session via Cashfree.
func (c *Client) RequestAadhaarOTP(ctx context.Context, aadhaarNumber string) (*AadhaarOTPInitResult, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	body := map[string]string{
		"aadhaar_number": strings.TrimSpace(aadhaarNumber),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.verificationEndpoint()+"/offline-aadhaar/otp", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := c.signKYC(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.kycClient().Do(req)
	if err != nil {
		if !strings.EqualFold(c.cfg.Env, "production") && c.cfg.MockKYC {
			last4 := "9999"
			cleanNum := strings.ReplaceAll(aadhaarNumber, " ", "")
			if len(cleanNum) >= 4 {
				last4 = cleanNum[len(cleanNum)-4:]
			}
			refStr := fmt.Sprintf("sb_mock_%s_%d", last4, time.Now().UnixNano())
			return &AadhaarOTPInitResult{
				RefID:   refStr,
				Status:  "SUCCESS",
				Message: "Sandbox test OTP generated (enter 111000 or any 6-digit code)",
			}, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(b),
		}
	}
	var out struct {
		RefID   any    `json:"ref_id"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("cashfree request aadhaar otp: unmarshal: %w", err)
	}
	refStr := fmt.Sprintf("%v", out.RefID)
	return &AadhaarOTPInitResult{
		RefID:   refStr,
		Status:  out.Status,
		Message: out.Message,
	}, nil
}

// VerifyAadhaarOTP completes the Aadhaar OTP verification via Cashfree.
func (c *Client) VerifyAadhaarOTP(ctx context.Context, refID, otp string) (*AadhaarOTPVerifyResult, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	trimmedRef := strings.TrimSpace(refID)
	if strings.HasPrefix(trimmedRef, "sb_mock_") && !strings.EqualFold(c.cfg.Env, "production") && c.cfg.MockKYC {
		last4 := "9999"
		parts := strings.Split(trimmedRef, "_")
		if len(parts) >= 3 && len(parts[2]) == 4 {
			last4 = parts[2]
		}
		return &AadhaarOTPVerifyResult{
			RefID:      trimmedRef,
			Status:     "VALID",
			Name:       "Aadhaar Verified Tenant",
			DOB:        "1998-05-15",
			Gender:     "M",
			MaskedUID:  last4,
			PhotoBytes: nil,
		}, nil
	}
	body := map[string]string{
		"ref_id": trimmedRef,
		"otp":    strings.TrimSpace(otp),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.verificationEndpoint()+"/offline-aadhaar/verify", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := c.signKYC(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.kycClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(b),
		}
	}
	var out struct {
		RefID     any    `json:"ref_id"`
		Status    string `json:"status"`
		Name      string `json:"name"`
		DOB       string `json:"dob"`
		Gender    string `json:"gender"`
		UID       string `json:"uid"`
		MaskedUID string `json:"masked_uid"`
		Image     string `json:"image"`
		PhotoLink string `json:"photo_link"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("cashfree verify aadhaar otp: unmarshal: %w", err)
	}
	mUID := out.MaskedUID
	if mUID == "" && len(out.UID) >= 4 {
		mUID = out.UID[len(out.UID)-4:]
	}
	var photoBytes []byte
	if out.Image != "" {
		photoBytes, _ = base64.StdEncoding.DecodeString(out.Image)
	} else if out.PhotoLink != "" {
		if strings.HasPrefix(out.PhotoLink, "data:") {
			parts := strings.Split(out.PhotoLink, ",")
			if len(parts) > 1 {
				photoBytes, _ = base64.StdEncoding.DecodeString(parts[1])
			}
		} else if b64, err := base64.StdEncoding.DecodeString(out.PhotoLink); err == nil && len(b64) > 0 {
			photoBytes = b64
		}
	}
	return &AadhaarOTPVerifyResult{
		RefID:      fmt.Sprintf("%v", out.RefID),
		Status:     out.Status,
		Name:       out.Name,
		DOB:        out.DOB,
		Gender:     out.Gender,
		MaskedUID:  mUID,
		PhotoBytes: photoBytes,
		PhotoLink:  out.PhotoLink,
	}, nil
}

// AadhaarFailureClass categorizes UIDAI/Cashfree failure modes.
type AadhaarFailureClass string

const (
	FailureRecoverable AadhaarFailureClass = "recoverable"
	FailureCooldown    AadhaarFailureClass = "cooldown"
	FailureTerminal    AadhaarFailureClass = "terminal"
	FailureSystemFault AadhaarFailureClass = "system_fault"
)

func ClassifyAadhaarError(httpStatus int, message string) (code string, class AadhaarFailureClass) {
	m := strings.ToLower(message)
	switch {
	case httpStatus == 401 || httpStatus == 403 || strings.Contains(m, "unauthorized") || strings.Contains(m, "ip not whitelisted"):
		return "PROVIDER_AUTH_ERROR", FailureSystemFault
	case httpStatus >= 500:
		return "PROVIDER_ERROR", FailureSystemFault
	case httpStatus == 422 || strings.Contains(m, "insufficient balance"):
		return "PROVIDER_BALANCE", FailureSystemFault
	case strings.Contains(m, "does not have mobile number"),
		strings.Contains(m, "not linked to mobile"),
		strings.Contains(m, "mobile number not registered"):
		return "MOBILE_NOT_LINKED", FailureTerminal
	case strings.Contains(m, "otp expired"):
		return "OTP_EXPIRED", FailureRecoverable
	case strings.Contains(m, "session expired"):
		return "SESSION_EXPIRED", FailureRecoverable
	case strings.Contains(m, "otp entered is invalid"),
		strings.Contains(m, "please enter a valid otp"),
		strings.Contains(m, "incorrect otp"):
		return "WRONG_OTP", FailureRecoverable
	case strings.Contains(m, "please try after some time"),
		strings.Contains(m, "otp generated for this aadhaar"):
		return "COOLDOWN_ACTIVE", FailureCooldown
	case strings.Contains(m, "invalid aadhaar"):
		return "INVALID_AADHAAR", FailureRecoverable // Upgraded to terminal on repeat in service layer
	default:
		return "UNKNOWN", FailureRecoverable // Fail open to recoverable to prevent accidental lockouts
	}
}

// CooldownSecondsFor returns the recommended retry cooldown in seconds.
// Cashfree guidance requires 50s before resubmitting after a wrong OTP failure,
// 30s before requesting a new OTP, and 45s for general throttle cooldowns.
func CooldownSecondsFor(class AadhaarFailureClass, reasonCode string) int {
	switch class {
	case FailureCooldown:
		return 45
	case FailureRecoverable:
		if reasonCode == "WRONG_OTP" {
			return 50
		}
		return 30
	default:
		return 0
	}
}

