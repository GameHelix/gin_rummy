-- The master key can be rotated: the engine re-wraps each key's data key
-- under the current master key and records the version it used. The
-- function is its only view across tenants of which keys still need it.

grant update (master_version) on credentials to trader_app;

create function credentials_to_rewrap(current int) returns table (tenant_id uuid, id uuid)
language sql stable security definer set search_path = public as $$
  select tenant_id, id from credentials
  where master_version <> current and revoked_at is null
  order by tenant_id, created_at
$$;

revoke all on function credentials_to_rewrap(int) from public;
grant execute on function credentials_to_rewrap(int) to trader_app;
