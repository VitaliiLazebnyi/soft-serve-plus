DELETE FROM settings WHERE substr("key", 1, 18) = 'initial_admin_key:';
INSERT INTO settings ("key", value, updated_at)
  SELECT 'initial_admin_key:' || public_key, CAST(synced_user_id AS TEXT), CURRENT_TIMESTAMP
  FROM public_keys WHERE synced_user_id IS NOT NULL;
ALTER TABLE public_keys DROP COLUMN synced_user_id;
