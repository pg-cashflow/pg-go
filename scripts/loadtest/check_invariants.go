//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

type BaselineState struct {
	Timestamp            time.Time `json:"timestamp"`
	DebitsPaise          int64     `json:"debits_paise"`
	CreditsPaise         int64     `json:"credits_paise"`
	PaymentCount         int64     `json:"payment_count"`
	JournalEntryCount    int64     `json:"journal_entry_count"`
	Deadlocks            int64     `json:"deadlocks"`
	UnmatchedOrdersCount int64     `json:"unmatched_orders_count"`
}

func main() {
	_ = godotenv.Load()

	snapshotPath := flag.String("snapshot", "", "Path to write baseline state JSON snapshot before test")
	assertDeltaPath := flag.String("assert-delta", "", "Path to baseline state JSON snapshot to assert positive delta against")
	requireActivity := flag.Bool("require-activity", false, "Require at least one new payment or journal entry to have been processed")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	// If non-flag argument provided, use it as DSN
	for _, arg := range flag.Args() {
		if arg != "" {
			dsn = arg
			break
		}
	}

	if dsn == "" {
		log.Fatal("DATABASE_URL is required to run invariant verification")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(ctx)

	// Fetch current state
	var debits, credits, pmtCount, journalCount, deadlocks, unmatchedCount int64
	_ = conn.QueryRow(ctx, `SELECT COALESCE(SUM(debit_paise), 0), COALESCE(SUM(credit_paise), 0), COUNT(*) FROM financial_journal_entries;`).Scan(&debits, &credits, &journalCount)
	_ = conn.QueryRow(ctx, `SELECT COUNT(*) FROM payments;`).Scan(&pmtCount)
	_ = conn.QueryRow(ctx, `SELECT COALESCE(deadlocks, 0) FROM pg_stat_database WHERE datname = current_database();`).Scan(&deadlocks)
	_ = conn.QueryRow(ctx, `SELECT COUNT(*) FROM unmatched_receipts;`).Scan(&unmatchedCount)

	currState := BaselineState{
		Timestamp:            time.Now().UTC(),
		DebitsPaise:          debits,
		CreditsPaise:         credits,
		PaymentCount:         pmtCount,
		JournalEntryCount:    journalCount,
		Deadlocks:            deadlocks,
		UnmatchedOrdersCount: unmatchedCount,
	}

	// 1. Snapshot mode
	if *snapshotPath != "" {
		data, err := json.MarshalIndent(currState, "", "  ")
		if err != nil {
			log.Fatalf("Failed to serialize baseline snapshot: %v", err)
		}
		if err := os.WriteFile(*snapshotPath, data, 0644); err != nil {
			log.Fatalf("Failed to write baseline snapshot file: %v", err)
		}
		fmt.Printf("[SNAPSHOT] Baseline state recorded to %s (Payments=%d, JournalEntries=%d, Debits=%d, Credits=%d)\n",
			*snapshotPath, pmtCount, journalCount, debits, credits)
		return
	}

	fmt.Println("=============================================================")
	fmt.Println("  PG Cashflow: Running Post-Load Stress SQL Invariants Check ")
	fmt.Println("=============================================================")

	hasFailure := false

	// Invariant 1: Double-Entry Debit/Credit Conservation
	drift := debits - credits
	if drift != 0 {
		fmt.Printf("[FAIL] Invariant 1: Ledger drift detected! Debits=%d, Credits=%d, Drift=%d paise\n", debits, credits, drift)
		hasFailure = true
	} else {
		fmt.Printf("[PASS] Invariant 1: Ledger double-entry balance preserved (Debits=%d, Credits=%d, Drift=0 paise)\n", debits, credits)
	}

	// Invariant 2: Per-Source Balanced Entries
	var unbalancedSources int64
	err = conn.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM (
			SELECT source_type, source_id
			FROM financial_journal_entries
			GROUP BY source_type, source_id
			HAVING SUM(debit_paise) <> SUM(credit_paise)
		) sub;
	`).Scan(&unbalancedSources)
	if err != nil {
		log.Fatalf("Query Invariant 2 failed: %v", err)
	}
	if unbalancedSources > 0 {
		fmt.Printf("[FAIL] Invariant 2: %d unbalanced journal source transactions found!\n", unbalancedSources)
		hasFailure = true
	} else {
		fmt.Println("[PASS] Invariant 2: All journal transactions strictly balanced per source")
	}

	// Invariant 3: Zero Duplicate Gateway Payments
	var duplicateCfPmts int64
	err = conn.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM (
			SELECT provider, provider_payment_id
			FROM payments
			WHERE provider_payment_id IS NOT NULL AND provider_payment_id <> ''
			GROUP BY provider, provider_payment_id
			HAVING COUNT(*) > 1
		) sub;
	`).Scan(&duplicateCfPmts)
	if err != nil {
		log.Fatalf("Query Invariant 3 failed: %v", err)
	}
	if duplicateCfPmts > 0 {
		fmt.Printf("[FAIL] Invariant 3: %d duplicate gateway provider_payment_id entries recorded!\n", duplicateCfPmts)
		hasFailure = true
	} else {
		fmt.Println("[PASS] Invariant 3: Zero duplicate provider payments recorded")
	}

	// Invariant 4: Zero Duplicate UTRs
	var duplicateUTRs int64
	err = conn.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM (
			SELECT upi_txn_id
			FROM payments
			WHERE upi_txn_id IS NOT NULL AND upi_txn_id <> ''
			GROUP BY upi_txn_id
			HAVING COUNT(*) > 1
		) sub;
	`).Scan(&duplicateUTRs)
	if err != nil {
		log.Fatalf("Query Invariant 4 failed: %v", err)
	}
	if duplicateUTRs > 0 {
		fmt.Printf("[FAIL] Invariant 4: %d duplicate bank UTRs recorded in payments!\n", duplicateUTRs)
		hasFailure = true
	} else {
		fmt.Println("[PASS] Invariant 4: Zero duplicate bank UTRs recorded")
	}

	// Invariant 5: Zero Overpaid Dues
	var overpaidDues int64
	err = conn.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM (
			SELECT d.id, d.original_amount, COALESCE(SUM(p.amount), 0) AS total_paid
			FROM dues d
			JOIN payments p ON p.due_id = d.id
			GROUP BY d.id, d.original_amount
			HAVING COALESCE(SUM(p.amount), 0) > d.original_amount
		) sub;
	`).Scan(&overpaidDues)
	if err != nil {
		log.Fatalf("Query Invariant 5 failed: %v", err)
	}
	if overpaidDues > 0 {
		fmt.Printf("[FAIL] Invariant 5: %d dues have payments exceeding original_amount!\n", overpaidDues)
		hasFailure = true
	} else {
		fmt.Println("[PASS] Invariant 5: Zero overpaid dues detected")
	}

	// Invariant 6: Deadlocks Stat Check (Fatal on new deadlocks)
	if *assertDeltaPath != "" {
		baseData, err := os.ReadFile(*assertDeltaPath)
		if err != nil {
			log.Fatalf("Failed to read baseline file %s: %v", *assertDeltaPath, err)
		}
		var base BaselineState
		if err := json.Unmarshal(baseData, &base); err != nil {
			log.Fatalf("Failed to parse baseline file: %v", err)
		}

		newDeadlocks := deadlocks - base.Deadlocks
		if newDeadlocks > 0 {
			fmt.Printf("[FAIL] Invariant 6: %d new deadlocks occurred during load test!\n", newDeadlocks)
			hasFailure = true
		} else {
			fmt.Println("[PASS] Invariant 6: Zero database deadlocks during test run")
		}

		// Delta checks
		newPayments := pmtCount - base.PaymentCount
		newEntries := journalCount - base.JournalEntryCount
		deltaDebits := debits - base.DebitsPaise
		deltaCredits := credits - base.CreditsPaise

		fmt.Println("-------------------------------------------------------------")
		fmt.Println("  Delta Execution Verification (Pre vs Post Run)            ")
		fmt.Println("-------------------------------------------------------------")
		fmt.Printf("  Payments Added:       +%d\n", newPayments)
		fmt.Printf("  Journal Entries:      +%d\n", newEntries)
		fmt.Printf("  Debits Delta:         +%d paise\n", deltaDebits)
		fmt.Printf("  Credits Delta:        +%d paise\n", deltaCredits)

		if deltaDebits != deltaCredits {
			fmt.Printf("[FAIL] Delta Drift: Delta Debits (%d) != Delta Credits (%d)\n", deltaDebits, deltaCredits)
			hasFailure = true
		} else {
			fmt.Printf("[PASS] Delta Ledger Balance: 0 drift across %d new transactions\n", newEntries)
		}

		if *requireActivity && newPayments <= 0 && newEntries <= 0 {
			fmt.Println("[FAIL] Invariant Growth: Zero payments or journal entries were recorded during the test! The test did not exercise the ledger.")
			hasFailure = true
		}
	} else {
		if deadlocks > 0 {
			fmt.Printf("[WARN] Invariant 6: %d cumulative database deadlocks recorded on database\n", deadlocks)
		} else {
			fmt.Println("[PASS] Invariant 6: Zero database deadlocks recorded")
		}
	}

	fmt.Println("=============================================================")
	if hasFailure {
		fmt.Println("  FAILED: One or more critical financial invariants were violated!")
		fmt.Println("=============================================================")
		os.Exit(1)
	}

	fmt.Println("  SUCCESS: All Gate 09 Post-Load Invariants Passed Strictly! ")
	fmt.Println("=============================================================")
}
