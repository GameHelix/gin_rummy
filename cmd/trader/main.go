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

	"github.com/ismatsamadov/trader/internal/trader"
)

const usage = `trader — a real-time paper-trading agent for US stocks

After each NYSE close, the analyst model researches the universe with live tools (price
history and news) and rates every stock. Deterministic rules turn the ratings
into orders that fill at the next open in a local paper account, next to a
buy-and-hold benchmark. No real orders are ever sent.

Commands:
  trader init     [--config PATH]             create the paper account
  trader run      [--config PATH] [--dry-run] run one cycle now (no-op when up to date)
  trader daemon   [--config PATH]             run a cycle after every session close
  trader status   [--config PATH] [--json]    agent vs benchmark, positions, scorecard
  trader budget   status  [--config PATH]
  trader budget   resolve [--config PATH] --attempt ID --cost-usd N --note TEXT
  trader models                               list models available to your API key

The default config is config/agent.json. Put ANTHROPIC_API_KEY and TRADER_MODEL in .env.`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, env{}); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// env holds what tests replace: endpoints, clock and sleep.
type env struct {
	HTTP      *http.Client
	MarketURL string
	NewsURL   string
	APIURL    string
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) error
}

func run(ctx context.Context, args []string, out io.Writer, e env) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(out, usage)
		return nil
	}
	if err := trader.LoadEnv(".env"); err != nil {
		return err
	}
	if e.HTTP == nil {
		e.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	cmd, rest := args[0], args[1:]
	if cmd == "budget" {
		if len(rest) == 0 {
			return errors.New("choose `budget status` or `budget resolve`")
		}
		cmd, rest = "budget "+rest[0], rest[1:]
	}
	allowed := map[string][]string{
		"init": {"config"}, "run": {"config", "dry-run"}, "daemon": {"config"}, "status": {"config", "json"},
		"budget status": {"config"}, "budget resolve": {"config", "attempt", "cost-usd", "note"}, "models": {},
	}
	names, ok := allowed[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q; run `trader help`", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config/agent.json", "agent configuration")
	dryRun := fs.Bool("dry-run", false, "fetch live data and build the briefing without calling the model or saving")
	asJSON := fs.Bool("json", false, "print machine-readable output")
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
	if cmd == "models" {
		return listModels(ctx, out, e)
	}
	c, err := trader.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	budget := &trader.Budget{Path: filepath.Join(c.DataDir, "budget.json"), DailyUSD: c.MaxDailyLLMUSD, MonthlyUSD: c.MaxMonthlyLLMUSD, Now: e.Now}
	switch cmd {
	case "init":
		st, err := trader.InitState(c, e.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Created paper account %q with $%.2f at %s.\nNext: `trader run --dry-run` to check live data, then `trader run` or `trader daemon`.\n", st.Name, st.InitialCash.Dollars(), trader.StatePath(c))
		return nil
	case "run":
		agent, err := newAgent(c, budget, e, out, !*dryRun)
		if err != nil {
			return err
		}
		rep, err := agent.RunCycle(ctx, *dryRun)
		printCycle(out, rep)
		return err
	case "daemon":
		agent, err := newAgent(c, budget, e, out, true)
		if err != nil {
			return err
		}
		if _, err := agent.Analyst.Client.Models.Get(ctx, c.Model, anthropic.ModelGetParams{}); err != nil {
			return fmt.Errorf("TRADER_MODEL is not available to this API key (see `trader models`): %w", err)
		}
		fmt.Fprintf(out, "trader daemon: %s. Press Ctrl-C to stop.\n", c.Name)
		return agent.Daemon(ctx)
	case "status":
		st, err := trader.LoadState(c)
		if err != nil {
			return err
		}
		summary, err := budget.Summary(ctx)
		if err != nil {
			return err
		}
		report := trader.BuildStatus(c, st, &summary, e.Now())
		if *asJSON {
			return writeJSON(out, report)
		}
		fmt.Fprint(out, report.Text())
		return nil
	case "budget status":
		summary, err := budget.Summary(ctx)
		if err != nil {
			return err
		}
		return writeJSON(out, summary)
	case "budget resolve":
		if *attempt == "" || *cost < 0 {
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

func apiKey() string {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key
	}
	return os.Getenv("ANTROPIC_API_KEY") // misspelled name kept for older .env files
}

func anthropicClient(key string, e env) anthropic.Client {
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if e.APIURL != "" {
		opts = append(opts, option.WithBaseURL(e.APIURL))
	}
	return anthropic.NewClient(opts...)
}

func newAgent(c trader.Config, budget *trader.Budget, e env, out io.Writer, withAnalyst bool) (*trader.Agent, error) {
	agent := &trader.Agent{Config: c, Market: trader.MarketSource{HTTP: e.HTTP, BaseURL: e.MarketURL}, Now: e.Now, Sleep: e.Sleep, Log: out}
	if withAnalyst {
		key := apiKey()
		if key == "" {
			return nil, errors.New("set ANTHROPIC_API_KEY in .env or the environment to run the analyst")
		}
		agent.Analyst = &trader.Analyst{Config: c, Client: anthropicClient(key, e), News: trader.NewsSource{HTTP: e.HTTP, BaseURL: e.NewsURL}, Budget: budget, Log: out, Sleep: e.Sleep}
	}
	return agent, nil
}

func listModels(ctx context.Context, out io.Writer, e env) error {
	key := apiKey()
	if key == "" {
		return errors.New("set ANTHROPIC_API_KEY in .env or the environment")
	}
	client := anthropicClient(key, e)
	pager := client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	for pager.Next() {
		m := pager.Current()
		fmt.Fprintf(out, "%s\t%s\n", m.ID, m.DisplayName)
	}
	return pager.Err()
}

func printCycle(out io.Writer, rep trader.CycleReport) {
	if rep.Status == "" {
		return
	}
	fmt.Fprintf(out, "\nCycle for %s: %s\n", rep.Session, strings.ReplaceAll(rep.Status, "_", " "))
	for _, e := range rep.CorporateEvents {
		fmt.Fprintf(out, "  corporate action: %s\n", e)
	}
	for _, f := range rep.Fills {
		fmt.Fprintf(out, "  filled %s %d %s @ $%.2f on %s (fee $%.2f)\n", strings.ToUpper(f.Side), f.Quantity, f.Symbol, f.Price.Dollars(), f.Session, f.Fee.Dollars())
	}
	for _, r := range rep.Rejections {
		fmt.Fprintf(out, "  rejected %s on %s: %s\n", r.Symbol, r.Session, r.Reason)
	}
	if b := rep.Briefing; b != nil {
		fmt.Fprintf(out, "  dry run: live data is ready; the analyst would receive this briefing (no model call, nothing saved)\n")
		for _, ind := range b.Universe {
			fmt.Fprintf(out, "  %-6s close %9.2f  5d %+6.2f%%  20d %+6.2f%%  60d %+6.2f%%  vol20 %.2f%%\n", ind.Symbol, ind.Close, ind.Return5D*100, ind.Return20D*100, ind.Return60D*100, ind.Volatility20D*100)
		}
		if m := b.Market; m != nil {
			fmt.Fprintf(out, "  market %-4s close %9.2f  5d %+6.2f%%  20d %+6.2f%%\n", m.Symbol, m.Close, m.Return5D*100, m.Return20D*100)
		}
	}
	if d := rep.Decision; d != nil {
		if d.Summary != "" {
			fmt.Fprintf(out, "  view: %s\n", d.Summary)
		}
		for _, line := range d.Plan {
			sig := d.Signals[line.Symbol]
			fmt.Fprintf(out, "  %-6s %-11s (%s) %s\n", line.Symbol, line.Rating, sig.Confidence, sig.Rationale)
		}
		if len(rep.Orders) == 0 {
			fmt.Fprintln(out, "  no orders: every change was a hold or inside the no-trade band")
		}
		for _, o := range rep.Orders {
			fmt.Fprintf(out, "  order for next open: %s %d %s (%s)\n", strings.ToUpper(o.Side), o.Quantity, o.Symbol, o.Reason)
		}
		fmt.Fprintf(out, "  estimated model cost: $%.4f\n", d.CostUSD)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
