-- Account rows serialize every authoritative transition. This schema does not
-- fence an exchange connection and contains no automatic live failover path.
CREATE FUNCTION ll_decimal_valid(v numeric) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT AS $$
  SELECT v NOT IN ('NaN'::numeric, 'Infinity'::numeric, '-Infinity'::numeric)
     AND scale(v) <= 144
     AND length(ltrim(replace(trim_scale(abs(v))::text, '.', ''), '0')) <= 512
$$;
CREATE DOMAIN ll_amount AS numeric CHECK (VALUE >= 0 AND ll_decimal_valid(VALUE));

CREATE TABLE accounts (
  tenant_id text NOT NULL, account_id text NOT NULL,
  revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
  owner_id text NOT NULL DEFAULT '', ownership_epoch bigint NOT NULL DEFAULT 0,
  lease_until timestamptz, ready boolean NOT NULL DEFAULT false,
  market_valid boolean NOT NULL DEFAULT false, market_snapshot_id text NOT NULL DEFAULT '',
  instrument_revision text NOT NULL DEFAULT '', snapshot jsonb NOT NULL DEFAULT '{}',
  PRIMARY KEY (tenant_id, account_id)
);
CREATE TABLE balances (
  tenant_id text NOT NULL, account_id text NOT NULL, currency text NOT NULL,
  total ll_amount NOT NULL, free ll_amount NOT NULL, locked ll_amount NOT NULL,
  CHECK (total = free + locked),
  PRIMARY KEY (tenant_id, account_id, currency),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts
);
CREATE TABLE plans (
  tenant_id text NOT NULL, account_id text NOT NULL, plan_id text NOT NULL,
  revision bigint NOT NULL CHECK (revision > 0),
  status text NOT NULL CHECK (status IN ('active','paused','cancel_requested','completed','cancelled','expired')),
  deadline timestamptz NOT NULL, detail jsonb NOT NULL,
  PRIMARY KEY (tenant_id, account_id, plan_id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts
);
CREATE UNIQUE INDEX one_parent_per_account ON plans(tenant_id,account_id)
WHERE status IN ('active','paused','cancel_requested');
CREATE TABLE plan_revisions (
  tenant_id text NOT NULL, account_id text NOT NULL, plan_id text NOT NULL,
  revision bigint NOT NULL, status text NOT NULL, deadline timestamptz NOT NULL, detail jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(tenant_id,account_id,plan_id,revision),
  FOREIGN KEY(tenant_id,account_id,plan_id) REFERENCES plans
);
CREATE TABLE account_journal (
  tenant_id text NOT NULL, account_id text NOT NULL, revision bigint NOT NULL,
  kind text NOT NULL, detail jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(tenant_id,account_id,revision),
  FOREIGN KEY(tenant_id,account_id) REFERENCES accounts
);
CREATE TABLE commands (
  tenant_id text NOT NULL, account_id text NOT NULL, command_id text NOT NULL,
  kind text NOT NULL, digest bytea NOT NULL, expected_revision bigint NOT NULL,
  result jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(tenant_id,account_id,command_id),
  FOREIGN KEY(tenant_id,account_id) REFERENCES accounts
);
CREATE TABLE outbox (
  tenant_id text NOT NULL, account_id text NOT NULL, intent_id text NOT NULL,
  plan_id text NOT NULL, plan_revision bigint NOT NULL,
  instrument_revision text NOT NULL, expires_at timestamptz NOT NULL, payload jsonb NOT NULL,
  state text NOT NULL CHECK(state IN ('queued','attempted','unknown','acknowledged','final','obsolete')),
  attempt_id text NOT NULL DEFAULT '', venue_order_id text NOT NULL DEFAULT '',
  authorization_revision bigint NOT NULL DEFAULT 0,
  authorization_snapshot text NOT NULL DEFAULT '',
  attempted_at timestamptz, resolution_evidence jsonb,
  PRIMARY KEY(tenant_id,account_id,intent_id),
  FOREIGN KEY(tenant_id,account_id,plan_id) REFERENCES plans
);
CREATE INDEX pending_outbox ON outbox(tenant_id,account_id,state)
WHERE state IN ('queued','attempted','unknown','acknowledged');
CREATE UNIQUE INDEX unique_dispatch_attempt ON outbox(tenant_id,account_id,attempt_id)
WHERE attempt_id <> '';
CREATE TABLE reservations (
  tenant_id text NOT NULL, account_id text NOT NULL, intent_id text NOT NULL, currency text NOT NULL,
  amount ll_amount NOT NULL CHECK(amount > 0), reflected boolean NOT NULL DEFAULT false,
  state text NOT NULL CHECK(state IN ('held','released')),
  PRIMARY KEY(tenant_id,account_id,intent_id,currency),
  FOREIGN KEY(tenant_id,account_id,intent_id) REFERENCES outbox,
  FOREIGN KEY(tenant_id,account_id,currency) REFERENCES balances
);
CREATE TABLE executions (
  tenant_id text NOT NULL, account_id text NOT NULL, venue text NOT NULL, execution_id text NOT NULL,
  intent_id text NOT NULL, digest bytea NOT NULL, detail jsonb NOT NULL, result jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(tenant_id,account_id,venue,execution_id),
  FOREIGN KEY(tenant_id,account_id,intent_id) REFERENCES outbox
);
