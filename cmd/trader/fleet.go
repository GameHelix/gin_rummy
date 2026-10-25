package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"

	"github.com/candylario/trader/internal/agent"
	"github.com/candylario/trader/internal/config"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/news"
	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/server"
	"github.com/candylario/trader/internal/store"
	"github.com/candylario/trader/internal/vault"
)

var fleetCommands = map[string]bool{"serve": true, "import": true, "tenant": true, "key": true, "agent": true}

var fleetFlags = map[string][]string{
	"serve": {"config"}, "import": {"config"},
	"tenant add":   {"config", "name", "plan"},
	"tenant plan":  {"config", "tenant", "plan"},
	"key add":      {"config", "tenant", "kind", "label", "mode"},
	"key list":     {"config", "tenant"},
	"key revoke":   {"config", "tenant", "key"},
	"agent add":    {"config", "tenant", "name", "mode", "keys"},
	"agent config": {"config", "tenant", "agent"},
	"agent start":  {"config", "tenant", "agent"},
	"agent stop":   {"config", "tenant", "agent"},
	"agent status": {"config", "tenant", "agent"},
	"agent list":   {"config", "tenant"},
}

func runFleet(ctx context.Context, args []string, out io.Writer, e env) error {
	cmd, rest := args[0], args[1:]
	if cmd == "tenant" || cmd == "key" || cmd == "agent" {
		if len(rest) == 0 {
			return fmt.Errorf("choose a %s command; run `trader help`", cmd)
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	}
	names, ok := fleetFlags[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q; run `trader help`", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config/agent.json", "agent configuration")
	tenantFlag := fs.String("tenant", "", "tenant ID")
	agentFlag := fs.String("agent", "", "agent ID")
	keyFlag := fs.String("key", "", "key ID")
	name := fs.String("name", "", "name")
	mode := fs.String("mode", config.Testnet, "testnet or live")
	kind := fs.String("kind", "", "key kind")
	label := fs.String("label", "main", "key label")
	planPath := fs.String("plan", "", "tier limits as JSON")
	keys := fs.String("keys", "", "key IDs, comma separated")
	if err := fs.Parse(rest); err != nil {
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
	defaults, err := config.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	svc, closeAll, err := openFleet(ctx, defaults, e)
	if err != nil {
		return err
	}
	defer closeAll()
	values := map[string]string{"tenant": *tenantFlag, "agent": *agentFlag, "key": *keyFlag}
	ids := func(want ...string) ([]uuid.UUID, error) {
		var out []uuid.UUID
		for _, w := range want {
			id, err := uuid.Parse(values[w])
			if err != nil {
				return nil, fmt.Errorf("--%s must be an ID", w)
			}
			out = append(out, id)
		}
		return out, nil
	}
	switch cmd {
	case "serve":
		return serve(ctx, svc, e, out)
	case "import":
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		return importStandalone(ctx, defaults, raw, svc, out)
	case "tenant add":
		var en fleet.Entitlements
		if err := readJSON(*planPath, &en); err != nil {
			return err
		}
		t, _, err := svc.AddTenant(ctx, uuid.Nil, *name, en)
		if err == nil {
			fmt.Fprintf(out, "tenant %s\n", t.ID)
		}
		return err
	case "tenant plan":
		t, err := ids("tenant")
		if err != nil {
			return err
		}
		var en fleet.Entitlements
		if err := readJSON(*planPath, &en); err != nil {
			return err
		}
		stopped, err := svc.SetEntitlements(ctx, t[0], en)
		for id, why := range stopped {
			fmt.Fprintf(out, "stopped agent %s: %s\n", id, why)
		}
		if err == nil {
			fmt.Fprintln(out, "plan updated")
		}
		return err
	case "key list":
		t, err := ids("tenant")
		if err != nil {
			return err
		}
		keys, err := svc.Store.Credentials(ctx, t[0])
		for _, k := range keys {
			state := "active"
			if k.Revoked {
				state = "revoked"
			}
			fmt.Fprintf(out, "%s  %-9s %-10s %s  %s\n", k.ID, k.Kind, k.Label, k.Hint, state)
		}
		return err
	case "key revoke":
		tk, err := ids("tenant", "key")
		if err != nil {
			return err
		}
		stopped, err := svc.RevokeKey(ctx, tk[0], tk[1])
		for _, id := range stopped {
			fmt.Fprintf(out, "stopped agent %s: it has no key of that kind now\n", id)
		}
		if err == nil {
			fmt.Fprintln(out, "key revoked")
		}
		return err
	case "key add":
		t, err := ids("tenant")
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(e.In, 1<<16))
		if err != nil {
			return err
		}
		c, _, err := svc.AddKey(ctx, t[0], fleet.NewKey{Kind: *kind, Label: *label, Mode: *mode, Secret: raw})
		clear(raw)
		if err == nil {
			fmt.Fprintf(out, "key %s (%s %s)\n", c.ID, c.Kind, c.Hint)
		}
		return err
	case "agent add":
		t, err := ids("tenant")
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		var kids []uuid.UUID
		for _, k := range strings.Split(*keys, ",") {
			if k = strings.TrimSpace(k); k == "" {
				continue
			}
			kid, err := uuid.Parse(k)
			if err != nil {
				return fmt.Errorf("--keys: %q is not an ID", k)
			}
			kids = append(kids, kid)
		}
		row, _, err := svc.AddAgent(ctx, t[0], fleet.NewAgent{Name: *name, Mode: *mode, Config: raw, Keys: kids, By: "cli"})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "agent %s (stopped; start it with `trader agent start`)\n", row.ID)
		return nil
	case "agent config":
		ta, err := ids("tenant", "agent")
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		v, err := svc.SetConfig(ctx, ta[0], ta[1], raw, "cli")
		if err == nil {
			fmt.Fprintf(out, "config version %d\n", v)
		}
		return err
	case "agent start", "agent stop":
		ta, err := ids("tenant", "agent")
		if err != nil {
			return err
		}
		if cmd == "agent start" {
			return svc.Start(ctx, ta[0], ta[1])
		}
		return svc.Stop(ctx, ta[0], ta[1])
	case "agent status":
		ta, err := ids("tenant", "agent")
		if err != nil {
			return err
		}
		spec, err := svc.Spec(ctx, ta[0], ta[1])
		if err != nil {
			return err
		}
		r, err := buildEngine(spec.Config, spec.Keys, e, nil, nil).Report(ctx)
		if err == nil {
			fmt.Fprintln(out, r.Text())
		}
		return err
	case "agent list":
		t, err := ids("tenant")
		if err != nil {
			return err
		}
		agents, err := svc.Store.Agents(ctx, t[0])
		for _, a := range agents {
			fmt.Fprintf(out, "%s  %-12s %-8s %-8s version %d\n", a.ID, a.Name, a.Mode, a.Status, a.Version)
		}
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// openFleet connects to the database and the vault.
func openFleet(ctx context.Context, defaults config.Config, e env) (*fleet.Service, func(), error) {
	url, master := os.Getenv("DATABASE_URL"), os.Getenv("ENGINE_MASTER_KEY")
	if url == "" || master == "" {
		return nil, nil, errors.New("set DATABASE_URL and ENGINE_MASTER_KEY")
	}
	v, err := vault.New(master, 1)
	if err != nil {
		return nil, nil, err
	}
	if err := pg.Migrate(ctx, url); err != nil {
		return nil, nil, err
	}
	st, err := pg.Open(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	svc := &fleet.Service{Store: st, Vault: v, DataDir: defaults.DataDir, Model: defaults.Model, Check: checkSpec(e), CheckKey: checkKey(e, defaults.Model)}
	return svc, st.Close, nil
}

// checkKey tries a key as it is added: a model key must reach the engine's
// model, an exchange key must read its account where it trades and, live,
// must not withdraw, and a Telegram bot says hello in its chat.
func checkKey(e env, model string) func(context.Context, string, string, fleet.Keys) error {
	return func(ctx context.Context, kind, mode string, k fleet.Keys) error {
		switch kind {
		case fleet.KindAnthropic:
			client := modelClient(k.Anthropic, e)
			_, err := client.Models.Get(ctx, model, anthropic.ModelGetParams{})
			var ae *anthropic.Error
			switch {
			case errors.As(err, &ae) && ae.StatusCode == http.StatusUnauthorized:
				return errors.New("the Anthropic key is not valid")
			case errors.As(err, &ae) && (ae.StatusCode == http.StatusForbidden || ae.StatusCode == http.StatusNotFound):
				return fmt.Errorf("the Anthropic key cannot use %s", model)
			case err != nil:
				return fmt.Errorf("could not check the Anthropic key: %w", err)
			}
		case fleet.KindBinance:
			ex := exchange(mode, k, e)
			if err := ex.SyncTime(ctx); err != nil {
				return err
			}
			if _, err := ex.Account(ctx); err != nil {
				return fmt.Errorf("the Binance key cannot read the %s account: %w", mode, err)
			}
			if mode == config.Live {
				p, err := ex.Permissions(ctx)
				if err == nil && (p.EnableWithdrawals == nil || *p.EnableWithdrawals) {
					err = errors.New("this Binance key can withdraw funds; create one with withdrawals disabled")
				}
				return err
			}
		case fleet.KindTelegram:
			if err := bot(k, e).Send(ctx, "✅ Connected. Your trading agents report here."); err != nil {
				return fmt.Errorf("the Telegram bot cannot message chat %d: %w", k.TelegramChat, err)
			}
		}
		return nil
	}
}

// checkSpec tries an agent's keys, in its own mode, before it starts.
func checkSpec(e env) func(context.Context, fleet.Spec) error {
	return func(ctx context.Context, spec fleet.Spec) error {
		eng := buildEngine(spec.Config, spec.Keys, e, nil, nil)
		if _, err := eng.Analyst.Client.Models.Get(ctx, spec.Config.Model, anthropic.ModelGetParams{}); err != nil {
			return fmt.Errorf("the Anthropic key cannot use the configured model: %w", err)
		}
		if err := eng.Exchange.SyncTime(ctx); err != nil {
			return err
		}
		if _, err := eng.Exchange.Account(ctx); err != nil {
			return fmt.Errorf("the Binance key cannot read the %s account: %w", spec.Config.Mode, err)
		}
		if spec.Config.Mode == config.Live {
			p, err := eng.Exchange.Permissions(ctx)
			if err == nil && (p.EnableWithdrawals == nil || *p.EnableWithdrawals) {
				err = errors.New("this Binance key can withdraw funds; create one with withdrawals disabled")
			}
			return err
		}
		return nil
	}
}

// newsPool gives agents that read the same feeds one shared cache.
type newsPool struct {
	e      env
	mu     sync.Mutex
	caches map[string]*news.Cache
}

func (p *newsPool) source(urls []string) news.Source {
	if len(urls) == 0 {
		return nil
	}
	key := strings.Join(urls, "\n")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.caches[key] == nil {
		p.caches[key] = &news.Cache{Feeds: news.Feeds{HTTP: p.e.HTTP, URLs: urls}, TTL: 4 * time.Minute}
	}
	return p.caches[key]
}

func serve(ctx context.Context, svc *fleet.Service, e env, out io.Writer) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(out, "%s %s\n", time.Now().UTC().Format("2006-01-02 15:04:05Z"), fmt.Sprintf(format, args...))
	}
	pool := &newsPool{e: e, caches: map[string]*news.Cache{}}
	sup := &fleet.Supervisor{
		Service: svc, Every: 30 * time.Second, Logf: logf,
		Build: func(spec fleet.Spec) (*agent.Engine, error) {
			eng := buildEngine(spec.Config, spec.Keys, e, pool.source(spec.Config.NewsFeeds), fleet.Prefixed(out, "["+spec.Name+"] "))
			if eng.Bot != nil && eng.Bot.ChatID == 0 {
				eng.Bot = nil
			}
			return eng, nil
		},
		Ship: func(ctx context.Context, spec fleet.Spec) {
			who := pg.Agent{Tenant: spec.Tenant, ID: spec.Agent, Name: spec.Name, Mode: spec.Config.Mode}
			pg.Ship(ctx, svc.Store, agent.EventsDir(spec.Config), func() (pg.Agent, bool) { return who, true }, 2*time.Second, logf)
		},
	}
	if err := startAPI(ctx, svc, sup, e, logf); err != nil {
		return err
	}
	logf("engine serving every active agent")
	return sup.Run(ctx)
}

// startAPI opens the engine API, for the app, when ENGINE_API_TOKEN is set.
func startAPI(ctx context.Context, svc *fleet.Service, sup *fleet.Supervisor, e env, logf func(string, ...any)) error {
	token := os.Getenv("ENGINE_API_TOKEN")
	if token == "" {
		logf("engine API off; set ENGINE_API_TOKEN to open it")
		return nil
	}
	if len(token) < 32 {
		return errors.New("ENGINE_API_TOKEN must be at least 32 characters")
	}
	l, err := net.Listen("tcp", cmp.Or(os.Getenv("ENGINE_API_ADDR"), ":8080"))
	if err != nil {
		return fmt.Errorf("engine API: %w", err)
	}
	srv := &server.Server{Fleet: svc, Agents: sup, Token: token, Logf: logf,
		Report: func(ctx context.Context, spec fleet.Spec) (agent.Report, error) {
			return buildEngine(spec.Config, fleet.Keys{}, e, nil, nil).Report(ctx)
		}}
	go func() {
		if err := srv.Serve(ctx, l); err != nil && ctx.Err() == nil {
			logf("engine API stopped: %v", err)
		}
	}()
	logf("engine API on %s", l.Addr())
	return nil
}

// importStandalone moves the agent configured in .env and the config file
// into the database under its own IDs, with its keys sealed in the vault,
// and starts it. The standalone agent must not be running.
func importStandalone(ctx context.Context, c config.Config, raw []byte, svc *fleet.Service, out io.Writer) error {
	lctx, cancel := context.WithTimeout(ctx, time.Second)
	unlock, err := store.LockFile(lctx, filepath.Join(c.Dir(), "agent.lock"))
	cancel()
	if err != nil {
		return errors.New("the standalone agent is running; stop it first (docker compose down)")
	}
	defer unlock()
	st, err := agent.LoadState(c)
	if err != nil {
		return err
	}
	tenant, err1 := uuid.Parse(st.TenantID)
	id, err2 := uuid.Parse(st.AgentID)
	if err1 != nil || err2 != nil {
		return errors.New("run the standalone agent once so its account gets IDs, then import")
	}
	k, err := keysFromEnv()
	if err != nil {
		return err
	}
	if k.Anthropic == "" || k.BinanceKey == "" || k.BinanceSecret == "" {
		return errors.New("set ANTHROPIC_API_KEY, BINANCE_API_KEY and BINANCE_API_SECRET in .env to import")
	}
	secrets := map[string]any{
		fleet.KindAnthropic: map[string]string{"key": k.Anthropic},
		fleet.KindBinance:   map[string]string{"key": k.BinanceKey, "secret": k.BinanceSecret},
	}
	if k.TelegramToken != "" && k.TelegramChat != 0 {
		secrets[fleet.KindTelegram] = map[string]any{"token": k.TelegramToken, "chat_id": k.TelegramChat}
	}
	en := fleet.Entitlements{Symbols: c.Symbols, MaxAgents: 10, MinDecisionIntervalHours: c.DecisionIntervalHours, MaxEventDecisionsPerDay: c.MaxEventDecisionsPerDay, Live: true}
	if err := svc.Adopt(ctx, tenant, id, c.Name, c.Mode, en, raw); err != nil {
		return err
	}
	for kind, v := range secrets {
		s, err := json.Marshal(v)
		if err != nil {
			return err
		}
		key, _, err := svc.AddKey(ctx, tenant, fleet.NewKey{Kind: kind, Label: "imported", Mode: c.Mode, Secret: s})
		clear(s)
		if err != nil {
			return err
		}
		if err := svc.AttachKey(ctx, tenant, id, key.ID); err != nil {
			return err
		}
	}
	dest := filepath.Join(svc.DataDir, "agents", id.String())
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := os.Rename(c.Dir(), filepath.Join(dest, c.Mode)); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(c.DataDir, "budget.json"), filepath.Join(dest, "budget.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := svc.Start(ctx, tenant, id); err != nil {
		return fmt.Errorf("imported, but the agent could not start: %w", err)
	}
	fmt.Fprintf(out, "Imported %s as agent %s of tenant %s. Run `trader serve`.\n", c.Name, id, tenant)
	return nil
}
