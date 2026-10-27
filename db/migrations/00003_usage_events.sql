-- Immutable per-request final usage events (T-009). The unique identity key is
-- the only duplicate arbiter. Reference integrity, value ranges and the quality
-- vocabulary are enforced by the application: no foreign keys, CHECK,
-- triggers or RLS.
-- +goose Up
CREATE TABLE usage_events (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    source_id uuid NOT NULL,
    source_event_id text COLLATE "C" NOT NULL,
    project_id uuid,
    provider text NOT NULL,
    model text NOT NULL,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    normalization_version text NOT NULL,
    payload_hash bytea NOT NULL,
    input_tokens bigint,
    input_tokens_quality text NOT NULL,
    output_tokens bigint,
    output_tokens_quality text NOT NULL,
    total_tokens bigint,
    total_tokens_quality text NOT NULL,
    cache_read_tokens bigint,
    cache_read_tokens_quality text NOT NULL,
    cache_write_tokens bigint,
    cache_write_tokens_quality text NOT NULL,
    reasoning_tokens bigint,
    reasoning_tokens_quality text NOT NULL,
    status text,
    duration_ms bigint,
    CONSTRAINT uk_usage_events_workspace_id_source_id_source_event_id UNIQUE (workspace_id, source_id, source_event_id)
);
CREATE INDEX idx_usage_events_workspace_id_occurred_at_id ON usage_events (workspace_id, occurred_at, id);
CREATE INDEX idx_usage_events_received_at ON usage_events (received_at);
COMMENT ON TABLE usage_events IS 'Immutable final per-request usage events. The service role holds only SELECT and INSERT, and the first received_at is never renewed.';
COMMENT ON COLUMN usage_events.project_id IS 'NULL means the event is not attributed to a project. An invalid or cross-workspace project is rejected by the application and is never stored as NULL.';
COMMENT ON COLUMN usage_events.received_at IS 'Server receive time sampled once when the event was first accepted. Retransmissions never update it, and it is the 180 day detail retention cut-off.';
COMMENT ON COLUMN usage_events.normalization_version IS 'Normalization algorithm version that payload_hash and the typed comparison columns belong to. Cross-version comparison must rebuild a common version from the stored columns.';
COMMENT ON COLUMN usage_events.payload_hash IS 'Raw SHA-256 digest of the normalized business content, compared bytewise. The algorithm and the compared field set are identified by normalization_version.';
COMMENT ON COLUMN usage_events.input_tokens_quality IS 'Quality of input_tokens, one of known, estimated or missing. input_tokens is NULL exactly when the quality is missing.';
COMMENT ON COLUMN usage_events.output_tokens_quality IS 'Quality of output_tokens, one of known, estimated or missing. output_tokens is NULL exactly when the quality is missing.';
COMMENT ON COLUMN usage_events.total_tokens_quality IS 'Quality of total_tokens, one of known, estimated or missing. total_tokens is NULL exactly when the quality is missing.';
COMMENT ON COLUMN usage_events.cache_read_tokens_quality IS 'Quality of cache_read_tokens, one of known, estimated or missing. cache_read_tokens is NULL exactly when the quality is missing.';
COMMENT ON COLUMN usage_events.cache_write_tokens_quality IS 'Quality of cache_write_tokens, one of known, estimated or missing. cache_write_tokens is NULL exactly when the quality is missing.';
COMMENT ON COLUMN usage_events.reasoning_tokens_quality IS 'Quality of reasoning_tokens, one of known, estimated or missing. reasoning_tokens is NULL exactly when the quality is missing.';
GRANT SELECT, INSERT ON usage_events TO tokenpivot_app;

-- +goose Down
DROP TABLE usage_events;
