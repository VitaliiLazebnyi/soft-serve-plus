-- Drop the tables in reverse order of their foreign keys: every table goes
-- before the tables it references.
DROP TABLE IF EXISTS access_tokens;
DROP TABLE IF EXISTS lfs_locks;
DROP TABLE IF EXISTS lfs_objects;
DROP TABLE IF EXISTS collabs;
DROP TABLE IF EXISTS repos;
DROP TABLE IF EXISTS public_keys;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS settings;
