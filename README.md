# trader

An AI crypto trading agent for Binance spot. It watches BTC and ETH around the clock, reads live prices and crypto news, and asks an advanced language model to rate each coin every 4 hours, or sooner on a sharp move or major news. Fixed rules turn the ratings into market orders. Every decision, trade and problem is reported to Telegram, and a buy-and-hold twin shows whether the AI beats doing nothing.

## Setup

1. **Binance:** create an API key with spot trading on and withdrawals off, ideally restricted to your IP. For a trial run, use testnet keys from testnet.binance.vision.
2. **Telegram:** create a bot with @BotFather and send it a message; `trader check` then prints your chat ID. Commands are only accepted from that private chat.
3. **Env:** `cp .env.example .env` and fill it in. `TRADER_MODE=testnet` uses test funds; `live` trades real money. `POSTGRES_PASSWORD` protects the engine's database. `ENGINE_MASTER_KEY` (`openssl rand -base64 32`) encrypts the keys stored there; back it up, because without it they cannot be read. `ENGINE_API_TOKEN` (`openssl rand -hex 32`) opens the engine API.

```sh
mkdir -p data                           # on Linux: sudo chown 65532 data
docker compose run --rm trader check    # keys, permissions, balance, news, Telegram
docker compose run --rm trader init     # start with capital_usd (100 USDT)
docker compose run --rm trader import   # hand the agent and its keys to the engine
docker compose up -d --build            # the engine trades around the clock
```

`trader decide --dry-run` shows the live briefing without calling the model or trading.

## Engine

`trader serve` runs any number of agents side by side. Each has its own keys, sealed in the database, its own coins and a versioned config; a new config version restarts the agent. A tenant's plan (allowed coins, number of agents, how often it may decide, live trading) is enforced on every change.

Run these as `docker compose run --rm -T trader ...`; files go in `config/`.

```sh
trader tenant add --name NAME --plan config/plan.json   # tenant plan changes it
trader key add --tenant ID --kind anthropic < key.json  # {"key":"..."}; binance and telegram too
trader key list|revoke --tenant ID [--key ID]
trader agent add --tenant ID --name NAME --keys ID,ID --config config/agent.json
trader agent start|stop|status --tenant ID --agent ID
trader agent list --tenant ID
```

## Server

Any server prepared by [wickwise/infra](https://github.com/wickwise/infra), at any cloud or your own, runs the engine:

```sh
./deploy.sh deploy@SERVER_IP   # builds for the server's CPU; ships the image, compose.yaml, config/ and .env over SSH
```

Data and the database stay on the server. Allow the server's IP on the Binance key.

## Engine API

The app's only way in, on `127.0.0.1:8080` with `Authorization: Bearer $ENGINE_API_TOKEN`. It does everything Telegram can (status, decide, stop, resume, sell all) and manages tenants, plans, keys, agents and configs; each agent's audit log comes as history or a live stream. The contract is [api/openapi.yaml](api/openapi.yaml); package `api` is its Go client.

```sh
curl -H "Authorization: Bearer $ENGINE_API_TOKEN" 127.0.0.1:8080/v1/tenants/ID/agents/ID/status
```

## Rules

- **Targets:** `strong_buy` is 45% of equity, `buy` 22.5%, `hold` keeps the position, and `sell` exits. At most 90% is invested, and changes under 5 points or $10 are skipped.
- **Stops:** a 10% loss in a day pauses trading until 00:00 UTC. A 30% loss from the start sells everything and stops; after `/resume` both limits count from that moment. While you have stopped trading yourself, a big loss is reported but nothing is sold.
- **Triggers:** a decision every 4 hours, plus up to 8 a day on 3% hourly moves or headlines matching alert keywords.
- **Telegram:** `/status`, `/decide`, `/stop`, `/resume`, `/sellall`.
- **Audit:** every step (the data shown to the model, each model call and its cost, ratings, orders, fills, stops, commands) is written to a hash-chained log in the agent's `events` folder before it happens, then copied to Postgres.
- **Safety:**
  - Each order is saved before it is sent. After an unclear reply it is looked up, never resent.
  - Keys that can withdraw are refused.
  - The agent trades only its own capital.
- **Settings:** everything is in `config/agent.json`. After editing it, `trader agent config` stores it as a new version and the engine restarts the agent.
- **Testnet:** decisions use live Binance prices and news. Orders fill on the testnet's own book with test funds; the account is valued at those prices and pays Binance's 0.1% fee on paper, so its profit and loss reflects real costs.
- **AI spend:** `trader budget status` shows it. A model call cut off by a crash is charged its maximum after an hour.

`make check` runs the tests.
