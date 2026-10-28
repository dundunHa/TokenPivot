-- Additive per-minute usage statistics (T-012). One row per UTC minute bucket and
-- business dimension. Rows are only ever incremented by the caller's transaction;
-- referential integrity, value ranges and quality vocabulary are enforced by the
-- application: no foreign keys, CHECK, triggers or RLS.
-- +goose Up
CREATE TABLE usage_minute_stats (
    workspace_id uuid NOT NULL,
    bucket_start timestamptz NOT NULL,
    source_id uuid NOT NULL,
    project_id uuid NULL,
    provider text NOT NULL,
    model text NOT NULL,
    event_count bigint NOT NULL,

    input_tokens_sum bigint NOT NULL,
    input_tokens_known_count bigint NOT NULL,
    input_tokens_estimated_count bigint NOT NULL,
    input_tokens_missing_count bigint NOT NULL,
    output_tokens_sum bigint NOT NULL,
    output_tokens_known_count bigint NOT NULL,
    output_tokens_estimated_count bigint NOT NULL,
    output_tokens_missing_count bigint NOT NULL,
    total_tokens_sum bigint NOT NULL,
    total_tokens_known_count bigint NOT NULL,
    total_tokens_estimated_count bigint NOT NULL,
    total_tokens_missing_count bigint NOT NULL,
    cache_read_tokens_sum bigint NOT NULL,
    cache_read_tokens_known_count bigint NOT NULL,
    cache_read_tokens_estimated_count bigint NOT NULL,
    cache_read_tokens_missing_count bigint NOT NULL,
    cache_write_tokens_sum bigint NOT NULL,
    cache_write_tokens_known_count bigint NOT NULL,
    cache_write_tokens_estimated_count bigint NOT NULL,
    cache_write_tokens_missing_count bigint NOT NULL,
    reasoning_tokens_sum bigint NOT NULL,
    reasoning_tokens_known_count bigint NOT NULL,
    reasoning_tokens_estimated_count bigint NOT NULL,
    reasoning_tokens_missing_count bigint NOT NULL,

    CONSTRAINT usage_minute_stats_key UNIQUE NULLS NOT DISTINCT (
        workspace_id, bucket_start, source_id, project_id, provider, model
    )
);
COMMENT ON TABLE usage_minute_stats IS 'Additive per-minute usage statistics, incremented only in the transaction that first inserted the detail event. The service role holds only SELECT, INSERT and UPDATE.';
COMMENT ON COLUMN usage_minute_stats.bucket_start IS 'UTC minute start of occurred_at. Never received_at, never a session time zone.';
COMMENT ON COLUMN usage_minute_stats.project_id IS 'NULL means the events are not attributed to a project; the unique constraint treats those rows as one bucket, not as distinct.';
COMMENT ON COLUMN usage_minute_stats.event_count IS 'Every accepted request-level event increments this by exactly one, whether its token values are real zeros or entirely missing.';
COMMENT ON COLUMN usage_minute_stats.input_tokens_sum IS 'Every metric column group holds the sum of known and estimated values plus the known, estimated and missing counts; missing contributes no value.';
GRANT SELECT, INSERT, UPDATE ON usage_minute_stats TO tokenpivot_app;

-- +goose Down
DROP TABLE usage_minute_stats;
