-- Closing an account keeps its id and trading history but nothing that names
-- the person or opens their keys: the tenant's name and every key's hint and
-- sealed secret are erased, and a closed tenant never runs again.

alter table tenants drop constraint tenants_status_check;
alter table tenants add constraint tenants_status_check check (status in ('active', 'suspended', 'closed'));
grant update (name) on tenants to trader_app;
grant update (hint, ciphertext, nonce, wrapped_key, key_nonce) on credentials to trader_app;
