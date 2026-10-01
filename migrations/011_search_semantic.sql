-- Search documents. Lexical search (ILIKE on title/body) needs only this table.
-- Vector search is optional: the pgvector extension, the embedding column and the
-- HNSW index are created only when pgvector is installed on the server.

CREATE TABLE IF NOT EXISTS search_documents (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  tenant_id    UUID REFERENCES tenants(id) ON DELETE CASCADE,
  entity_type  VARCHAR(40) NOT NULL,
  entity_id    UUID NOT NULL,
  title        VARCHAR(200) NOT NULL,
  body         TEXT NOT NULL DEFAULT '',
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_search_documents_entity UNIQUE (property_id, entity_type, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_search_documents_property ON search_documents(property_id);
CREATE INDEX IF NOT EXISTS idx_search_documents_tenant ON search_documents(tenant_id) WHERE tenant_id IS NOT NULL;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
    CREATE EXTENSION IF NOT EXISTS vector;
    ALTER TABLE search_documents ADD COLUMN IF NOT EXISTS embedding vector(384);
    CREATE INDEX IF NOT EXISTS idx_search_documents_embedding ON search_documents
      USING hnsw (embedding vector_cosine_ops)
      WHERE embedding IS NOT NULL;
  END IF;
EXCEPTION
  WHEN insufficient_privilege THEN
    RAISE NOTICE 'pgvector is available but this role cannot create it; vector search stays disabled';
END $$;
