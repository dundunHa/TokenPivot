-- Client-observed generation timings (T-061). Two nullable raw millisecond
-- columns: the decode span between the first and the last effective output, and
-- the first-token latency from request start. Both carry no default, are never
-- backfilled and are not indexed: generation speed is derived at query time from
-- output_tokens and generation_duration_ms, so no stored ratio can drift. Value
-- ranges and the meaning of a real zero are enforced by the ingestion entry
-- point, exactly like the other outcome columns: no CHECK, foreign key, trigger
-- or RLS. Existing rows keep NULL, which means unknown rather than zero.
-- +goose Up
ALTER TABLE usage_events ADD COLUMN generation_duration_ms bigint;
ALTER TABLE usage_events ADD COLUMN ttft_ms bigint;
COMMENT ON COLUMN usage_events.generation_duration_ms IS 'Client observed last effective output time minus first effective output time, in milliseconds. NULL means unknown: an older row, a missing output boundary or an invalid span. 0 is a real observation of a single-chunk output and is kept, but it never yields a generation speed.';
COMMENT ON COLUMN usage_events.ttft_ms IS 'Client observed first effective output time minus request start, in milliseconds. NULL means unknown: an older row or a missing boundary. 0 is a real observation of a first output in the same millisecond as the request start.';

-- +goose Down
ALTER TABLE usage_events DROP COLUMN ttft_ms;
ALTER TABLE usage_events DROP COLUMN generation_duration_ms;
