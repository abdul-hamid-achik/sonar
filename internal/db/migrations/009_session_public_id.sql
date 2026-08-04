-- User-facing session handles are independent random mini-hashes.
-- Internal foreign keys and execution ledgers continue to use sessions.id.
ALTER TABLE sessions ADD COLUMN public_id TEXT NOT NULL DEFAULT '';
