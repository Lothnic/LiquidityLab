-- Replay workers own only analytical jobs. Their lease/generation is separate
-- from financial account ownership, and no job transition changes account state.
CREATE TABLE replay_admission (
  singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
  per_account integer NOT NULL CHECK(per_account BETWEEN 1 AND 64),
  global_limit integer NOT NULL CHECK(global_limit BETWEEN per_account AND 1024)
);

CREATE TABLE replay_jobs (
  sequence bigserial UNIQUE NOT NULL,
  tenant_id text NOT NULL,
  account_id text NOT NULL,
  job_id text NOT NULL CHECK(length(job_id) BETWEEN 1 AND 128),
  input_json bytea NOT NULL CHECK(octet_length(input_json) BETWEEN 1 AND 65536),
  input_digest bytea NOT NULL CHECK(octet_length(input_digest)=32),
  status text NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled')),
  attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
  worker_id text NOT NULL DEFAULT '' CHECK(length(worker_id)<=128),
  lease_until timestamptz,
  result_json bytea CHECK(octet_length(result_json) BETWEEN 1 AND 16777216),
  result_digest bytea CHECK(octet_length(result_digest)=32),
  error_code text NOT NULL DEFAULT '' CHECK(error_code IN ('','input_invalid','archive_unavailable','execution_failed','deadline_exceeded','worker_lost')),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(tenant_id,account_id,job_id),
  FOREIGN KEY(tenant_id,account_id) REFERENCES accounts,
  CHECK((status='running') = (lease_until IS NOT NULL)),
  CHECK(status<>'running' OR (worker_id<>'' AND attempts>0)),
  CHECK((result_json IS NULL) = (result_digest IS NULL)),
  CHECK((status='succeeded') = (result_json IS NOT NULL AND result_digest IS NOT NULL)),
  CHECK((status='failed') = (error_code<>''))
);

CREATE INDEX replay_jobs_pending ON replay_jobs(status,sequence)
  WHERE status IN ('queued','running');
CREATE INDEX replay_jobs_account_pending ON replay_jobs(tenant_id,account_id,status)
  WHERE status IN ('queued','running');
CREATE INDEX replay_jobs_account_history ON replay_jobs(tenant_id,account_id,sequence DESC);
