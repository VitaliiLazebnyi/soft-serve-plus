-- The record's value is the decimal id the sync wrote. A value that is not
-- one keeps no record. Leading zeros are read as Postgres reads them; a value
-- too large for a user id saturates here and matches no user, so its key is
-- never removed, as with NULL.
ALTER TABLE public_keys ADD COLUMN synced_user_id INTEGER;
UPDATE public_keys SET synced_user_id = (
  SELECT CASE WHEN value <> '' AND value NOT GLOB '*[^0-9]*' THEN NULLIF(CAST(value AS INTEGER), 0) END
  FROM settings
  WHERE "key" = 'initial_admin_key:' || public_keys.public_key
);
DELETE FROM settings WHERE substr("key", 1, 18) = 'initial_admin_key:';
