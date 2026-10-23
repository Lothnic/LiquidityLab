-- Durable facts are ingress data, independent of execution ownership. Unprocessed
-- facts block new dispatch until an owner incorporates them transactionally.
ALTER TABLE accounts ADD COLUMN observed_sequence bigint NOT NULL DEFAULT 0 CHECK(observed_sequence >= 0);
ALTER TABLE accounts ADD COLUMN processed_sequence bigint NOT NULL DEFAULT 0 CHECK(processed_sequence >= 0);
ALTER TABLE accounts ADD COLUMN pending_observation_bytes bigint NOT NULL DEFAULT 0 CHECK(pending_observation_bytes BETWEEN 0 AND 16777216);
ALTER TABLE accounts ADD CONSTRAINT observation_cursor_order CHECK(processed_sequence <= observed_sequence);
CREATE TABLE observations (
  tenant_id text NOT NULL, account_id text NOT NULL, sequence bigint NOT NULL CHECK(sequence > 0),
  observation_id text NOT NULL, kind text NOT NULL, payload jsonb NOT NULL,
  payload_bytes integer NOT NULL CHECK(payload_bytes BETWEEN 1 AND 1048576),
  digest bytea NOT NULL, received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(tenant_id,account_id,sequence),
  UNIQUE(tenant_id,account_id,observation_id),
  FOREIGN KEY(tenant_id,account_id) REFERENCES accounts
);
