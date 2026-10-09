//go:build ignore

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
	_ = godotenv.Load(".env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is not set in .env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(ctx)

	fmt.Println("=== 1. Terminating any stale lock-holding sessions ===")
	_, _ = conn.Exec(ctx, `
		SELECT pg_terminate_backend(pid) 
		FROM pg_stat_activity 
		WHERE pid <> pg_backend_pid() 
		  AND datname = current_database()
		  AND (state = 'idle in transaction' OR (state = 'active' AND query ILIKE '%payment%'));
	`)

	fmt.Println("=== 2. Cleaning orphaned payment_allocations via fast indexed NOT EXISTS ===")
	// Orphan due_id
	tagDues, err := conn.Exec(ctx, `
		DELETE FROM payment_allocations pa
		WHERE NOT EXISTS (SELECT 1 FROM dues d WHERE d.id = pa.due_id);
	`)
	if err != nil {
		log.Printf("Error deleting payment_allocations with missing due_id: %v", err)
	} else {
		fmt.Printf("✓ Removed %d payment_allocations with missing due_id\n", tagDues.RowsAffected())
	}

	// Orphan payment_id
	tagPayments, err := conn.Exec(ctx, `
		DELETE FROM payment_allocations pa
		WHERE NOT EXISTS (SELECT 1 FROM payments p WHERE p.id = pa.payment_id);
	`)
	if err != nil {
		log.Printf("Error deleting payment_allocations with missing payment_id: %v", err)
	} else {
		fmt.Printf("✓ Removed %d payment_allocations with missing payment_id\n", tagPayments.RowsAffected())
	}

	// 3. Delete unmirrored test departures (clearing referencing test payout_items first)
	_, _ = conn.Exec(ctx, `
		DELETE FROM payout_items 
		WHERE departure_id IN (
			SELECT d.id FROM tenant_departures d
			WHERE d.status IN ('approved', 'refunded') 
			  AND NOT EXISTS (
			      SELECT 1 FROM financial_journal_entries fje 
			      WHERE fje.source_type = 'departure_settlement' 
			        AND fje.source_id = d.id
			  )
		);
	`)
	tagDep, err := conn.Exec(ctx, `
		DELETE FROM tenant_departures d
		WHERE d.status IN ('approved', 'refunded') 
			AND NOT EXISTS (
			    SELECT 1 FROM financial_journal_entries fje 
			    WHERE fje.source_type = 'departure_settlement' 
			      AND fje.source_id = d.id
			);
	`)
	if err != nil {
		log.Printf("Error cleaning tenant_departures: %v", err)
	} else {
		fmt.Printf("✓ Removed %d unmirrored test tenant_departures\n", tagDep.RowsAffected())
	}

	fmt.Println("=== 4. Cleaning dead-lettered and pending test ledger_outbox_events ===")
	// Delete outbox events associated with test properties or simulated errors
	tagOutbox, err := conn.Exec(ctx, `
		DELETE FROM ledger_outbox_events
		WHERE last_error IN ('simulated mirror ledger failure', 'payout batch dispatcher not configured')
		   OR property_id IN (
		       SELECT id FROM properties 
		       WHERE name ILIKE '%test%' 
		          OR name ILIKE '%noise%'
		          OR name ILIKE '%prop%'
		   )
		   OR NOT EXISTS (SELECT 1 FROM properties p WHERE p.id = ledger_outbox_events.property_id);
	`)
	if err != nil {
		log.Printf("Error cleaning test ledger_outbox_events: %v", err)
	} else {
		fmt.Printf("✓ Removed %d test/dead-letter ledger_outbox_events\n", tagOutbox.RowsAffected())
	}

	fmt.Println("=== Database cleanup finished successfully! ===")
}
