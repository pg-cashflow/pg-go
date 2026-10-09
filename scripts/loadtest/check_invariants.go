package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	dsn := os.Getenv("DATABASE_URL")
	if len(os.Args) > 1 && os.Args[1] != "" {
		dsn = os.Args[1]
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

	fmt.Println("=============================================================")
	fmt.Println("  PG Cashflow: Running Post-Load Stress SQL Invariants Check ")
	fmt.Println("=============================================================")

	hasFailure := false

	// Invariant 1: Double-Entry Debit/Credit Conservation
	var debitTotal, creditTotal int64
	err = conn.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise), 0), COALESCE(SUM(credit_paise), 0)
		FROM financial_journal_entries;
	`).Scan(&debitTotal, &creditTotal)
	if err != nil {
		log.Fatalf("Query Invariant 1 failed: %v", err)
	}

	drift := debitTotal - creditTotal
	if drift != 0 {
		fmt.Printf("[FAIL] Invariant 1: Ledger drift detected! Debits=%d, Credits=%d, Drift=%d paise\n", debitTotal, creditTotal, drift)
		hasFailure = true
	} else {
		fmt.Printf("[PASS] Invariant 1: Ledger double-entry balance preserved (Debits=%d, Credits=%d, Drift=0 paise)\n", debitTotal, creditTotal)
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

	// Invariant 6: Deadlocks Stat Check
	var deadlocks int64
	err = conn.QueryRow(ctx, `
		SELECT COALESCE(deadlocks, 0)
		FROM pg_stat_database
		WHERE datname = current_database();
	`).Scan(&deadlocks)
	if err != nil {
		log.Printf("[WARN] Invariant 6: Could not query pg_stat_database: %v", err)
	} else if deadlocks > 0 {
		fmt.Printf("[WARN] Invariant 6: %d database deadlocks recorded\n", deadlocks)
	} else {
		fmt.Println("[PASS] Invariant 6: Zero database deadlocks recorded")
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
