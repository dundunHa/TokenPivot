-- Source archival (T-054). Archiving keeps the row, its events, minute stats and
-- credentials; only this nullable marker is added. No default, backfill, index,
-- foreign key or cascade: existing rows are active.
-- +goose Up
ALTER TABLE sources ADD COLUMN archived_at timestamptz;
COMMENT ON COLUMN sources.archived_at IS 'NULL means active; otherwise the time the source was first archived. Archiving also revokes its active credentials but never deletes history.';

-- +goose Down
ALTER TABLE sources DROP COLUMN archived_at;
