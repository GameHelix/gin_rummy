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

	"github.com/ismatsamadov/trader/internal/agent"
	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/market"
)

const usage = `trader — real-time paper-trading agent for US stocks (no real orders)

  trader init                  create the paper account
  trader run [--dry-run]       run one cycle now
  trader daemon                run a cycle after every close
  trader status [--json]       agent vs buy-and-hold
  trader budget status
  trader budget resolve --attempt ID --cost-usd N --note TEXT
  trader models                list model IDs for TRADER_MODEL

All commands take --config PATH (default config/agent.json).
Secrets go in .env: ANTHROPIC_API_KEY and TRADER_MODEL.`

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
	if err := config.LoadEnv(".env"); err != nil {
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
	c, err := config.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	budget := &analyst.Budget{Path: filepath.Join(c.DataDir, "budget.json"), DailyUSD: c.MaxDailyLLMUSD, MonthlyUSD: c.MaxMonthlyLLMUSD, Now: e.Now}
	switch cmd {
	case "init":
		st, err := agent.InitState(c, e.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "created %s: $%.2f paper cash (%s)\n", st.Name, st.InitialCash.Dollars(), agent.StatePath(c))
		return nil
	case "run":
		runner, err := newAgent(c, budget, e, nil, !*dryRun)
		if err != nil {
			return err
		}
		rep, err := runner.RunCycle(ctx, *dryRun)
		printCycle(out, rep)
		return err
	case "daemon":
		runner, err := newAgent(c, budget, e, out, true)
		if err != nil {
			return err
		}
		if _, err := runner.Analyst.Client.Models.Get(ctx, c.Model, anthropic.ModelGetParams{}); err != nil {
			return fmt.Errorf("TRADER_MODEL is not available to this API key (see `trader models`): %w", err)
		}
		fmt.Fprintf(out, "trader daemon: %s\n", c.Name)
		return runner.Daemon(ctx)
	case "status":
		st, err := agent.LoadState(c)
		if err != nil {
			return err
		}
		summary, err := budget.Summary(ctx)
		if err != nil {
			return err
		}
		report := agent.BuildStatus(c, st, &summary, e.Now())
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

// newAgent wires the agent; log is nil for one-off runs, which print only a summary.
func newAgent(c config.Config, budget *analyst.Budget, e env, log io.Writer, withAnalyst bool) (*agent.Agent, error) {
	a := &agent.Agent{Config: c, Market: market.PriceSource{HTTP: e.HTTP, BaseURL: e.MarketURL}, Now: e.Now, Sleep: e.Sleep, Log: log}
	if withAnalyst {
		key := apiKey()
		if key == "" {
			return nil, errors.New("set ANTHROPIC_API_KEY in .env or the environment to run the analyst")
		}
		a.Analyst = &analyst.Analyst{Config: c, Client: anthropicClient(key, e), News: market.NewsSource{HTTP: e.HTTP, BaseURL: e.NewsURL}, Budget: budget, Log: log, Sleep: e.Sleep}
	}
	return a, nil
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

func printCycle(out io.Writer, rep agent.CycleReport) {
	if rep.Status == "" {
		return
	}
	head := rep.Session + " · " + strings.ReplaceAll(rep.Status, "_", " ")
	if d := rep.Decision; d != nil {
		head += fmt.Sprintf(" · cost $%.2f", d.CostUSD)
	}
	fmt.Fprintln(out, head)
	item := func(label, value string) { fmt.Fprintf(out, "  %-9s %s\n", label+":", value) }
	if b := rep.Briefing; b != nil {
		closes := []string{}
		for _, ind := range b.Universe {
			closes = append(closes, fmt.Sprintf("%s %.2f", ind.Symbol, ind.Close))
		}
		if b.Market != nil {
			closes = append(closes, fmt.Sprintf("%s %.2f", b.Market.Symbol, b.Market.Close))
		}
		item("data", strings.Join(closes, " · ")+" (no model call, nothing saved)")
	}
	for _, f := range rep.Fills {
		item("filled", fmt.Sprintf("%s %d %s @ $%.2f on %s", strings.ToUpper(f.Side), f.Quantity, f.Symbol, f.Price.Dollars(), f.Session))
	}
	for _, r := range rep.Rejections {
		item("rejected", fmt.Sprintf("%s on %s: %s", r.Symbol, r.Session, r.Reason))
	}
	for _, e := range rep.CorporateEvents {
		item("event", e)
	}
	if d := rep.Decision; d != nil {
		ratings := []string{}
		for _, line := range d.Plan {
			ratings = append(ratings, line.Symbol+" "+string(line.Rating))
		}
		item("ratings", strings.Join(ratings, " · "))
		orders := []string{}
		for _, o := range rep.Orders {
			orders = append(orders, fmt.Sprintf("%s %d %s", strings.ToUpper(o.Side), o.Quantity, o.Symbol))
		}
		if len(orders) == 0 {
			orders = append(orders, "none")
		}
		item("orders", strings.Join(orders, ", "))
	}
	for _, n := range rep.Notes {
		item("note", n)
	}
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
