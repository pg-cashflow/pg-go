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

// DigiLockerStatus is the result of a Cashfree Secure ID status poll.
type DigiLockerStatus struct {
	// Status is one of PENDING | COMPLETED | FAILED (Cashfree values).
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// DigiLockerAadhaarDoc holds the demographic fields Cashfree returns after the
// tenant completes the DigiLocker consent flow. Only last-4 of UID is present
// (masked_uid is always the XXXX-XXXX-XXXX-1234 masked form).
type DigiLockerAadhaarDoc struct {
	Name       string `json:"name"`
	DOB        string `json:"dob"`    // YYYY-MM-DD or YYYY depending on UIDAI record
	Gender     string `json:"gender"` // M | F | T
	MaskedUID  string `json:"masked_uid"`
	PhotoBytes []byte `json:"-"`
}

// createDigiLockerReq is the request body for POST /verification/digilocker.
type createDigiLockerReq struct {
	VerificationID string `json:"verification_id"`
	RedirectURL    string `json:"redirect_url"`
}

type createDigiLockerResp struct {
	VerificationURL string `json:"verification_url"`
}

// CreateDigiLockerLink creates a Cashfree Secure ID verification session and
// returns the consent/DigiLocker URL to redirect the tenant to.
//
// verificationID must be a stable identifier (we use the kyc_verification UUID
// as vendor_reference_id) so that the incoming webhook can be matched back.
// redirectURL must be server-constructed — never client-supplied.
func (c *Client) CreateDigiLockerLink(ctx context.Context, verificationID, redirectURL string) (string, error) {
	if !c.cfg.Enabled() {
		return "", fmt.Errorf("cashfree: not configured")
	}
	body := createDigiLockerReq{
		VerificationID: verificationID,
		RedirectURL:    redirectURL,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.verificationEndpoint()+"/digilocker", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	c.sign(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("cashfree create digilocker: %s %s", resp.Status, string(b))
	}
	var out createDigiLockerResp
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("cashfree create digilocker: unmarshal: %w", err)
	}
	if out.VerificationURL == "" {
		return "", fmt.Errorf("cashfree create digilocker: empty verification_url")
	}
	return out.VerificationURL, nil
}

// getDigiLockerStatusResp is the response body for GET /verification/digilocker/:id.
type getDigiLockerStatusResp struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// GetDigiLockerStatus polls the status of a Cashfree Secure ID session.
// The caller should treat Status=="COMPLETED" as the signal to fetch the document.
func (c *Client) GetDigiLockerStatus(ctx context.Context, verificationID string) (*DigiLockerStatus, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.verificationEndpoint()+"/digilocker/"+verificationID, nil)
	if err != nil {
		return nil, err
	}
	c.sign(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree get digilocker status: %s %s", resp.Status, string(b))
	}
	var out getDigiLockerStatusResp
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("cashfree get digilocker status: unmarshal: %w", err)
	}
	return &DigiLockerStatus{Status: out.Status, Message: out.Message}, nil
}

// getDigiLockerDocResp wraps the demographic data returned by Cashfree Secure ID.
type getDigiLockerDocResp struct {
	Data struct {
		Aadhaar struct {
			Name      string `json:"name"`
			DOB       string `json:"dob"`
			Gender    string `json:"gender"`
			MaskedUID string `json:"masked_uid"`
			Image     string `json:"image"`
		} `json:"aadhaar"`
	} `json:"data"`
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

// GetDigiLockerDocument fetches the Aadhaar demographic data after the tenant
// has completed the DigiLocker consent flow.
//
// IMPORTANT: This is a network call. Callers must invoke it BEFORE opening any
// DB transaction (network-before-transaction invariant, ADR-004).
func (c *Client) GetDigiLockerDocument(ctx context.Context, verificationID string) (*DigiLockerAadhaarDoc, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.verificationEndpoint()+"/digilocker/"+verificationID+"/document", nil)
	if err != nil {
		return nil, err
	}
	c.sign(req)
	resp, err := c.http.Do(req)
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
	var out getDigiLockerDocResp
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, &CorruptDocumentError{Reason: fmt.Sprintf("unmarshal json: %v", err)}
	}
	a := out.Data.Aadhaar
	if a.Name == "" || a.MaskedUID == "" {
		return nil, &CorruptDocumentError{Reason: "incomplete demographic data (name or masked_uid missing)"}
	}

	var photoBytes []byte
	if a.Image != "" {
		photoBytes, _ = base64.StdEncoding.DecodeString(a.Image)
	}

	return &DigiLockerAadhaarDoc{
		Name:       a.Name,
		DOB:        a.DOB,
		Gender:     a.Gender,
		MaskedUID:  a.MaskedUID,
		PhotoBytes: photoBytes,
	}, nil
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
	c.sign(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.http.Do(req)
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
