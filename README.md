# trader

A real-time paper-trading agent for US stocks, written in Go.

After every NYSE close, an advanced language model researches a small universe of stocks. It works with live tools: recent price history and the last week's headlines. It then rates each stock from `strong_sell` to `strong_buy`, citing its evidence. Deterministic code turns those ratings into orders, which fill at the next session's open in a local paper account.

A buy-and-hold account holding the same stocks, with the same costs, runs alongside. `trader status` tells you whether the agent is beating it.

No real orders are ever sent. Trading real money would be a separate, deliberate step (see the [roadmap](docs/04-roadmap.md)).

## Quickstart

Requires Go 1.24+ on macOS or Linux.

```sh
go build -o bin/trader ./cmd/trader
cp .env.example .env              # then set ANTHROPIC_API_KEY and TRADER_MODEL
./bin/trader init                 # paper account with $10,000 in data/agent/
./bin/trader run --dry-run        # check live data: no model call, nothing saved
./bin/trader run                  # one real cycle (paid model calls, capped)
./bin/trader status               # agent vs buy-and-hold, positions, orders, spend
./bin/trader daemon               # keep running: one cycle after every close
```

`run` is safe to repeat. Once the latest session has been processed it does nothing until the next close.

Or run it as a service with Docker. The image is about 23 MB and reads your `.env` at start-up:

```sh
docker compose up -d --build              # daemon in the background, restarts with Docker
docker compose logs -f                    # follow cycles as they run
docker compose run --rm trader status     # account view
```

## What one cycle does

1. **Wait for the close.** The cycle runs 30 minutes after the 16:00 New York close (13:00 on early-close days), using the NYSE calendar for 2026–2027.
2. **Fetch live data.** It downloads daily bars for the universe and SPY from Yahoo Finance and archives the raw responses.
3. **Update the paper account.**
   - Splits and dividends are applied at the open.
   - The previous decision's orders fill at the open, with 5 bps adverse slippage and a $1 commission.
   - Positions are marked at the close.
   - Ratings whose horizon has ended are scored against SPY.
4. **Research.** The model receives price indicators for every stock and for SPY. It can call `get_price_history` and `get_news` as often as it needs, within 8 turns. It finishes with `submit_signals`: one rating per stock, with a rationale and cited evidence IDs. Unknown evidence IDs are rejected and sent back for correction.
5. **Size the orders in code.** The model never sees the portfolio or picks quantities.

   | Rating | Target |
   | --- | --- |
   | `strong_buy` | 20% of equity |
   | `buy` | 10% |
   | `hold` | keep the current position |
   | `sell`, `strong_sell` | exit |

   - Total exposure is capped at 80%; new purchases are cut first.
   - Weight changes under 3 percentage points, and trades under $100, are skipped. Exits always go through.
6. **Save.** The account is written atomically, along with a full record of the cycle. The orders wait for the next open.

If a cycle cannot run before the next open, positions are held and the decision is recorded as missed. Orders are never placed after the fact.

## Commands

| Command | Purpose |
| --- | --- |
| `trader init` | Create the paper account (refuses to overwrite one) |
| `trader run [--dry-run]` | Run one cycle now. `--dry-run` fetches live data and prints the briefing without calling the model or saving |
| `trader daemon` | Run a cycle after every close. Transient failures retry every 10 minutes; failures that retrying cannot fix are logged and retried at the next close |
| `trader status [--json]` | Agent vs benchmark, positions, pending orders, the last decision, the rating scorecard and LLM spend |
| `trader budget status` | Spend today and this month, plus any unresolved charges |
| `trader budget resolve --attempt ID --cost-usd N --note TEXT` | Record the provider-verified cost of an uncertain request |
| `trader models` | List the models your API key can use |

All commands read `config/agent.json` unless you pass `--config`.

## Configuration

| Field | Default | Meaning |
| --- | --- | --- |
| `symbols` | AAPL, MSFT, JPM, XOM, KO | Universe (1–20 US stocks or ETFs) |
| `market_symbol` | SPY | Market context and scorecard benchmark |
| `initial_cash`, `commission`, `slippage_bps` | 10000, 1, 5 | Paper account and costs |
| `max_position_weight`, `max_invested_weight` | 0.2, 0.8 | Size of `strong_buy` and the exposure cap |
| `rebalance_band`, `min_trade_usd` | 0.03, 100 | No-trade band and minimum trade |
| `horizon_sessions` | 5 | Forecast horizon the ratings are scored on |
| `effort` | `high` | Reasoning effort. The model itself is `TRADER_MODEL` in `.env`; list the choices with `trader models` |
| `max_output_tokens`, `max_turns`, `samples` | 16000, 8, 1 | Per-call output cap, research turns, independent runs (median vote) |
| `news_items` | 8 | Headlines per `get_news` call (0 disables the tool) |
| `refusal_fallback` | true | Let the API retry a refused request on Anthropic's recommended fallback model |
| `input_usd_per_million`, `output_usd_per_million` | 4, 20 | Token prices used for cost estimates; set them to your model's list prices |
| `max_daily_llm_usd`, `max_monthly_llm_usd` | 3, 40 | Spending caps (UTC day and month) |
| `data_dir` | `data/agent` | Account state and cycle records |

Changing the name or the symbols needs a new `data_dir`. Other fields can change between cycles.

## Safety

- **Paper only.** The model's tools are read-only, it cannot change limits, and the code contains no broker connection.
- **Spending caps hold across crashes.** Before every model call, an upper-bound cost is reserved in a durable journal shared by all processes using the same `data_dir`.
  - An HTTP error response releases the reservation.
  - A network failure keeps it charged as *uncertain* until you resolve it.
  - Caps are estimates from configured prices. Your Anthropic bill is authoritative.
- **API failures are classified.** 408, 409, 429 and 5xx are retried with backoff. Other HTTP errors (bad request, bad key, unknown model) stop the cycle without retrying.
- **Retried cycles reproduce the same result.** A retried cycle reuses the archived market data and any finished analyst runs, so it gets the same fills and does not pay twice.
- **News is untrusted.** Headlines are labelled as third-party data in the prompt, and the analyst is told to ignore any instructions inside them.

## Limitations

- Prices and headlines come from Yahoo Finance's unofficial endpoints, which can change or rate-limit.
- Fills are modelled at the official open plus fixed slippage. There are no partial fills, queue position or market impact.
- The data source has no dividend payment dates. Dividends count in equity from the ex-date but cannot fund purchases.
- A five-stock universe and a few weeks of results tell you little. Watch the scorecard grow before drawing conclusions.

## Development

```sh
make check   # go vet + race-enabled tests (fakes only: no network, no paid calls)
```

## Docs

1. [How trading works](docs/01-trading-basics.md): orders, fills, risk and costs for beginners.
2. [Architecture](docs/02-architecture.md): components, the cycle, files, failure handling and design choices.
3. [Operations](docs/03-operations.md): running the daemon, reading state, budget recovery.
4. [Roadmap](docs/04-roadmap.md): broker integration, more tools, alerts.
