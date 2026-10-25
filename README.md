# trader

An AI crypto trading agent for Binance spot. It watches BTC and ETH around the clock, reads live prices and crypto news, and asks an advanced language model to rate each coin every 4 hours, or sooner on a sharp move or major news. Fixed rules turn the ratings into market orders. Every decision, trade and problem is reported to Telegram, and a buy-and-hold twin shows whether the AI beats doing nothing.

## Setup

1. **Binance:** create an API key with spot trading on and withdrawals off, ideally restricted to your IP. For a trial run, use testnet keys from testnet.binance.vision.
2. **Telegram:** create a bot with @BotFather and send it a message; `trader check` then prints your chat ID. Commands are only accepted from that private chat.
3. **Env:** `cp .env.example .env` and fill it in. `TRADER_MODE=testnet` uses test funds; `live` trades real money.

```sh
mkdir -p data                           # on Linux: sudo chown 65532 data
docker compose run --rm trader check    # keys, permissions, balance, news, Telegram
docker compose run --rm trader init     # start with capital_usd (100 USDT)
docker compose up -d --build            # trade around the clock
docker compose run --rm trader status
```

`trader decide --dry-run` shows the live briefing without calling the model or trading.

## Rules

- **Targets:** `strong_buy` is 45% of equity, `buy` 22.5%, `hold` keeps the position, and `sell` exits. At most 90% is invested, and changes under 5 points or $10 are skipped.
- **Stops:** a 10% loss in a day pauses trading until 00:00 UTC. A 30% loss from the start sells everything and stops; after `/resume` both limits count from that moment. While you have stopped trading yourself, a big loss is reported but nothing is sold.
- **Triggers:** a decision every 4 hours, plus up to 8 a day on 3% hourly moves or headlines matching alert keywords.
- **Telegram:** `/status`, `/decide`, `/stop`, `/resume`, `/sellall`.
- **Safety:**
  - Each order is saved before it is sent. After an unclear reply it is looked up, never resent.
  - Keys that can withdraw are refused.
  - The agent trades only its own capital.
- **Settings:** everything is in `config/agent.json`; restart the container after editing it.
- **Testnet:** decisions use live Binance prices and news. Orders fill on the testnet's own book with test funds; the account is valued at those prices and pays Binance's 0.1% fee on paper, so its profit and loss reflects real costs.
- **AI spend:** `trader budget status` shows it. A model call cut off by a crash is charged its maximum after an hour.

`make check` runs the tests.
