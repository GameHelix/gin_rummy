-- The engine's event store. Every table carries a tenant, and row-level
-- security limits each transaction to the tenant in app.tenant_id. The
-- engine runs as trader_app, which can add events but never change them.

do $$
begin
  if not exists (select from pg_roles where rolname = 'trader_app') then
    create role trader_app nologin nobypassrls;
  end if;
end $$;

create table tenants (
  id uuid primary key,
  name text not null,
  created_at timestamptz not null default now()
);

create table agents (
  id uuid primary key,
  tenant_id uuid not null references tenants (id),
  name text not null,
  mode text not null check (mode in ('testnet', 'live')),
  created_at timestamptz not null default now()
);
create index agents_tenant on agents (tenant_id);

-- The head of each agent's hash chain: the last event stored.
create table streams (
  agent_id uuid primary key references agents (id),
  tenant_id uuid not null references tenants (id),
  seq bigint not null check (seq > 0),
  hash bytea not null,
  updated_at timestamptz not null default now()
);

-- body keeps the exact JSON text that was hashed; payload is its queryable copy.
create table events (
  id uuid not null,
  tenant_id uuid not null,
  agent_id uuid not null,
  seq bigint not null check (seq > 0),
  type text not null,
  at timestamptz not null,
  body json not null,
  payload jsonb generated always as (body::jsonb) stored,
  prev_hash bytea,
  hash bytea not null,
  primary key (at, id)
) partition by range (at);
create index events_agent_seq on events (agent_id, seq);
create index events_tenant_type on events (tenant_id, type, at);

create function forbid_change() returns trigger language plpgsql as $$
begin
  raise exception 'audit events are append-only';
end $$;
create trigger events_append_only before update or delete on events
  for each row execute function forbid_change();
create trigger events_no_truncate before truncate on events
  for each statement execute function forbid_change();

alter table tenants enable row level security;
alter table tenants force row level security;
create policy tenant_isolation on tenants
  using (id = nullif(current_setting('app.tenant_id', true), '')::uuid);

alter table agents enable row level security;
alter table agents force row level security;
create policy tenant_isolation on agents
  using (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

alter table streams enable row level security;
alter table streams force row level security;
create policy tenant_isolation on streams
  using (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

alter table events enable row level security;
alter table events force row level security;
create policy tenant_isolation on events
  using (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Monthly partitions are created on demand, as their owner, with the same
-- isolation as the parent table.
create function ensure_event_partition(month date) returns void
language plpgsql security definer set search_path = public as $$
declare
  first date := date_trunc('month', month)::date;
  name text := format('events_%s', to_char(first, 'YYYY_MM'));
begin
  perform pg_advisory_xact_lock(hashtext(name));
  if to_regclass(name) is not null then
    return;
  end if;
  execute format('create table %I partition of events for values from (%L) to (%L)', name, first, (first + interval '1 month')::date);
  execute format('alter table %I enable row level security', name);
  execute format('alter table %I force row level security', name);
  execute format('create policy tenant_isolation on %I using (tenant_id = nullif(current_setting(%L, true), %L)::uuid)', name, 'app.tenant_id', '');
end $$;

revoke all on function ensure_event_partition(date) from public;
grant usage on schema public to trader_app;
grant select, insert on tenants, agents, events to trader_app;
grant select, insert, update on streams to trader_app;
grant execute on function ensure_event_partition(date) to trader_app;
