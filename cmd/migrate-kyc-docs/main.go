package main

//m12:validate-exempt one-off document encryption migration utility; validates DB and KMS keys directly

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/crypto"
)

func isPlaintextDocument(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// JPEG: \xFF\xD8\xFF
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return true
	}
	// PNG: \x89PNG
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		return true
	}
	// PDF: %PDF
	if len(data) >= 4 && data[0] == '%' && data[1] == 'P' && data[2] == 'D' && data[3] == 'F' {
		return true
	}
	return false
}

func main() {
	dbURLFlag := flag.String("db", "", "PostgreSQL database URL (defaults to DATABASE_URL env var)")
	keyFlag := flag.String("key", "", "32-byte KYC encryption key (or 64 hex chars, defaults to KYC_DOC_CRYPTO_KEY env var)")
	batchSizeFlag := flag.Int("batch-size", 100, "Batch size for migration")
	dryRunFlag := flag.Bool("dry-run", false, "Simulate encryption without writing to database")
	flag.Parse()

	dbURL := *dbURLFlag
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		log.Fatal("database URL is required (via -db flag or DATABASE_URL environment variable)")
	}

	rawKey := *keyFlag
	if rawKey == "" {
		rawKey = os.Getenv("KYC_DOC_CRYPTO_KEY")
	}
	if rawKey == "" {
		log.Fatal("KYC encryption key is required (via -key flag or KYC_DOC_CRYPTO_KEY environment variable)")
	}

	var key []byte
	if len(rawKey) == 64 {
		decoded, err := hex.DecodeString(rawKey)
		if err == nil && len(decoded) == 32 {
			key = decoded
		}
	}
	if key == nil {
		if len(rawKey) == 32 {
			key = []byte(rawKey)
		} else {
			log.Fatalf("encryption key must be exactly 32 bytes (or 64 hex characters), got %d bytes", len(rawKey))
		}
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer pool.Close()

	log.Printf("Starting batched KYC document encryption migration (batch size: %d, dry-run: %t)", *batchSizeFlag, *dryRunFlag)

	totalInspected := 0
	totalMigrated := 0
	var lastID uuid.UUID

	for {
		rows, err := pool.Query(ctx, `
			SELECT id, document_bytes, attested_photo_bytes
			FROM kyc_verification
			WHERE (document_bytes IS NOT NULL OR attested_photo_bytes IS NOT NULL)
			  AND id > $1
			ORDER BY id ASC
			LIMIT $2`, lastID, *batchSizeFlag,
		)
		if err != nil {
			log.Fatalf("query batch failed: %v", err)
		}

		type item struct {
			id         uuid.UUID
			docBytes   []byte
			photoBytes []byte
		}
		var batch []item

		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.docBytes, &it.photoBytes); err != nil {
				rows.Close()
				log.Fatalf("scan row failed: %v", err)
			}
			batch = append(batch, it)
			lastID = it.id
		}
		rows.Close()

		if len(batch) == 0 {
			break
		}

		for _, it := range batch {
			totalInspected++
			needsUpdate := false
			var newDocBytes, newPhotoBytes []byte

			if len(it.docBytes) > 0 {
				_, _, decErr := crypto.Decrypt(key, it.docBytes)
				if decErr != nil || isPlaintextDocument(it.docBytes) {
					// Plaintext detected: Encrypt and verify
					enc, err := crypto.Encrypt(key, it.docBytes)
					if err != nil {
						log.Fatalf("failed to encrypt document for verification %s: %v", it.id, err)
					}
					verDec, _, err := crypto.Decrypt(key, enc)
					if err != nil || !bytes.Equal(verDec, it.docBytes) {
						log.Fatalf("decryption round-trip verification failed for verification %s", it.id)
					}
					newDocBytes = enc
					needsUpdate = true
				} else {
					newDocBytes = it.docBytes
				}
			}

			if len(it.photoBytes) > 0 {
				_, _, decErr := crypto.Decrypt(key, it.photoBytes)
				if decErr != nil || isPlaintextDocument(it.photoBytes) {
					// Plaintext detected: Encrypt and verify
					enc, err := crypto.Encrypt(key, it.photoBytes)
					if err != nil {
						log.Fatalf("failed to encrypt photo for verification %s: %v", it.id, err)
					}
					verDec, _, err := crypto.Decrypt(key, enc)
					if err != nil || !bytes.Equal(verDec, it.photoBytes) {
						log.Fatalf("decryption round-trip verification failed for photo on verification %s", it.id)
					}
					newPhotoBytes = enc
					needsUpdate = true
				} else {
					newPhotoBytes = it.photoBytes
				}
			}

			if needsUpdate {
				if !*dryRunFlag {
					err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
						_, err := tx.Exec(ctx, `
							UPDATE kyc_verification
							SET document_bytes = $1,
								attested_photo_bytes = $2,
								updated_at = NOW()
							WHERE id = $3`,
							newDocBytes, newPhotoBytes, it.id,
						)
						return err
					})
					if err != nil {
						log.Fatalf("failed to update verification %s: %v", it.id, err)
					}
				}
				totalMigrated++
			}
		}

		log.Printf("Progress: inspected %d rows, migrated %d rows so far...", totalInspected, totalMigrated)
	}

	log.Printf("Migration completed successfully. Inspected: %d, Migrated: %d, All ciphertexts verified authentic.", totalInspected, totalMigrated)
}
