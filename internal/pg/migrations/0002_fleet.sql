-- Tenants with their tier's limits, agents with versioned configs, and
-- users' keys sealed by the engine's vault.

alter table tenants add column entitlements jsonb not null default '{}';
alter table tenants add column status text not null default 'active' check (status in ('active', 'suspended'));

alter table agents add column status text not null default 'stopped' check (status in ('active', 'stopped'));
alter table agents add column version int not null default 0 check (version >= 0);
create unique index agents_tenant_name on agents (tenant_id, name);

-- Every config an agent has had. A change is a new version; rows never change.
create table agent_versions (
  agent_id uuid not null references agents (id),
  tenant_id uuid not null references tenants (id),
  version int not null check (version > 0),
  config jsonb not null,
  created_by text not null,
  created_at timestamptz not null default now(),
  primary key (agent_id, version)
);

create table credentials (
  id uuid primary key,
  tenant_id uuid not null references tenants (id),
  kind text not null check (kind in ('anthropic', 'binance', 'telegram')),
  label text not null,
  hint text not null,
  ciphertext bytea not null,
  nonce bytea not null,
  wrapped_key bytea not null,
  key_nonce bytea not null,
  master_version int not null,
  created_at timestamptz not null default now(),
  revoked_at timestamptz
);
create index credentials_tenant on credentials (tenant_id, kind);

-- Which keys each agent uses. An exchange key or a Telegram bot serves
-- one agent only; one Anthropic key may serve all of a tenant's agents.
create table agent_credentials (
  agent_id uuid not null references agents (id),
  tenant_id uuid not null references tenants (id),
  kind text not null check (kind in ('anthropic', 'binance', 'telegram')),
  credential_id uuid not null references credentials (id),
  primary key (agent_id, kind)
);
create unique index agent_credentials_exclusive on agent_credentials (credential_id) where kind <> 'anthropic';

alter table agent_versions enable row level security;
alter table agent_versions force row level security;
create policy tenant_isolation on agent_versions
  using (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

alter table credentials enable row level security;
alter table credentials force row level security;
create policy tenant_isolation on credentials
  using (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

alter table agent_credentials enable row level security;
alter table agent_credentials force row level security;
create policy tenant_isolation on agent_credentials
  using (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The scheduler's only view across tenants: which agents should be running,
-- at which config version and with which keys. An agent runs only with an
-- Anthropic key and a Binance key.
create function runnable_agents() returns table (agent_id uuid, tenant_id uuid, version int, keys text)
language sql stable security definer set search_path = public as $$
  select a.id, a.tenant_id, a.version, string_agg(c.id::text, ',' order by c.kind)
  from agents a
  join tenants t on t.id = a.tenant_id
  join agent_credentials ac on ac.agent_id = a.id
  join credentials c on c.id = ac.credential_id and c.revoked_at is null
  where a.status = 'active' and t.status = 'active' and a.version > 0
  group by a.id, a.tenant_id, a.version
  having count(*) filter (where c.kind in ('anthropic', 'binance')) = 2
$$;

revoke all on function runnable_agents() from public;
grant execute on function runnable_agents() to trader_app;
grant update (entitlements, status) on tenants to trader_app;
grant update (status, version) on agents to trader_app;
grant select, insert on agent_versions to trader_app;
grant select, insert on credentials to trader_app;
grant update (revoked_at) on credentials to trader_app;
grant select, insert, update, delete on agent_credentials to trader_app;
