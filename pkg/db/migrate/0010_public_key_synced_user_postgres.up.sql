-- The record's value is the decimal id the sync wrote. A value that is not
-- one keeps no record. So does one of ten digits or more, which may still be
-- a valid id (up to 2147483647): its key is then never removed, the safe
-- side. Leading zeros are allowed, as SQLite reads them.
ALTER TABLE public_keys ADD COLUMN synced_user_id INTEGER;
UPDATE public_keys SET synced_user_id = (
  SELECT CAST(value AS INTEGER) FROM settings
  WHERE "key" = 'initial_admin_key:' || public_keys.public_key
    AND value ~ '^0*[1-9][0-9]{0,8}$'
);
DELETE FROM settings WHERE substr("key", 1, 18) = 'initial_admin_key:';
