package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"

	"github.com/candylario/trader/internal/agent"
	"github.com/candylario/trader/internal/analyst"
	"github.com/candylario/trader/internal/audit"
	"github.com/candylario/trader/internal/binance"
	"github.com/candylario/trader/internal/config"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/news"
	"github.com/candylario/trader/internal/num"
	"github.com/candylario/trader/internal/pg"
)

const usage = `trader — AI crypto trading agent for Binance spot

  trader check            check keys, permissions, balances and news
  trader init             start the account with capital_usd from the config
  trader run              trade around the clock on its own, with keys from .env
  trader decide           make one decision now
  trader decide --dry-run show the live briefing; no model call, no trade
  trader status [--json]  equity, positions, last decision and AI spend
  trader budget status
  trader budget resolve --attempt ID --cost-usd N --note TEXT

Multi-user engine (needs DATABASE_URL and ENGINE_MASTER_KEY):
  trader serve            run every active agent (what Docker runs), and the
                          engine API on ENGINE_API_ADDR (:8080) when
                          ENGINE_API_TOKEN is set
  trader import           move this standalone agent into the database
  trader vault rewrap     seal every stored key under the current master key,
                          after a rotation (see the README)
  trader tenant add --name NAME --plan FILE
  trader tenant plan --tenant ID --plan FILE
  trader key add --tenant ID --kind anthropic|binance [--mode testnet|live] [--label TEXT] < key.json
  trader key list --tenant ID
  trader key revoke --tenant ID --key ID
  trader agent add --tenant ID --name NAME --mode testnet|live --config FILE --keys ID,ID
  trader agent config|start|stop|status --tenant ID --agent ID [--config FILE]
  trader agent list --tenant ID

All commands take --config PATH (default config/agent.json).
TRADER_MODE in .env picks testnet (default) or live.`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, env{}); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// env holds what tests replace: endpoints, the clock and standard input.
type env struct {
	HTTP       *http.Client
	BinanceURL string
	APIURL     string
	Now        func() time.Time
	In         io.Reader
}

func run(ctx context.Context, args []string, out io.Writer, e env) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(out, usage)
		return nil
	}
	if err := config.LoadEnv(".env"); err != nil {
		return err
	}
	if e.HTTP == nil {
		e.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.In == nil {
		e.In = os.Stdin
	}
	if fleetCommands[args[0]] {
		return runFleet(ctx, args, out, e)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "budget" {
		if len(rest) == 0 {
			return errors.New("choose `budget status` or `budget resolve`")
		}
		cmd, rest = "budget "+rest[0], rest[1:]
	}
	allowed := map[string][]string{
		"check": {"config"}, "init": {"config"}, "run": {"config"}, "decide": {"config", "dry-run"}, "status": {"config", "json"},
		"budget status": {"config"}, "budget resolve": {"config", "attempt", "cost-usd", "note"},
	}
	names, ok := allowed[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q; run `trader help`", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config/agent.json", "agent configuration")
	asJSON := fs.Bool("json", false, "print machine-readable output")
	dryRun := fs.Bool("dry-run", false, "build the briefing from live data without calling the model or trading")
	attempt := fs.String("attempt", "", "budget attempt ID")
	cost := fs.Float64("cost-usd", -1, "provider-verified cost")
	note := fs.String("note", "", "reconciliation note")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, usage)
			return nil
		}
		return err
	}
	var bad error
	fs.Visit(func(f *flag.Flag) {
		if !slices.Contains(names, f.Name) {
			bad = fmt.Errorf("--%s does not apply to %s", f.Name, cmd)
		}
	})
	if bad != nil {
		return bad
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments; run `trader help`")
	}
	c, err := config.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	eng, err := build(c, e, out)
	if err != nil {
		return err
	}
	budget := eng.Analyst.Budget
	switch cmd {
	case "check":
		return check(ctx, out, c, eng)
	case "init":
		st, err := eng.Init(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Created the %s account with %s %s (%s).\n", st.Mode, st.Capital, c.QuoteAsset, agent.StatePath(c))
		return nil
	case "run":
		if err := modelAvailable(ctx, eng, c); err != nil {
			return err
		}
		if url := os.Getenv("DATABASE_URL"); url != "" {
			if err := startShipper(ctx, url, c, out); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "trader %s on %s\n", c.Name, c.Mode)
		return eng.Run(ctx)
	case "decide":
		if *dryRun {
			b, err := eng.Preview(ctx)
			if err != nil {
				return err
			}
			printBriefing(out, b)
			return nil
		}
		if err := modelAvailable(ctx, eng, c); err != nil {
			return err
		}
		d, err := eng.DecideOnce(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Decision #%d saved; cost $%.2f.\n", d.ID, d.CostUSD)
		return nil
	case "status":
		r, err := eng.Report(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(out, r)
		}
		fmt.Fprintln(out, r.Text())
		return nil
	case "budget status":
		summary, err := budget.Summary(ctx)
		if err != nil {
			return err
		}
		return writeJSON(out, summary)
	case "budget resolve":
		if *attempt == "" || *cost < 0 || strings.TrimSpace(*note) == "" {
			return errors.New("budget resolve needs --attempt, --cost-usd and --note")
		}
		if err := budget.Resolve(ctx, *attempt, *cost, *note); err != nil {
			return err
		}
		fmt.Fprintf(out, "Recorded $%.4f for attempt %s.\n", *cost, *attempt)
		return nil
	}
	return nil
}

// build wires a standalone agent with the keys in .env.
func build(c config.Config, e env, log io.Writer) (*agent.Engine, error) {
	k, err := keysFromEnv()
	if err != nil {
		return nil, err
	}
	return buildEngine(c, k, e, news.Feeds{HTTP: e.HTTP, URLs: c.NewsFeeds}, log), nil
}

// keysFromEnv reads the standalone agent's keys from the environment.
func keysFromEnv() (fleet.Keys, error) {
	return fleet.Keys{Anthropic: apiKey(), BinanceKey: os.Getenv("BINANCE_API_KEY"), BinanceSecret: os.Getenv("BINANCE_API_SECRET")}, nil
}

// buildEngine wires an agent to Binance and the model with the given keys.
func buildEngine(c config.Config, k fleet.Keys, e env, src news.Source, log io.Writer) *agent.Engine {
	return &agent.Engine{
		Config:   c,
		Exchange: exchange(c.Mode, k, e),
		// One budget for testnet and live: both spend the same model key.
		Analyst: &analyst.Analyst{Config: c, Client: modelClient(k.Anthropic, e), Log: log,
			Budget: &analyst.Budget{Path: filepath.Join(c.DataDir, "budget.json"), DailyUSD: c.MaxDailyLLMUSD, MonthlyUSD: c.MaxMonthlyLLMUSD, Now: e.Now}},
		News: src,
		Now:  e.Now,
		Log:  log,
	}
}

func exchange(mode string, k fleet.Keys, e env) *binance.Client {
	base, data := binance.TestnetURL, binance.DataURL
	if mode == config.Live {
		base = binance.LiveURL
	}
	if e.BinanceURL != "" {
		base, data = e.BinanceURL, e.BinanceURL
	}
	return &binance.Client{HTTP: exchangeClient(e.HTTP), BaseURL: base, DataURL: data, Key: k.BinanceKey, Secret: k.BinanceSecret, Now: e.Now}
}

func modelClient(key string, e env) anthropic.Client {
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if e.APIURL != "" {
		opts = append(opts, option.WithBaseURL(e.APIURL))
	}
	return anthropic.NewClient(opts...)
}

// exchangeClient never follows redirects, so the API key header cannot be
// carried to another host.
func exchangeClient(base *http.Client) *http.Client {
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// startShipper copies the engine's audit log to Postgres in the background.
// The log on disk stays the record; the database catches up after an outage.
func startShipper(ctx context.Context, url string, c config.Config, out io.Writer) error {
	if err := pg.Migrate(ctx, url); err != nil {
		return err
	}
	store, err := pg.Open(ctx, url)
	if err != nil {
		return err
	}
	dir := agent.EventsDir(c)
	var who pg.Agent
	identify := func() (pg.Agent, bool) {
		if who.ID == uuid.Nil {
			events, err := audit.ReadAfter(dir, 0, 1)
			if err != nil || len(events) == 0 {
				return who, false
			}
			who = pg.Agent{Tenant: events[0].Tenant, ID: events[0].Agent, Name: c.Name, Mode: c.Mode}
		}
		return who, true
	}
	logf := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	go func() {
		defer store.Close()
		pg.Ship(ctx, store, dir, identify, 2*time.Second, logf)
	}()
	return nil
}

func apiKey() string {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key
	}
	return os.Getenv("ANTROPIC_API_KEY") // misspelled name kept for older .env files
}

func modelAvailable(ctx context.Context, eng *agent.Engine, c config.Config) error {
	if apiKey() == "" {
		return errors.New("set ANTHROPIC_API_KEY in .env")
	}
	if _, err := eng.Analyst.Client.Models.Get(ctx, c.Model, anthropic.ModelGetParams{}); err != nil {
		return fmt.Errorf("TRADER_MODEL is not available to this API key: %w", err)
	}
	return nil
}

// check tries every outside dependency without trading or calling the model.
func check(ctx context.Context, out io.Writer, c config.Config, eng *agent.Engine) error {
	failed := false
	report := func(err error, ok string) {
		if err != nil {
			failed = true
			fmt.Fprintf(out, "FAIL %v\n", err)
			return
		}
		fmt.Fprintf(out, "ok   %s\n", ok)
	}
	fmt.Fprintf(out, "mode %s · %s · capital %g %s\n", c.Mode, strings.Join(c.Symbols, ", "), c.CapitalUSD, c.QuoteAsset)
	report(modelAvailable(ctx, eng, c), "model "+c.Model)
	ex := eng.Exchange
	report(ex.SyncTime(ctx), "Binance reachable")
	_, err := ex.Symbols(ctx, c.Symbols)
	report(err, "market rules for "+strings.Join(c.Symbols, ", "))
	acct, err := ex.Account(ctx)
	if err == nil {
		capital, _ := num.FromFloat(c.CapitalUSD)
		if free := acct.Balances[c.QuoteAsset].Free; free.Less(capital) {
			err = fmt.Errorf("only %s %s free; capital_usd needs %s", free, c.QuoteAsset, capital)
		}
	}
	report(err, fmt.Sprintf("API key reads the account; %s %s free", acct.Balances[c.QuoteAsset].Free, c.QuoteAsset))
	if c.Mode == config.Live {
		p, err := ex.Permissions(ctx)
		if err == nil && (p.EnableWithdrawals == nil || *p.EnableWithdrawals) {
			err = errors.New("this API key can withdraw funds; disable withdrawals for it")
		}
		report(err, "API key cannot withdraw")
		if err == nil && !p.IPRestrict {
			fmt.Fprintln(out, "--   API key has no IP restriction; restricting it to your IP is safer")
		}
	}
	report(ex.TestOrder(ctx, binance.OrderRequest{Symbol: c.Symbols[0], Side: "BUY", QuoteQty: num.Int(10), ClientID: "check"}), "API key may place orders (test order, nothing traded)")
	if url := os.Getenv("DATABASE_URL"); url != "" {
		store, err := pg.Open(ctx, url)
		if err == nil {
			err = store.Ping(ctx)
			store.Close()
		}
		report(err, "database reachable")
	}
	items, err := eng.News.Fetch(ctx, eng.Now(), 72*time.Hour, nil)
	report(err, fmt.Sprintf("%d headlines from %d feeds", len(items), len(c.NewsFeeds)))
	if failed {
		return errors.New("some checks failed")
	}
	return nil
}

func printBriefing(out io.Writer, b analyst.Briefing) {
	for _, v := range b.Coins {
		fmt.Fprintf(out, "%-8s %12.2f · 24h %+.1f%% · 7d %+.1f%% · 30d %+.1f%% · RSI 1h %.0f · 1d %.0f\n", v.Symbol, v.Price, v.Return24H*100, v.Return7D*100, v.Return30D*100, v.RSI14H, v.RSI14D)
	}
	fmt.Fprintf(out, "%d headlines in the briefing\n", len(b.Headlines))
	for _, h := range b.Headlines[:min(3, len(b.Headlines))] {
		fmt.Fprintf(out, "  %s · %s\n", h.Source, h.Title)
	}
	fmt.Fprintln(out, "preview only: no model call, no trade, nothing saved")
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
