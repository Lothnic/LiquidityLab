-- The local paper venue has no effect outside its durable observations. Activate
-- this contract only through the paper ownership path; external financial facts
-- remain independent of the execution owner's lease.
ALTER TABLE accounts ADD COLUMN paper_transport_contract text NOT NULL DEFAULT '';

CREATE FUNCTION ll_guard_paper_observation() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE
  contract text;
  active_owner text;
  active_epoch bigint;
  active_lease timestamptz;
BEGIN
  IF NEW.kind NOT IN ('transport', 'paper_trade') THEN
    RETURN NEW;
  END IF;
  -- A legacy binary can insert without the new Go API. Serialize its INSERT
  -- against takeover and use the trigger table's schema, never caller search_path.
  EXECUTE pg_catalog.format(
    'SELECT paper_transport_contract,owner_id,ownership_epoch,lease_until FROM %I.accounts WHERE tenant_id=$1 AND account_id=$2 FOR UPDATE',
    TG_TABLE_SCHEMA)
    INTO contract,active_owner,active_epoch,active_lease
    USING NEW.tenant_id,NEW.account_id;
  IF contract IS NULL OR contract = '' THEN
    RETURN NEW;
  END IF;
  IF contract <> 'postgres-paper-v1'
     OR pg_catalog.current_setting('liquiditylab.paper_owner_id',true) IS DISTINCT FROM active_owner
     OR pg_catalog.current_setting('liquiditylab.paper_owner_epoch',true) IS DISTINCT FROM active_epoch::text
     OR active_owner = '' OR active_epoch <= 0
     OR active_lease IS NULL OR active_lease <= pg_catalog.clock_timestamp() THEN
    RAISE EXCEPTION USING ERRCODE = 'P0001',
      MESSAGE = 'paper observation ownership is not current',
      CONSTRAINT = 'll_paper_observation_owner';
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER ll_paper_observation_owner
  BEFORE INSERT ON observations FOR EACH ROW
  EXECUTE FUNCTION ll_guard_paper_observation();
