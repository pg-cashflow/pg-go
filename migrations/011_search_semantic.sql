-- Semantic search documents (pgvector). Hybrid mode fuses lexical + vector ranks (RRF).

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS search_documents (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  tenant_id    UUID REFERENCES tenants(id) ON DELETE CASCADE,
  entity_type  VARCHAR(40) NOT NULL,
  entity_id    UUID NOT NULL,
  title        VARCHAR(200) NOT NULL,
  body         TEXT NOT NULL DEFAULT '',
  embedding    vector(384),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_search_documents_entity UNIQUE (property_id, entity_type, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_search_documents_property ON search_documents(property_id);
CREATE INDEX IF NOT EXISTS idx_search_documents_tenant ON search_documents(tenant_id) WHERE tenant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_search_documents_embedding ON search_documents
  USING hnsw (embedding vector_cosine_ops)
  WHERE embedding IS NOT NULL;
