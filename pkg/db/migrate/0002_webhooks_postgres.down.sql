-- Drop the tables in reverse order of their foreign keys: every table goes
-- before the tables it references.
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS webhooks;
