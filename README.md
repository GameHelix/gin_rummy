# trader

The Candylario engine. Its radar watches every Hyperliquid perpetual and raises unusual moves, each with the record of what followed such moves since 2021 and the trade that record argues for; [candylario/notifier](https://github.com/candylario/notifier) posts them to Telegram. It also holds each account's agent wallet, sealed in its vault, and places one-tap trades with their stops already on the exchange.

`go run ./cmd/trader radar` prints the alerts live and needs no keys (`--json`, `--interval 30s`, `--testnet`, `--no-warmup`).

## Setup

`cp .env.example .env` and fill it in. `POSTGRES_PASSWORD` protects the engine's database. `ENGINE_MASTER_KEY` (`openssl rand -base64 32`) encrypts the wallet keys stored there; back it up, because without it they cannot be read. `ENGINE_MASTER_KEY_PREVIOUS` holds an older master key during a rotation. `ENGINE_API_TOKEN` (`openssl rand -hex 32`) opens the engine API for the app; `ENGINE_NOTIFIER_TOKEN` (another `openssl rand -hex 32`) is the notifier's and opens only what it needs. `HYPERLIQUID_BUILDER` and `HYPERLIQUID_BUILDER_FEE` (in tenths of a basis point) are the builder fee accounts approve; `TRADE_MAX_USD` and `TRADE_MAX_LEVERAGE` cap each trade ($100 and 5x by default).

```sh
docker network create candylario   # once per machine; Candylario services meet on it
docker compose up -d --build        # the radar and the engine API
```

## Server

Any server prepared by [candylario/infra](https://github.com/candylario/infra), at any cloud or your own, runs the engine:

```sh
./deploy.sh deploy@SERVER_IP              # builds the pushed commit for the server's CPU; ships the image, compose.yaml and the settings over SSH
./deploy.sh --rollback deploy@SERVER_IP   # runs the previous image again; no build
```

Only a clean, pushed tree deploys; the image is tagged with the commit, and `/v1/health` reports it. The last three images stay on the server for rollbacks. The settings come from the central `../.env` through `../infra/env.sh` when infra sits next to this repo, else from `.env`. The database stays on the server. `./move.sh deploy@SERVER_IP` moves an engine running here to the server, database included, and stops it here.

## Engine API

The app's and the notifier's only way in, on `127.0.0.1:8080` with `Authorization: Bearer $ENGINE_API_TOKEN`. The notifier uses `ENGINE_NOTIFIER_TOKEN` instead, which opens only the radar and a tenant's trades, so a leak there cannot close accounts or touch wallets. It manages tenants and each tenant's wallet (made by the engine, approved by the account's own wallet in the browser) and trades, and serves the radar's alerts as a list or a live stream. The contract is [api/openapi.yaml](api/openapi.yaml); package `api` is its Go client.

```sh
curl -H "Authorization: Bearer $ENGINE_API_TOKEN" '127.0.0.1:8080/v1/radar/alerts?limit=5'
```

## Rotating the master key

Every wallet key is sealed under a data key of its own, and only that data key is encrypted with the master key, so a rotation re-wraps data keys and never touches the secrets. Each row records the master key version it was sealed under; the engine opens rows with the key of their version and seals new rows with the current one.

1. Make a new key: `openssl rand -base64 32`. In the central `.env`, set `ENGINE_MASTER_KEY=2:NEW` (the version before the colon, one more than the old one; a key without a version is version 1) and `ENGINE_MASTER_KEY_PREVIOUS=OLD`.
2. `./deploy.sh deploy@SERVER`. New keys now seal under version 2; old rows still open.
3. On the server: `cd /srv/trader && docker compose run --rm trader vault rewrap`. It prints how many keys it moved; run it again to see 0.
4. Clear `ENGINE_MASTER_KEY_PREVIOUS` and deploy again. Keep the old key next to the backup key pair until every backup made under it has expired (30 days): a restored database from before the rewrap needs it in `ENGINE_MASTER_KEY_PREVIOUS` again.

`make check` runs the tests.
