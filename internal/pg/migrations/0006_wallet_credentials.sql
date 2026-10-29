alter table credentials drop constraint credentials_kind_check;
alter table credentials add constraint credentials_kind_check check (kind in ('anthropic', 'binance', 'hyperliquid'));
