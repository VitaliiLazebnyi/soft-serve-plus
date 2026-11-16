ALTER TABLE public_keys ADD COLUMN synced_user_id INTEGER;
UPDATE public_keys SET synced_user_id = (
  SELECT CAST(value AS INTEGER) FROM settings
  WHERE "key" = 'initial_admin_key:' || public_keys.public_key
    AND value ~ '^[1-9][0-9]{0,8}$'
);
DELETE FROM settings WHERE substr("key", 1, 18) = 'initial_admin_key:';
