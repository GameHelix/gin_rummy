# Operations

## Start

```sh
go build -o bin/trader ./cmd/trader
./bin/trader models               # confirm your key can use the configured model
./bin/trader init                 # once per data_dir
./bin/trader run --dry-run        # live data check, free
./bin/trader run                  # first real decision
```

The first cycle records the latest closed session and places two sets of orders for the next open: the agent's, and the benchmark's equal-weight basket. Run `trader status` after the next session's close to see the first fills.

## Keep it running

`trader daemon` sleeps until each close plus the buffer, runs a cycle and goes back to sleep. It checks the clock every minute, so if the computer was asleep it catches up within a minute of waking. A decision is only missed when the machine sleeps from the close until just before the next open.

- Transient problems (bars not yet published, provider overload, network errors) are retried every 10 minutes until the next open.
- Problems that retrying cannot fix, such as a rejected request or an exhausted credit balance, are logged. The daemon tries again at the next close, so it recovers once you fix the cause.
- Startup checks fail fast and exit: a missing API key or a model your key cannot use. A date beyond the reviewed calendar also stops it.

It must run on a machine that is awake after the close. On macOS a LaunchAgent is the simplest option. Save this as `~/Library/LaunchAgents/com.trader.agent.plist`, adjusting the paths:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.trader.agent</string>
  <key>WorkingDirectory</key><string>/path/to/trader</string>
  <key>ProgramArguments</key><array><string>/path/to/trader/bin/trader</string><string>daemon</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>600</integer>
  <key>StandardOutPath</key><string>/path/to/trader/data/agent/daemon.log</string>
  <key>StandardErrorPath</key><string>/path/to/trader/data/agent/daemon.log</string>
</dict></plist>
```

Then run `launchctl load ~/Library/LaunchAgents/com.trader.agent.plist`. launchd restarts the daemon after any error exit, at most every 10 minutes because of `ThrottleInterval`. A startup problem such as a bad API key keeps failing until you fix it, so check the log whenever `trader status` shows no new session.

Instead of the daemon you can schedule `trader run` from cron at 16:45 New York time on weekdays. Cron uses the machine's local time zone, so convert the time. Extra runs are harmless no-ops.

## Run with Docker

The repository includes a `Dockerfile` and `compose.yaml`. The image is about 23 MB: a static binary on distroless, running as a non-root user. It reads the API key from `.env` when the container starts; the key is never copied into the image. It mounts `./data`, so the container and the local `bin/trader` share the same paper account.

```sh
docker compose up -d --build              # start the daemon (restarts with Docker)
docker compose logs -f                    # follow cycles as they run
docker compose run --rm trader status     # account view from a one-off container
docker compose run --rm trader run --dry-run
docker compose down                       # stop
```

Use one runner at a time, either the container or a local daemon. The account lock may not hold across the Docker Desktop virtual machine boundary. On a Linux host, make `./data` writable by the container user first: `sudo chown -R 65532:65532 data`.

## Watch it

- `trader status`: equity and return for the agent and for buy-and-hold, the gap between them, drawdown, exposure, fees, positions with their last rating, orders waiting for the open, the last decision with its rationale, the rating scorecard, missed decisions and LLM spend.
- `trader status --json`: the same data for scripts and dashboards.
- `data/agent/cycles/<date>/cycle.json`: exactly what one cycle did.
- `data/agent/cycles/<date>/sample-1.json`: which tools the analyst used, which headlines it saw, and what it concluded.

## Money and the budget

Each research turn is one paid model call. Before each call the agent reserves an upper bound: about half a token per prompt byte at the input price, plus the full output allowance at the output price. When the response arrives it settles the actual estimated cost. Caps are per UTC day and month (`max_daily_llm_usd`, `max_monthly_llm_usd`). If the next call would break a cap, the cycle records a missed decision and holds its positions.

`trader budget status` lists anything unresolved:

- **`uncertain`.** The network failed after the request was sent, so it may have been billed. Check usage in the Anthropic Console, then record it:

  ```sh
  ./bin/trader budget resolve --attempt ATTEMPT_ID --cost-usd 0.42 --note "console usage 2026-10-26"
  ```
- **`dispatched` left behind by a killed process.** Resolve it the same way.

The journal refuses to load if it is damaged, so spending history can never be silently reset. Fix or restore the file before continuing.

## Change settings

You can change these between cycles: the model, effort, turns, samples, news items, caps, prices, costs, band, minimum trade, horizon and buffer. Signals already open keep the horizon they were created with.

Changing `name` or `symbols` is refused for an existing account. Start a fresh account by pointing `data_dir` somewhere new (or moving the old directory away) and running `init`.

## Calendar upkeep

The NYSE calendar covers 2026 and 2027. Before 2028 starts, add the 2028 schedule from https://www.nyse.com/markets/hours-calendars to `calendar.go` and its tests. Until then the agent stops with an explicit error rather than guessing.
