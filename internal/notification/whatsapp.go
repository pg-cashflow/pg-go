package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// WhatsAppSender defines the contract for sending automated WhatsApp messages.
type WhatsAppSender interface {
	Send(ctx context.Context, toPhone, message string) error
	SendTemplate(ctx context.Context, toPhone, templateName, language string, parameters []string) error
}

// MetaWhatsAppConfig holds parameters for the Meta WhatsApp Cloud API.
type MetaWhatsAppConfig struct {
	BaseURL       string        // e.g. "https://graph.facebook.com/v21.0"
	PhoneNumberID string        // Phone number ID from Meta WhatsApp Developer Dashboard
	AccessToken   string        // System user permanent access token
	Timeout       time.Duration // HTTP client timeout (defaults to 10s if <= 0)
}

// MetaWhatsAppSender delivers messages via the official Meta WhatsApp Cloud API.
type MetaWhatsAppSender struct {
	cfg    MetaWhatsAppConfig
	client *http.Client
}

// NewMetaWhatsAppSender constructs a MetaWhatsAppSender.
func NewMetaWhatsAppSender(cfg MetaWhatsAppConfig) *MetaWhatsAppSender {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://graph.facebook.com/v21.0"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &MetaWhatsAppSender{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// NoopWhatsAppSender safely discards WhatsApp messages (tests or local development).
type NoopWhatsAppSender struct{}

func (NoopWhatsAppSender) Send(ctx context.Context, toPhone, message string) error {
	return nil
}

func (NoopWhatsAppSender) SendTemplate(ctx context.Context, toPhone, templateName, language string, parameters []string) error {
	return nil
}

var _ WhatsAppSender = NoopWhatsAppSender{}
var _ WhatsAppSender = (*MetaWhatsAppSender)(nil)

var nonDigits = regexp.MustCompile(`\D`)

// NormalizePhone formats an Indian or international phone number into E.164 digits without leading '+'
// suitable for the WhatsApp Business API.
func NormalizePhone(phone string) (string, error) {
	digits := nonDigits.ReplaceAllString(phone, "")
	if len(digits) == 10 {
		return "91" + digits, nil
	}
	if len(digits) == 11 && strings.HasPrefix(digits, "0") {
		return "91" + digits[1:], nil
	}
	if len(digits) == 12 && strings.HasPrefix(digits, "91") {
		return digits, nil
	}
	if len(digits) > 7 && len(digits) <= 15 {
		return digits, nil
	}
	return "", fmt.Errorf("invalid phone number: %s", phone)
}

type metaTextMessage struct {
	MessagingProduct string       `json:"messaging_product"`
	RecipientType    string       `json:"recipient_type"`
	To               string       `json:"to"`
	Type             string       `json:"type"`
	Text             metaTextBody `json:"text"`
}

type metaTextBody struct {
	PreviewURL bool   `json:"preview_url"`
	Body       string `json:"body"`
}

type metaTemplateMessage struct {
	MessagingProduct string       `json:"messaging_product"`
	RecipientType    string       `json:"recipient_type"`
	To               string       `json:"to"`
	Type             string       `json:"type"`
	Template         metaTemplate `json:"template"`
}

type metaTemplate struct {
	Name       string               `json:"name"`
	Language   metaTemplateLanguage `json:"language"`
	Components []metaComponent      `json:"components,omitempty"`
}

type metaTemplateLanguage struct {
	Code string `json:"code"`
}

type metaComponent struct {
	Type       string               `json:"type"`
	Parameters []metaComponentParam `json:"parameters"`
}

type metaComponentParam struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type metaErrorResponse struct {
	Error struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      int    `json:"code"`
		FBTraceID string `json:"fbtrace_id"`
	} `json:"error"`
}

// Send dispatches a freeform text WhatsApp message.
func (s *MetaWhatsAppSender) Send(ctx context.Context, toPhone, message string) error {
	if s.cfg.PhoneNumberID == "" || s.cfg.AccessToken == "" {
		return fmt.Errorf("whatsapp: unconfigured phone_number_id or access_token")
	}
	normalized, err := NormalizePhone(toPhone)
	if err != nil {
		return fmt.Errorf("whatsapp send: %w", err)
	}

	payload := metaTextMessage{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               normalized,
		Type:             "text",
		Text: metaTextBody{
			PreviewURL: false,
			Body:       message,
		},
	}
	return s.post(ctx, payload)
}

// SendTemplate dispatches a pre-approved template WhatsApp message with dynamic variables.
func (s *MetaWhatsAppSender) SendTemplate(ctx context.Context, toPhone, templateName, language string, parameters []string) error {
	if s.cfg.PhoneNumberID == "" || s.cfg.AccessToken == "" {
		return fmt.Errorf("whatsapp: unconfigured phone_number_id or access_token")
	}
	normalized, err := NormalizePhone(toPhone)
	if err != nil {
		return fmt.Errorf("whatsapp send template: %w", err)
	}
	if language == "" {
		language = "en_US"
	}

	tmpl := metaTemplate{
		Name:     templateName,
		Language: metaTemplateLanguage{Code: language},
	}
	if len(parameters) > 0 {
		params := make([]metaComponentParam, len(parameters))
		for i, p := range parameters {
			params[i] = metaComponentParam{Type: "text", Text: p}
		}
		tmpl.Components = []metaComponent{
			{
				Type:       "body",
				Parameters: params,
			},
		}
	}

	payload := metaTemplateMessage{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               normalized,
		Type:             "template",
		Template:         tmpl,
	}
	return s.post(ctx, payload)
}

func (s *MetaWhatsAppSender) post(ctx context.Context, payload any) error {
	endpoint := fmt.Sprintf("%s/%s/messages", s.cfg.BaseURL, s.cfg.PhoneNumberID)
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("whatsapp marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("whatsapp new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp post error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	var metaErr metaErrorResponse
	if jsonErr := json.Unmarshal(respBody, &metaErr); jsonErr == nil && metaErr.Error.Message != "" {
		return fmt.Errorf("whatsapp API error (status %d, code %d): %s", resp.StatusCode, metaErr.Error.Code, metaErr.Error.Message)
	}
	return fmt.Errorf("whatsapp HTTP %d: %s", resp.StatusCode, string(respBody))
}

