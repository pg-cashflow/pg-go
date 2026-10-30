package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"9876543210", "919876543210", false},
		{"+919876543210", "919876543210", false},
		{"09876543210", "919876543210", false},
		{"+91 98765 43210", "919876543210", false},
		{"919876543210", "919876543210", false},
		{"14155552671", "14155552671", false}, // international US
		{"123", "", true},                      // too short
		{"", "", true},                         // empty
	}

	for _, tt := range tests {
		got, err := NormalizePhone(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("NormalizePhone(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMetaWhatsAppSender_SendText_Success(t *testing.T) {
	var capturedAuth string
	var capturedPath string
	var capturedBody metaTextMessage

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"wamid.123"}]}`))
	}))
	defer ts.Close()

	sender := NewMetaWhatsAppSender(MetaWhatsAppConfig{
		BaseURL:       ts.URL,
		PhoneNumberID: "10987654321",
		AccessToken:   "test-access-token",
		Timeout:       2 * time.Second,
	})

	err := sender.Send(context.Background(), "+91 98765 43210", "Your rent payment is due")
	if err != nil {
		t.Fatalf("expected Send to succeed, got %v", err)
	}

	if capturedAuth != "Bearer test-access-token" {
		t.Errorf("expected Bearer token in Authorization header, got %q", capturedAuth)
	}
	if capturedPath != "/10987654321/messages" {
		t.Errorf("expected path /10987654321/messages, got %q", capturedPath)
	}
	if capturedBody.To != "919876543210" {
		t.Errorf("expected formatted phone '919876543210', got %q", capturedBody.To)
	}
	if capturedBody.Text.Body != "Your rent payment is due" {
		t.Errorf("expected message body match, got %q", capturedBody.Text.Body)
	}
}

func TestMetaWhatsAppSender_SendTemplate_Success(t *testing.T) {
	var capturedBody metaTemplateMessage

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"wamid.456"}]}`))
	}))
	defer ts.Close()

	sender := NewMetaWhatsAppSender(MetaWhatsAppConfig{
		BaseURL:       ts.URL,
		PhoneNumberID: "10987654321",
		AccessToken:   "test-token",
	})

	err := sender.SendTemplate(context.Background(), "9876543210", "rent_reminder", "en_IN", []string{"Ramesh", "Rs 8,500"})
	if err != nil {
		t.Fatalf("expected SendTemplate to succeed, got %v", err)
	}

	if capturedBody.Template.Name != "rent_reminder" {
		t.Errorf("expected template name 'rent_reminder', got %q", capturedBody.Template.Name)
	}
	if capturedBody.Template.Language.Code != "en_IN" {
		t.Errorf("expected language code 'en_IN', got %q", capturedBody.Template.Language.Code)
	}
	if len(capturedBody.Template.Components) != 1 || len(capturedBody.Template.Components[0].Parameters) != 2 {
		t.Fatalf("unexpected components: %+v", capturedBody.Template.Components)
	}
	if capturedBody.Template.Components[0].Parameters[0].Text != "Ramesh" {
		t.Errorf("param 0 mismatch: %q", capturedBody.Template.Components[0].Parameters[0].Text)
	}
}

func TestMetaWhatsAppSender_ErrorHandling(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"error": {
				"message": "Template name does not exist in the translated language",
				"type": "OAuthException",
				"code": 100,
				"fbtrace_id": "trace-xyz"
			}
		}`))
	}))
	defer ts.Close()

	sender := NewMetaWhatsAppSender(MetaWhatsAppConfig{
		BaseURL:       ts.URL,
		PhoneNumberID: "10987654321",
		AccessToken:   "test-token",
	})

	err := sender.Send(context.Background(), "9876543210", "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "code 100") || !strings.Contains(err.Error(), "Template name does not exist") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestMetaWhatsAppSender_UnconfiguredFails(t *testing.T) {
	sender := NewMetaWhatsAppSender(MetaWhatsAppConfig{})
	if err := sender.Send(context.Background(), "9876543210", "test"); err == nil {
		t.Fatal("expected error when unconfigured")
	}
}

// --- Dispatcher integration tests with WhatsApp channel ---

type stubWhatsAppSender struct {
	sentCount int
	lastPhone string
	lastMsg   string
	sendErr   error
}

func (s *stubWhatsAppSender) Send(ctx context.Context, toPhone, message string) error {
	s.sentCount++
	s.lastPhone = toPhone
	s.lastMsg = message
	return s.sendErr
}

func (s *stubWhatsAppSender) SendTemplate(ctx context.Context, toPhone, templateName, language string, parameters []string) error {
	return nil
}

type stubUserPhoneLookup struct {
	users map[uuid.UUID]*domain.User
}

func (l *stubUserPhoneLookup) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return l.users[id], nil
}

type stubDispatchRepo struct {
	deliveries []*domain.NotificationDelivery
}

func (r *stubDispatchRepo) FetchCandidateIDs(ctx context.Context, limit int) ([]int64, error) {
	return nil, nil
}
func (r *stubDispatchRepo) LockAndFetchEventTx(ctx context.Context, tx pgx.Tx, id int64) (*domain.OutboxEvent, error) {
	return nil, nil
}
func (r *stubDispatchRepo) MarkDispatchedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	return nil
}
func (r *stubDispatchRepo) RecordEventFailureTx(ctx context.Context, tx pgx.Tx, id int64, newAttempts int, errorJSON string, nextRetryAt time.Time, isDeadLetter bool) error {
	return nil
}
func (r *stubDispatchRepo) CreateOrGetNotificationTx(ctx context.Context, tx pgx.Tx, n *domain.Notification) (uuid.UUID, error) {
	return uuid.New(), nil
}
func (r *stubDispatchRepo) RecordDeliveryTx(ctx context.Context, tx pgx.Tx, d *domain.NotificationDelivery) error {
	r.deliveries = append(r.deliveries, d)
	return nil
}

func TestDispatcher_DeliverWhatsApp_Success(t *testing.T) {
	recipientID := uuid.New()
	userLookup := &stubUserPhoneLookup{
		users: map[uuid.UUID]*domain.User{
			recipientID: {
				ID:    recipientID,
				Phone: "9876543210",
			},
		},
	}
	waSender := &stubWhatsAppSender{}
	repo := &stubDispatchRepo{}

	d := NewDispatcher(nil, repo, nil, DefaultConfig(), nil)
	d.SetWhatsAppSender(waSender, userLookup)

	notifID := uuid.New()
	notif := &domain.Notification{
		ID:               notifID,
		RecipientID:      recipientID,
		Type:             "due_paid",
		Title:            "Rent payment received",
		DeepLink:         "/tenant/dues/123",
		IsActionRequired: true,
	}

	err := d.deliverWhatsApp(context.Background(), nil, notifID, notif)
	if err != nil {
		t.Fatalf("expected deliverWhatsApp to succeed, got %v", err)
	}

	if waSender.sentCount != 1 {
		t.Fatalf("expected 1 send, got %d", waSender.sentCount)
	}
	if waSender.lastPhone != "9876543210" {
		t.Errorf("phone mismatch: %q", waSender.lastPhone)
	}
	if !strings.Contains(waSender.lastMsg, "Rent payment received") || !strings.Contains(waSender.lastMsg, "/tenant/dues/123") {
		t.Errorf("unexpected message: %q", waSender.lastMsg)
	}

	if len(repo.deliveries) != 1 {
		t.Fatalf("expected 1 delivery record, got %d", len(repo.deliveries))
	}
	del := repo.deliveries[0]
	if del.Channel != "whatsapp" || del.Status != "sent" {
		t.Errorf("delivery mismatch: Channel=%s Status=%s", del.Channel, del.Status)
	}
}

func TestDispatcher_DeliverWhatsApp_SkippedPrefWhenNoPhone(t *testing.T) {
	recipientID := uuid.New()
	userLookup := &stubUserPhoneLookup{
		users: map[uuid.UUID]*domain.User{
			recipientID: {
				ID:    recipientID,
				Phone: "", // empty phone
			},
		},
	}
	waSender := &stubWhatsAppSender{}
	repo := &stubDispatchRepo{}

	d := NewDispatcher(nil, repo, nil, DefaultConfig(), nil)
	d.SetWhatsAppSender(waSender, userLookup)

	notifID := uuid.New()
	notif := &domain.Notification{
		ID:          notifID,
		RecipientID: recipientID,
		Type:        "due_paid",
		Title:       "Rent payment received",
	}

	err := d.deliverWhatsApp(context.Background(), nil, notifID, notif)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if waSender.sentCount != 0 {
		t.Fatalf("expected 0 sends, got %d", waSender.sentCount)
	}
	if len(repo.deliveries) != 1 {
		t.Fatalf("expected 1 delivery record, got %d", len(repo.deliveries))
	}
	del := repo.deliveries[0]
	if del.Channel != "whatsapp" || del.Status != "skipped_pref" {
		t.Errorf("delivery mismatch: Channel=%s Status=%s", del.Channel, del.Status)
	}
}

func TestDispatcher_DeliverWhatsApp_FailureRecorded(t *testing.T) {
	recipientID := uuid.New()
	userLookup := &stubUserPhoneLookup{
		users: map[uuid.UUID]*domain.User{
			recipientID: {
				ID:    recipientID,
				Phone: "9876543210",
			},
		},
	}
	waSender := &stubWhatsAppSender{
		sendErr: fmt.Errorf("Meta API rate limit exceeded"),
	}
	repo := &stubDispatchRepo{}

	d := NewDispatcher(nil, repo, nil, DefaultConfig(), nil)
	d.SetWhatsAppSender(waSender, userLookup)

	notifID := uuid.New()
	notif := &domain.Notification{
		ID:          notifID,
		RecipientID: recipientID,
		Type:        "payment_rejected",
		Title:       "Payment rejected",
	}

	err := d.deliverWhatsApp(context.Background(), nil, notifID, notif)
	if err == nil {
		t.Fatal("expected deliverWhatsApp to return error when sender fails")
	}

	if len(repo.deliveries) != 1 {
		t.Fatalf("expected 1 delivery record, got %d", len(repo.deliveries))
	}
	del := repo.deliveries[0]
	if del.Channel != "whatsapp" || del.Status != "failed" {
		t.Errorf("delivery mismatch: Channel=%s Status=%s", del.Channel, del.Status)
	}
	if del.LastError == nil || !strings.Contains(*del.LastError, "rate limit exceeded") {
		t.Errorf("expected LastError to contain 'rate limit exceeded', got %v", del.LastError)
	}
}

