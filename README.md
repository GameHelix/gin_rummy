# trader

A real-time paper-trading agent for US stocks. After every NYSE close, an advanced language model reads live prices and news and rates each stock. Fixed rules turn the ratings into orders that fill at the next open in a local paper account, next to a buy-and-hold benchmark. No real orders are sent.

## Run

```sh
cp .env.example .env                      # add your ANTHROPIC_API_KEY
docker compose run --rm trader init       # once
docker compose up -d --build              # one cycle after every close
docker compose run --rm trader status     # agent vs buy-and-hold
```

Without Docker, build with `go build -o bin/trader ./cmd/trader` and run `bin/trader help` for the commands.

## Rules

- **Ratings to targets:** `strong_buy` targets 20%, `buy` 10%, `hold` keeps the position, and `sell` exits.
- **Limits:** exposure is capped at 80%, and changes under 3 points or $100 are skipped.
- **Spending:** model calls are capped at $3 a day and $40 a month.
- **Settings:** everything is in `config/agent.json`.

`make check` runs the tests.
