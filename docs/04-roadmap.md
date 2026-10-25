# Roadmap

Ordered by how much each item improves a working agent.

1. **Alerts.** Send fills, missed decisions, budget stops and daemon errors to Slack or email, so a stopped daemon gets noticed the same day.
2. **SEC filings tool.** EDGAR's submissions API gives 8-K/10-Q/10-K filings with acceptance timestamps. It is official and free, but it requires a declared User-Agent with a contact email, which belongs in configuration and not in code.
3. **Earnings calendar tool.** Upcoming report dates change what `hold` means. Expose them to the analyst.
4. **Broker adapter (paper first).** Implement Alpaca's paper API behind the same order flow. Orders become market-on-open orders submitted after the decision; fills and the account come from the broker. The broker becomes the source of truth, and the local ledger reconciles against it every cycle.
5. **Calibration report.** Once enough ratings have matured, compare hit rates by confidence level. If confidence turns out to mean something, consider letting it scale position sizes.
6. **Multiple samples by default.** `samples: 3` is a median vote, at three times the cost. Turn it on if single-run ratings prove unstable from day to day.
7. **Real money.** This is a separate decision, not a config flag. Prerequisites:
   - months of paper results ahead of the benchmark after LLM costs;
   - a working broker adapter with reconciliation;
   - alerts;
   - a hard position-size cap;
   - a documented way to stop the agent instantly.
