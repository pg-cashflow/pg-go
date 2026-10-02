package cashfree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	ErrDisabled           = errors.New("cashfree: client not configured or disabled")
	ErrSettlementNotFound = errors.New("cashfree: settlement not found")
	ErrSettlementFailed   = errors.New("cashfree: settlement fetch failed")
)

// OrderSettlementRecord represents an order-level settlement from Cashfree.
type OrderSettlementRecord struct {
	CFSettlementID     string    `json:"cf_settlement_id"`
	CFPaymentID        string    `json:"cf_payment_id"`
	OrderID            string    `json:"order_id"`
	GrossAmountPaise   int64     `json:"gross_amount_paise"`
	NetAmountPaise     int64     `json:"net_amount_paise"`
	ServiceChargePaise int64     `json:"service_charge_paise"`
	ServiceTaxPaise    int64     `json:"service_tax_paise"`
	AdjustmentPaise    int64     `json:"adjustment_paise"`
	UTR                string    `json:"transfer_id"`
	TransferTime       time.Time `json:"transfer_time"`
	Status             string    `json:"status"`
	RawPayload         []byte    `json:"-"`
}

// SettlementWebhookPayload represents settlement webhook events (SETTLEMENT_SUCCESS, SETTLEMENT_FAILED, SETTLEMENT_REVERSED).
type SettlementWebhookPayload struct {
	Data struct {
		Settlement struct {
			SettlementID          any    `json:"settlement_id"` // string or int64
			Status                string `json:"status"`
			AmountSettled         any    `json:"amount_settled"`
			UTR                   any    `json:"utr"` // string or int64
			SettledOn             string `json:"settled_on"`
			SettlementType        string `json:"settlement_type"`
			PaymentAmount         any    `json:"payment_amount"`
			ServiceCharge         any    `json:"service_charge"`
			ServiceTax            any    `json:"service_tax"`
			Adjustment            any    `json:"adjustment"`
			SettlementInitiatedOn string `json:"settlement_initiated_on"`
		} `json:"settlement"`
	} `json:"data"`
	EventTime string `json:"event_time"`
	Type      string `json:"type"`
}

// SettlementWebhookRecord is the parsed, normalized domain-friendly webhook data with integer paise.
type SettlementWebhookRecord struct {
	CFSettlementID        string
	Status                string
	GrossAmountPaise      int64
	NetAmountPaise        int64
	ServiceChargePaise    int64
	ServiceTaxPaise       int64
	AdjustmentPaise       int64
	UTR                   string
	SettledOn             *time.Time
	SettlementInitiatedOn *time.Time
	EventType             string
	RawPayload            []byte
}

// GetOrderSettlements retrieves settlement details for an order using GET /pg/orders/{order_id}/settlements.
// It implements polymorphic unmarshaling for both 2025-01-01 (flat) and 2026-01-01 (nested) schemas.
func (c *Client) GetOrderSettlements(ctx context.Context, orderID string) ([]OrderSettlementRecord, error) {
	if c == nil || !c.cfg.Enabled() {
		return nil, ErrDisabled
	}
	trimmed := strings.TrimSpace(orderID)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty order_id", ErrSettlementNotFound)
	}

	url := fmt.Sprintf("%s/orders/%s/settlements", c.endpoint(), trimmed)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("x-client-id", c.cfg.AppID)
	req.Header.Set("x-client-secret", c.cfg.SecretKey)
	version := c.cfg.APIVersion
	if version == "" {
		version = "2025-01-01"
	}
	req.Header.Set("x-api-version", version)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cashfree order settlements: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read settlement response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: order %s", ErrSettlementNotFound, orderID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d body %s", ErrSettlementFailed, resp.StatusCode, string(bodyBytes))
	}

	return ParseOrderSettlementsJSON(bodyBytes, trimmed)
}

// ParseOrderSettlementsJSON parses raw JSON response from Cashfree order settlements endpoint.
// Supports both a single object or an array of objects, and both flat and nested schemas.
func ParseOrderSettlementsJSON(raw []byte, defaultOrderID string) ([]OrderSettlementRecord, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return nil, nil
	}

	var rawItems []json.RawMessage
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &rawItems); err != nil {
			return nil, fmt.Errorf("unmarshal settlements array: %w", err)
		}
	} else {
		rawItems = []json.RawMessage{json.RawMessage(raw)}
	}

	var records []OrderSettlementRecord
	for _, item := range rawItems {
		rec, err := parseSingleOrderSettlement(item, defaultOrderID)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}

func parseSingleOrderSettlement(raw []byte, fallbackOrderID string) (OrderSettlementRecord, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return OrderSettlementRecord{}, fmt.Errorf("unmarshal settlement json map: %w", err)
	}

	var rec OrderSettlementRecord
	rec.RawPayload = raw

	// Check if nested 2026-01-01 schema
	if ordObj, ok := m["order_details"].(map[string]any); ok {
		if payObj, ok := m["payment_details"].(map[string]any); ok {
			stlmObj, _ := m["settlement_details"].(map[string]any)

			rec.OrderID = getString(ordObj, "order_id")
			rec.GrossAmountPaise = getAmountPaise(ordObj, "order_amount")

			rec.CFPaymentID = coerceString(payObj["cf_payment_id"])
			if rec.GrossAmountPaise == 0 {
				rec.GrossAmountPaise = getAmountPaise(payObj, "payment_amount")
			}
			rec.ServiceChargePaise = getAmountPaise(payObj, "service_charge")
			if rec.ServiceChargePaise == 0 {
				rec.ServiceChargePaise = getAmountPaise(payObj, "pg_service_charge")
			}
			rec.ServiceTaxPaise = getAmountPaise(payObj, "service_tax")
			if rec.ServiceTaxPaise == 0 {
				rec.ServiceTaxPaise = getAmountPaise(payObj, "pg_service_tax")
			}
			rec.AdjustmentPaise = getAmountPaise(payObj, "adjustment")
			rec.NetAmountPaise = getAmountPaise(payObj, "settlement_amount")

			if stlmObj != nil {
				rec.CFSettlementID = coerceString(stlmObj["cf_settlement_id"])
				rec.UTR = coerceString(stlmObj["settlement_utr"])
				rec.Status = getString(stlmObj, "status")
				rec.TransferTime = parseTime(getString(stlmObj, "settlement_processed_on"))
			}
			if rec.OrderID == "" {
				rec.OrderID = fallbackOrderID
			}
			return rec, nil
		}
	}

	// Flat 2025-01-01 schema
	rec.OrderID = getString(m, "order_id")
	if rec.OrderID == "" {
		rec.OrderID = fallbackOrderID
	}
	rec.CFPaymentID = coerceString(m["cf_payment_id"])
	rec.CFSettlementID = coerceString(m["cf_settlement_id"])
	if rec.CFSettlementID == "" {
		rec.CFSettlementID = coerceString(m["settlement_id"])
	}
	rec.UTR = coerceString(m["transfer_id"])
	if rec.UTR == "" {
		rec.UTR = coerceString(m["transfer_utr"])
	}
	if rec.UTR == "" {
		rec.UTR = coerceString(m["utr"])
	}
	rec.Status = getString(m, "status")

	rec.GrossAmountPaise = getAmountPaise(m, "order_amount")
	if rec.GrossAmountPaise == 0 {
		rec.GrossAmountPaise = getAmountPaise(m, "payment_amount")
	}
	rec.NetAmountPaise = getAmountPaise(m, "settlement_amount")
	if rec.NetAmountPaise == 0 {
		rec.NetAmountPaise = getAmountPaise(m, "amount_settled")
	}
	rec.ServiceChargePaise = getAmountPaise(m, "service_charge")
	rec.ServiceTaxPaise = getAmountPaise(m, "service_tax")
	rec.AdjustmentPaise = getAmountPaise(m, "adjustment")

	tStr := getString(m, "transfer_time")
	if tStr == "" {
		tStr = getString(m, "settled_on")
	}
	rec.TransferTime = parseTime(tStr)

	return rec, nil
}

// ParseSettlementWebhook parses Cashfree settlement webhook payloads.
func ParseSettlementWebhook(raw []byte) (*SettlementWebhookRecord, error) {
	var payload SettlementWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal settlement webhook: %w", err)
	}

	s := payload.Data.Settlement
	stlmID := coerceString(s.SettlementID)
	if stlmID == "" {
		return nil, fmt.Errorf("missing settlement_id in webhook payload")
	}

	rec := &SettlementWebhookRecord{
		CFSettlementID:     stlmID,
		Status:             strings.ToUpper(strings.TrimSpace(s.Status)),
		GrossAmountPaise:   coercePaise(s.PaymentAmount),
		NetAmountPaise:     coercePaise(s.AmountSettled),
		ServiceChargePaise: coercePaise(s.ServiceCharge),
		ServiceTaxPaise:    coercePaise(s.ServiceTax),
		AdjustmentPaise:    coercePaise(s.Adjustment),
		UTR:                coerceString(s.UTR),
		EventType:          strings.TrimSpace(payload.Type),
		RawPayload:         raw,
	}

	if s.SettledOn != "" {
		t := parseTime(s.SettledOn)
		if !t.IsZero() {
			rec.SettledOn = &t
		}
	}
	if s.SettlementInitiatedOn != "" {
		t := parseTime(s.SettlementInitiatedOn)
		if !t.IsZero() {
			rec.SettlementInitiatedOn = &t
		}
	}

	return rec, nil
}

func coerceString(val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func getString(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return coerceString(v)
	}
	return ""
}

func getAmountPaise(m map[string]any, k string) int64 {
	val, ok := m[k]
	if !ok || val == nil {
		return 0
	}
	return coercePaise(val)
}

func coercePaise(val any) int64 {
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case string:
		p, err := ParseRupeesToPaise(v)
		if err == nil {
			return p
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p, _ := ParseRupeesToPaise(strconv.FormatFloat(f, 'f', 2, 64))
			return p
		}
		return 0
	case json.Number:
		p, err := ParseRupeesToPaise(v.String())
		if err == nil {
			return p
		}
		return 0
	case float64:
		p, err := ParseRupeesToPaise(strconv.FormatFloat(v, 'f', 2, 64))
		if err == nil {
			return p
		}
		return 0
	case float32:
		p, err := ParseRupeesToPaise(strconv.FormatFloat(float64(v), 'f', 2, 64))
		if err == nil {
			return p
		}
		return 0
	case int64:
		return v * 100
	case int:
		return int64(v) * 100
	default:
		return 0
	}
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05+05:30",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}
