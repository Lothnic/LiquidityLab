-- Bounded session history reads enumerate creation events and find the latest
-- reducer snapshot for each parent without scanning unrelated account journals.
CREATE INDEX account_journal_session_creations
  ON account_journal(tenant_id, account_id, revision DESC)
  WHERE kind = 'create';

CREATE INDEX account_journal_session_parent_revision
  ON account_journal(tenant_id, account_id, ((detail->'snapshot'->'parent'->>'id')), revision DESC);

-- The first final snapshot freezes the residual valuation reference even when
-- source-only receipts continue to append snapshots after the parent ends.
CREATE INDEX account_journal_session_terminal
 ON account_journal(tenant_id,account_id,((detail->'snapshot'->'parent'->>'id')),revision ASC)
 WHERE detail->'snapshot'->'parent'->>'status' IN ('completed','cancelled','expired');
