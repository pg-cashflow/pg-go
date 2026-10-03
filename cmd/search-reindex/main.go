package main

// search-reindex is deprecated.
//
// ADR-012: search_documents (vector/hybrid search) has been permanently disabled.
// Search V2 queries live tables directly using trigram indexes (pg_trgm).
// No reindexing is required or supported.
//
// Usage: this binary now exits immediately with a deprecation message.
import "log"

func main() {
	log.Println("[DEPRECATED] search-reindex: ADR-012 decommissioned vector search. No action taken.")
	log.Println("Search V2 operates directly on live tables (tenants, dues, payments, ...) via pg_trgm indexes.")
}
