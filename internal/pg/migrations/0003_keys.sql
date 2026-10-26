-- Telegram moved to the notifier: the engine keeps model and exchange keys
-- only. The owner is under the forced row-level security too, so it is
-- lifted while the legacy rows go.

alter table agent_credentials no force row level security;
alter table credentials no force row level security;
delete from agent_credentials where kind = 'telegram';
delete from credentials where kind = 'telegram';
alter table credentials force row level security;
alter table agent_credentials force row level security;

alter table credentials drop constraint credentials_kind_check;
alter table credentials add constraint credentials_kind_check check (kind in ('anthropic', 'binance'));
alter table agent_credentials drop constraint agent_credentials_kind_check;
alter table agent_credentials add constraint agent_credentials_kind_check check (kind in ('anthropic', 'binance'));
