-- The trading agent is gone, and with it the model and Binance keys: they
-- are deleted, every agent is stopped, and only wallets can be stored. The
-- agents' configs and audit logs stay as they were. The owner is under the
-- forced row-level security too, so it is lifted while the rows change.

alter table agent_credentials no force row level security;
alter table credentials no force row level security;
alter table agents no force row level security;
delete from agent_credentials;
delete from credentials where kind <> 'hyperliquid';
update agents set status = 'stopped' where status = 'active';
alter table agents force row level security;
alter table credentials force row level security;
alter table agent_credentials force row level security;

alter table credentials drop constraint credentials_kind_check;
alter table credentials add constraint credentials_kind_check check (kind = 'hyperliquid');
