-- Index the foreign-key columns that no existing index leads with. Neither
-- engine indexes them on its own, so looking rows up by them, and the cascade
-- when a user or repository is deleted, scanned the whole table.
CREATE INDEX IF NOT EXISTS public_keys_user_id_idx ON public_keys (user_id);
CREATE INDEX IF NOT EXISTS repos_user_id_idx ON repos (user_id);
CREATE INDEX IF NOT EXISTS collabs_repo_id_idx ON collabs (repo_id);
CREATE INDEX IF NOT EXISTS lfs_objects_repo_id_idx ON lfs_objects (repo_id);
CREATE INDEX IF NOT EXISTS lfs_locks_user_id_idx ON lfs_locks (user_id);
CREATE INDEX IF NOT EXISTS access_tokens_user_id_idx ON access_tokens (user_id);
