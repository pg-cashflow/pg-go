package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
)

func maskString(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("❌ Failed to load configuration: %v", err)
	}

	fmt.Println("==================================================")
	fmt.Println("  Cashfree Sandbox Integration Smoke Test (Gate 4)")
	fmt.Println("==================================================")

	appID := cfg.CashfreePGAppID
	secretKey := cfg.CashfreePGSecretKey
	env := cfg.CashfreeEnv
	if env == "" {
		env = "sandbox"
	}

	if appID == "" || secretKey == "" {
		fmt.Println("⚠️  Cashfree PG credentials not configured in environment or .env")
		fmt.Printf("   CASHFREE_PG_APP_ID:     %s\n", maskString(appID))
		fmt.Printf("   CASHFREE_PG_SECRET_KEY: %s\n", maskString(secretKey))
		os.Exit(1)
	}

	fmt.Printf("✓ Environment:  %s\n", env)
	fmt.Printf("✓ App ID:       %s (length=%d)\n", maskString(appID), len(appID))
	fmt.Printf("✓ Secret Key:   %s (length=%d)\n", maskString(secretKey), len(secretKey))
	fmt.Printf("✓ API Version:  %s\n", cfg.WebhookAPIVersion)
	fmt.Printf("✓ Expiry Dur:   %v\n\n", cfg.OrderExpiryDuration)

	cfConfig := cashfree.Config{
		AppID:               appID,
		SecretKey:           secretKey,
		Env:                 env,
		APIVersion:          cfg.WebhookAPIVersion,
		OrderExpiryDuration: cfg.OrderExpiryDuration,
	}

	client := cashfree.NewClient(cfConfig)

	// Step 1: Live Sandbox Order Creation
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	smokeOrderID := fmt.Sprintf("smoke_%d", time.Now().Unix())
	smokeAmountPaise := int64(100) // ₹1.00
	customerPhone := "+919999999999"
	note := "Gate 4 Sandbox Smoke Test"

	fmt.Printf("[1/3] Creating live sandbox UPI order %s (₹1.00)...\n", smokeOrderID)
	sessionID, exp, err := client.CreateUPIOrder(ctx, smokeOrderID, smokeAmountPaise, customerPhone, note)
	if err != nil {
		log.Fatalf("❌ Order creation failed: %v", err)
	}

	fmt.Printf("  ✓ Success! Payment Session ID: %s\n", maskString(sessionID))
	if exp != nil {
		fmt.Printf("  ✓ Order expires at: %s (in ~%.0fm)\n", exp.Format(time.RFC3339), time.Until(*exp).Minutes())
	}

	// Step 2: Query Order Payment Status Endpoint
	fmt.Println("\n[2/3] Checking payment query endpoint for newly created order...")
	_, _, _, ok, err := client.FetchSuccessfulPayment(ctx, smokeOrderID)
	if err != nil {
		log.Fatalf("❌ Payment query failed: %v", err)
	}
	if ok {
		fmt.Println("  ✓ Payment already marked success (unexpected for fresh order, but response parsed validly)")
	} else {
		fmt.Println("  ✓ Status checked: Payment not yet completed (expected for unpaid order)")
	}

	// Step 3: Webhook HMAC Signature & Parsing Verification
	fmt.Println("\n[3/3] Validating Webhook HMAC-SHA256 signature verification & parser...")
	webhookSecret := cfg.CashfreeWebhookSecret
	if webhookSecret == "" {
		webhookSecret = secretKey
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	rawPayload := fmt.Sprintf(`{"type":"PAYMENT_SUCCESS_WEBHOOK","event_time":"%s","data":{"order":{"order_id":"%s","order_amount":1.00,"order_currency":"INR"},"payment":{"cf_payment_id":999999,"payment_status":"SUCCESS","payment_amount":1.00,"payment_currency":"INR","payment_message":"Transaction Successful","payment_time":"%s","bank_reference":"123456789012"}}}`, time.Now().UTC().Format(time.RFC3339), smokeOrderID, time.Now().UTC().Format(time.RFC3339))

	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(ts + rawPayload))
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Verify valid signature
	if !cashfree.VerifyWebhookHMAC(webhookSecret, ts, rawPayload, validSig) {
		log.Fatalf("❌ Valid webhook HMAC signature was falsely rejected")
	}
	fmt.Println("  ✓ Valid HMAC signature accepted")

	// Verify tampered signature is rejected
	tamperedSig := validSig[:len(validSig)-4] + "AAAA"
	if cashfree.VerifyWebhookHMAC(webhookSecret, ts, rawPayload, tamperedSig) {
		log.Fatalf("❌ Tampered webhook signature was falsely accepted")
	}
	fmt.Println("  ✓ Tampered HMAC signature correctly rejected")

	// Verify parser
	evt, evtType, err := cashfree.ParseWebhook([]byte(rawPayload))
	if err != nil {
		log.Fatalf("❌ Webhook parser failed: %v", err)
	}
	if evtType != "PAYMENT_SUCCESS_WEBHOOK" {
		log.Fatalf("❌ Expected event type PAYMENT_SUCCESS_WEBHOOK, got %s", evtType)
	}
	succ, ok := evt.(cashfree.SuccessWebhook)
	if !ok || succ.OrderID != smokeOrderID || succ.AmountPaise != 100 {
		log.Fatalf("❌ Parsed webhook data mismatch: %+v", evt)
	}
	fmt.Printf("  ✓ Webhook payload successfully parsed: order_id=%s amount=%d paise status=%s\n", succ.OrderID, succ.AmountPaise, succ.PaymentStatus)

	fmt.Println("\n==================================================")
	fmt.Println("  ✅ ALL GATE 4 CASHFREE SANDBOX CHECKS PASSED!   ")
	fmt.Println("==================================================")
}
