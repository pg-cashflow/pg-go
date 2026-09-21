-- Lexical search support (pg_trgm + targeted indexes).

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS idx_tenants_name_trgm ON tenants USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_tenants_room_trgm ON tenants USING gin (room_number gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_tenants_phone_trgm ON tenants USING gin (phone gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_join_requests_name_trgm ON join_requests USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_join_requests_phone_trgm ON join_requests USING gin (phone gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_payments_upi_trgm ON payments USING gin (upi_txn_id gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_payment_reports_upi_trgm ON payment_reports USING gin (upi_txn_id gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_events_type_trgm ON events USING gin (event_type gin_trgm_ops);
