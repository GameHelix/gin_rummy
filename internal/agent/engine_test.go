package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wickwise/trader/internal/analyst"
	"github.com/wickwise/trader/internal/audit"
	"github.com/wickwise/trader/internal/binance"
	"github.com/wickwise/trader/internal/config"
	"github.com/wickwise/trader/internal/news"
	"github.com/wickwise/trader/internal/num"
	"github.com/wickwise/trader/internal/telegram"
	"github.com/wickwise/trader/internal/testkit"
)

type rig struct {
	t     *testing.T
	feed  *http.Client
	mu    sync.Mutex
	clock time.Time
	ex    *testkit.Binance
	api   *testkit.API
	tg    *testkit.Telegram
	news  []testkit.Headline
	eng   *Engine
}

func (r *rig) now() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock }

func (r *rig) advance(d time.Duration) { r.mu.Lock(); r.clock = r.clock.Add(d); r.mu.Unlock() }

func (r *rig) headline(title string, at time.Time) {
	r.mu.Lock()
	r.news = append(r.news, testkit.Headline{Title: title, At: at})
	r.mu.Unlock()
}

func rate(btc, eth string) func(string) (int, any) {
	return testkit.SubmitStep(map[string]string{"BTCUSDT": btc, "ETHUSDT": eth}, testkit.CitePrices)
}

// newRig starts a fresh testnet account against fake Binance, model,
// news and Telegram servers. Each call to tick is one pass of the loop.
func newRig(t *testing.T, script ...func(string) (int, any)) *rig {
	t.Helper()
	r := &rig{t: t, clock: testkit.At(t, "2026-10-25 12:00")}
	r.ex = testkit.NewBinance(r.now)
	r.api = &testkit.API{Script: script}
	r.tg = &testkit.Telegram{ChatID: 7, Now: r.now}
	c := testkit.Config(t.TempDir())
	feed := testkit.NewsServer(t, func() []testkit.Headline {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]testkit.Headline(nil), r.news...)
	})
	c.NewsFeeds, r.feed = []string{feed.URL}, feed.Client()
	r.eng = newEngine(t, c, r)
	if _, err := r.eng.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.open()
	return r
}

func newEngine(t *testing.T, c config.Config, r *rig) *Engine {
	return &Engine{
		Config:   c,
		Exchange: r.ex.Client(t),
		Analyst: &analyst.Analyst{Config: c, Client: testkit.Client(r.api.Server(t).URL), Sleep: func(context.Context, time.Duration) error { return nil },
			Budget: &analyst.Budget{Path: filepath.Join(c.Dir(), "budget.json"), DailyUSD: c.MaxDailyLLMUSD, MonthlyUSD: c.MaxMonthlyLLMUSD, Now: r.now}},
		News: news.Feeds{HTTP: r.feed, URLs: c.NewsFeeds},
		Bot:  &telegram.Bot{HTTP: http.DefaultClient, BaseURL: r.tg.Server(t).URL, Token: "T", ChatID: 7},
		Now:  r.now,
	}
}

func (r *rig) open() {
	r.t.Helper()
	if err := r.eng.open(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// tick runs one pass of the loop and, like Run, finishes a decision it
// started and handles commands that arrived meanwhile.
func (r *rig) tick() {
	ctx := context.Background()
	r.eng.tick(ctx)
	if r.eng.job != nil {
		r.eng.finish(ctx, <-r.eng.results)
	}
	r.eng.handleLater(ctx)
}

func (r *rig) sent(substr string) bool {
	for _, m := range r.tg.Sent() {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// ledgerMatchesExchange checks the agent's ledger against the fake account,
// which started with 1000 USDT of which 100 was given to the agent.
func (r *rig) ledgerMatchesExchange() {
	r.t.Helper()
	l := r.eng.st.Ledger
	if got := r.ex.Balance("USDT").Sub(num.Int(900)); got.Cmp(l.Cash) != 0 {
		r.t.Fatalf("ledger cash %s, exchange shows %s for the agent", l.Cash, got)
	}
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		if got := r.ex.Balance(strings.TrimSuffix(s, "USDT")); got.Cmp(l.Holdings[s]) != 0 {
			r.t.Fatalf("ledger %s %s, exchange %s", s, l.Holdings[s], got)
		}
	}
}

func TestInitGivesTheAgentItsCapitalAndAHoldingTwin(t *testing.T) {
	r := newRig(t)
	st := r.eng.st
	if st.Capital.String() != "100" || st.Ledger.Cash.String() != "100" || st.NextID != 1 || st.Mode != config.Testnet {
		t.Fatalf("state %+v", st)
	}
	bench, _ := st.Benchmark.Equity(map[string]num.Dec{"BTCUSDT": num.Int(80000), "ETHUSDT": num.Int(2400)})
	if st.Benchmark.Cash.String() != "10" || bench.Float() < 99 || bench.Float() > 100.5 {
		t.Fatalf("the twin must invest 45%% in each coin once: %+v (%s)", st.Benchmark, bench)
	}
	if _, err := r.eng.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "already initialised") {
		t.Fatalf("a second init must fail: %v", err)
	}
	poor := &rig{t: t, clock: r.clock}
	poor.ex = testkit.NewBinance(poor.now)
	poor.ex.SetBalance("USDT", num.Int(50))
	poor.api, poor.tg = &testkit.API{}, &testkit.Telegram{ChatID: 7, Now: poor.now}
	if _, err := newEngine(t, testkit.Config(t.TempDir()), poor).Init(context.Background()); err == nil || !strings.Contains(err.Error(), "capital_usd needs") {
		t.Fatalf("init without the capital must fail: %v", err)
	}
}

func TestScheduledDecisionTradesOnBinanceAndReports(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.tick()
	orders := r.ex.OrderList()
	if len(orders) != 1 || orders[0].Side != "BUY" || orders[0].Symbol != "BTCUSDT" || !strings.HasSuffix(orders[0].ClientID, "-1-btc-buy") || orders[0].Quote.Float() < 44 {
		t.Fatalf("expected one $45 BTC buy: %+v", orders)
	}
	r.ledgerMatchesExchange()
	st := r.eng.st
	if len(st.Decisions) != 1 || len(st.Trades) != 1 || len(st.Pending) != 0 || len(st.Open) != 1 || st.NextID != 2 || st.Ledger.FeesUSD.Sign() <= 0 {
		t.Fatalf("state after the decision: %+v", st)
	}
	if !r.sent("🧠 Decision #1: scheduled check") || !r.sent("Bought 0.0005") || !r.sent("🧪") {
		t.Fatalf("telegram: %q", r.tg.Sent())
	}
	saved, err := LoadState(r.eng.Config)
	if err != nil || saved.Ledger.Cash.Cmp(st.Ledger.Cash) != 0 {
		t.Fatalf("the ledger must be saved: %v", err)
	}
	r.advance(30 * time.Second)
	r.tick()
	if r.api.Count() != 1 || len(r.ex.OrderList()) != 1 {
		t.Fatal("nothing is due 30 seconds later")
	}
}

func TestSharpMoveTriggersADecisionAfterTheCooldown(t *testing.T) {
	r := newRig(t, rate("hold", "hold"))
	r.tick()
	r.advance(30 * time.Minute)
	r.ex.SetPrice("BTCUSDT", r.ex.Price("BTCUSDT", r.now())*1.04)
	r.tick()
	if r.api.Count() != 1 {
		t.Fatal("a move inside the cooldown must not trigger a decision")
	}
	r.advance(31 * time.Minute)
	r.tick()
	if r.api.Count() != 2 || !strings.HasPrefix(r.eng.st.Last().Trigger, "BTC moved +4.0% in the last hour") {
		t.Fatalf("expected a move-triggered decision, got %d calls, trigger %q", r.api.Count(), r.eng.st.Last().Trigger)
	}
}

func TestUnclearOrderReplyIsLookedUpNeverResent(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultFailAfterFill)
	r.tick()
	if len(r.eng.st.Pending) != 1 || r.eng.st.Ledger.Cash.String() != "100" || !r.sent("no clear answer from Binance") {
		t.Fatalf("an unclear reply must leave the order pending: %+v", r.eng.st.Pending)
	}
	// The process restarts before the next pass.
	r.eng = newEngine(t, r.eng.Config, r)
	r.open()
	if len(r.eng.st.Pending) != 0 || len(r.eng.st.Trades) != 1 || len(r.ex.OrderList()) != 1 {
		t.Fatalf("the filled order must be booked once: pending %v, trades %d, exchange orders %d", r.eng.st.Pending, len(r.eng.st.Trades), len(r.ex.OrderList()))
	}
	r.ledgerMatchesExchange()
}

func TestOrderThatNeverArrivedIsDropped(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultDropBefore)
	r.tick()
	if len(r.eng.st.Pending) != 1 {
		t.Fatal("a dropped connection leaves the outcome unknown")
	}
	r.advance(30 * time.Second)
	r.tick()
	if len(r.eng.st.Pending) != 1 {
		t.Fatal("an order may still arrive within two minutes")
	}
	r.advance(3 * time.Minute)
	r.tick()
	if len(r.eng.st.Pending) != 0 || len(r.ex.OrderList()) != 0 || r.eng.st.Ledger.Cash.String() != "100" || !r.sent("never reached Binance") {
		t.Fatalf("pending %v, orders %v", r.eng.st.Pending, r.ex.OrderList())
	}
}

func TestRejectedOrderIsReportedAndNotRetried(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultReject)
	r.tick()
	if len(r.eng.st.Pending) != 0 || len(r.ex.OrderList()) != 0 || !r.sent("Binance rejected the order to buy BTC") {
		t.Fatalf("pending %v", r.eng.st.Pending)
	}
	r.advance(time.Minute)
	r.tick()
	if len(r.ex.OrderList()) != 0 {
		t.Fatal("a rejected order must wait for the next decision")
	}
}

func TestStopFromTelegramBlocksOrdersOfARunningDecision(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.eng.inbox <- inbound{name: "stop", source: sourceTelegram, sent: r.now(), offset: 3, received: r.now()}
	r.tick()
	r.eng.handleLater(context.Background())
	if len(r.eng.st.Decisions) != 1 || len(r.ex.OrderList()) != 0 || r.eng.st.HaltKind != haltManual || r.eng.st.TelegramOffset != 3 {
		t.Fatalf("a stop sent during the decision must block its orders: orders %v, halt %q", r.ex.OrderList(), r.eng.st.Halted)
	}
	r.advance(5 * time.Hour)
	r.tick()
	if r.api.Count() != 1 {
		t.Fatal("no decisions while stopped")
	}
	r.eng.handle(context.Background(), inbound{name: "resume", source: sourceTelegram, sent: r.now()})
	r.eng.handle(context.Background(), inbound{name: "status", source: sourceTelegram, sent: r.now()})
	if r.eng.st.Halted != "" || !r.sent("Trading resumed.") || !r.sent("Account: $100.00") {
		t.Fatalf("resume or status failed: %q", r.tg.Sent())
	}
}

func TestStaleCommandsAreIgnoredExceptStop(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.tick()
	old := r.now().Add(-time.Hour)
	r.eng.handle(context.Background(), inbound{name: "sellall", source: sourceTelegram, sent: old, received: r.now()})
	if r.eng.st.Halted != "" || len(r.ex.OrderList()) != 1 || !r.sent("Ignored /sellall") {
		t.Fatal("a sell-all queued while offline must not run")
	}
	r.eng.handle(context.Background(), inbound{name: "stop", source: sourceTelegram, sent: old, received: r.now()})
	if r.eng.st.HaltKind != haltManual {
		t.Fatal("a stop is honoured even late")
	}
}

func TestTelegramPollerDeliversCommands(t *testing.T) {
	r := newRig(t)
	r.tg.Command("/status")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.eng.poll(ctx, 0)
	select {
	case in := <-r.eng.inbox:
		if in.name != "status" || in.offset != 2 {
			t.Fatalf("got %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no command delivered")
	}
}

func TestDrawdownSellsEverythingAndStops(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	if len(r.ex.OrderList()) != 2 {
		t.Fatalf("expected two buys: %v", r.ex.OrderList())
	}
	r.advance(10 * time.Minute)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.3)
	}
	r.tick()
	orders := r.ex.OrderList()
	if r.eng.st.HaltKind != haltDrawdown || len(orders) != 4 || orders[2].Side != "SELL" || orders[3].Side != "SELL" || !r.sent("everything is being sold") {
		t.Fatalf("halt %q, orders %v", r.eng.st.Halted, orders)
	}
	r.ledgerMatchesExchange()
	r.advance(5 * time.Hour)
	r.tick()
	if r.api.Count() != 1 {
		t.Fatal("no decisions after a drawdown stop")
	}
	r.eng.handle(context.Background(), inbound{name: "resume", source: sourceTelegram, sent: r.now()})
	r.advance(time.Minute)
	r.tick()
	if r.eng.st.HaltKind == haltDrawdown || r.api.Count() != 2 {
		t.Fatalf("after /resume the drawdown is measured from the new equity: %q, %d calls", r.eng.st.Halted, r.api.Count())
	}
}

func TestDailyLossLimitPausesUntilTheNextDay(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.advance(10 * time.Minute)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.85)
	}
	r.tick()
	if r.eng.st.HaltKind != haltDaily || len(r.ex.OrderList()) != 2 {
		t.Fatalf("a 13%% day must pause trading without selling: %q", r.eng.st.Halted)
	}
	r.advance(12 * time.Hour)
	r.tick()
	if r.eng.st.Halted != "" || !r.sent("Summary for Sun Oct 25") || !r.sent("New day: trading resumed") {
		t.Fatalf("the next UTC day resumes trading: %q", r.eng.st.Halted)
	}
}

func TestMajorNewsTriggersADecision(t *testing.T) {
	r := newRig(t, rate("hold", "hold"))
	r.headline("Bitcoin ETF approval hopes build", r.now().Add(-3*time.Hour))
	r.tick()
	if r.api.Count() != 1 || r.sent("Market news") {
		t.Fatal("headlines already out at startup are not alerts")
	}
	r.advance(65 * time.Minute)
	r.headline("Ether developers meet", r.now().Add(-time.Minute))
	r.headline("Major exchange hack hits Bitcoin holders", r.now().Add(-2*time.Minute))
	r.tick()
	if r.api.Count() != 2 || !strings.HasPrefix(r.eng.st.Last().Trigger, "headline news:") || !r.sent("Market news") {
		t.Fatalf("calls %d, trigger %q", r.api.Count(), r.eng.st.Last().Trigger)
	}
	r.advance(65 * time.Minute)
	r.tick()
	if r.api.Count() != 2 {
		t.Fatal("a headline only triggers once")
	}
}

func TestBalanceShortfallStopsTrading(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetBalance("USDT", num.Int(40))
	r.open()
	if r.eng.st.HaltKind != haltMismatch || !r.sent("Binance holds less than the agent's books") {
		t.Fatalf("halt %q", r.eng.st.Halted)
	}
	r.tick()
	if r.api.Count() != 0 || len(r.ex.OrderList()) != 0 {
		t.Fatal("no trading while the balance is short")
	}
}

func TestLiveModeRefusesKeysThatCanWithdraw(t *testing.T) {
	r := &rig{t: t, clock: testkit.At(t, "2026-10-25 12:00")}
	r.ex = testkit.NewBinance(r.now)
	r.ex.SetRestriction("enableWithdrawals", true)
	r.api, r.tg = &testkit.API{}, &testkit.Telegram{ChatID: 7, Now: r.now}
	c := testkit.Config(t.TempDir())
	c.Mode = config.Live
	if _, err := newEngine(t, c, r).Init(context.Background()); err == nil || !strings.Contains(err.Error(), "can withdraw") {
		t.Fatalf("init with a withdrawing key must fail: %v", err)
	}
	r.ex.SetRestriction("enableWithdrawals", false)
	r.eng = newEngine(t, c, r)
	if _, err := r.eng.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.ex.SetRestriction("enableWithdrawals", true)
	r.open()
	if r.eng.st.HaltKind != haltKey || r.sent("[testnet]") {
		t.Fatalf("a key that gains withdrawal rights must stop trading: %q", r.eng.st.Halted)
	}
}

func TestCallsAreGradedAtTheirDueTime(t *testing.T) {
	r := newRig(t, rate("strong_buy", "sell"))
	r.tick()
	entry := r.eng.st.Open[0].Entry
	r.advance(10 * time.Hour)
	r.ex.SetPrice("BTCUSDT", entry.Float()*1.02)
	r.ex.SetPrice("ETHUSDT", r.ex.Price("ETHUSDT", r.now())*1.01)
	r.advance(15 * time.Hour)
	r.tick()
	if len(r.eng.st.Scored) != 2 {
		t.Fatalf("both calls are due: %+v", r.eng.st.Scored)
	}
	for _, sc := range r.eng.st.Scored {
		want := sc.Symbol == "BTCUSDT"
		if sc.Hit != want || sc.Decision != 1 {
			t.Fatalf("%s: a rise makes the buy right and the sell wrong: %+v", sc.Symbol, sc)
		}
	}
}

func TestPlanOrdersRespectsLimitsAndExchangeRules(t *testing.T) {
	c := testkit.Config("unused")
	rules := map[string]binance.Symbol{
		"BTCUSDT": {StepSize: num.MustParse("0.00001"), MinNotional: num.Int(5)},
		"ETHUSDT": {StepSize: num.MustParse("0.0001"), MinNotional: num.Int(5)},
	}
	prices := map[string]num.Dec{"BTCUSDT": num.Int(80000), "ETHUSDT": num.Int(2000)}
	sig := func(btc, eth analyst.Rating) map[string]analyst.Signal {
		return map[string]analyst.Signal{"BTCUSDT": {Rating: btc}, "ETHUSDT": {Rating: eth}}
	}
	empty := Ledger{Cash: num.Int(100), Holdings: map[string]num.Dec{}}
	plans, _ := PlanOrders(c, rules, empty, prices, sig(analyst.RatingStrongBuy, analyst.RatingBuy))
	if len(plans) != 2 || plans[0].QuoteQty.String() != "45" || plans[1].QuoteQty.String() != "22.5" {
		t.Fatalf("strong_buy and buy targets: %+v", plans)
	}
	// 60% in BTC rated hold is trimmed to the 45% cap.
	heavy := Ledger{Cash: num.Int(40), Holdings: map[string]num.Dec{"BTCUSDT": num.MustParse("0.00075")}}
	plans, _ = PlanOrders(c, rules, heavy, prices, sig(analyst.RatingHold, analyst.RatingHold))
	if len(plans) != 1 || plans[0].Side != "SELL" || plans[0].Qty.String() != "0.00018" {
		t.Fatalf("trim: %+v", plans)
	}
	// Selling dust below the minimum notional is skipped; a full exit sells whole steps.
	dust := Ledger{Cash: num.Int(97), Holdings: map[string]num.Dec{"BTCUSDT": num.MustParse("0.00003"), "ETHUSDT": num.MustParse("0.01234567")}}
	plans, _ = PlanOrders(c, rules, dust, prices, sig(analyst.RatingSell, analyst.RatingStrongSell))
	if len(plans) != 1 || plans[0].Symbol != "ETHUSDT" || plans[0].Qty.String() != "0.0123" {
		t.Fatalf("exit: %+v", plans)
	}
	// A move inside the band does not trade.
	near := Ledger{Cash: num.Int(80), Holdings: map[string]num.Dec{"BTCUSDT": num.MustParse("0.00025")}}
	if plans, _ := PlanOrders(c, rules, near, prices, sig(analyst.RatingBuy, analyst.RatingHold)); len(plans) != 0 {
		t.Fatalf("20%% held against a 22.5%% target is inside the band: %+v", plans)
	}
	// Targets above the exposure cap are scaled down, and buys never exceed cash.
	c.MaxPositionWeight = 0.6
	plans, _ = PlanOrders(c, rules, empty, prices, sig(analyst.RatingStrongBuy, analyst.RatingStrongBuy))
	if len(plans) != 2 || plans[0].QuoteQty.String() != "45" || plans[1].QuoteQty.String() != "45" {
		t.Fatalf("exposure cap: %+v", plans)
	}
	c.MaxPositionWeight = 0.45
	poor := Ledger{Cash: num.Int(20), Holdings: map[string]num.Dec{"ETHUSDT": num.MustParse("0.04")}}
	plans, _ = PlanOrders(c, rules, poor, prices, sig(analyst.RatingStrongBuy, analyst.RatingHold))
	if len(plans) != 2 || plans[0].Side != "BUY" || plans[0].QuoteQty.String() != "20" || plans[1].Side != "SELL" || plans[1].Qty.String() != "0.0175" {
		t.Fatalf("buys are limited to cash on hand; the 80%% ETH position is trimmed: %+v", plans)
	}
}

func TestSpentBudgetWaitsForTheNextDay(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.eng.Analyst.Budget.DailyUSD = 0.0001
	r.tick()
	if r.api.Count() != 0 || !r.sent("LLM spending limit reached") {
		t.Fatalf("the first decision must stop at the cap: %q", r.tg.Sent())
	}
	alerts := len(r.tg.Sent())
	for i := 0; i < 23; i++ {
		r.advance(30 * time.Minute)
		r.tick()
	}
	if len(r.tg.Sent()) != alerts {
		t.Fatalf("no retries or repeated alerts before the UTC day ends: %q", r.tg.Sent()[alerts:])
	}
	r.eng.Analyst.Budget.DailyUSD = 5
	r.advance(30 * time.Minute)
	r.tick()
	if r.api.Count() != 1 || !r.sent("Summary for Sun Oct 25") {
		t.Fatal("the next day decides again")
	}
}

func (r *rig) command(name string) {
	r.eng.handle(context.Background(), inbound{name: name, source: sourceTelegram, sent: r.now(), received: r.now()})
	r.eng.handleLater(context.Background())
}

func TestASecondSellAllNeverSellsTwice(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.eng.inbox <- inbound{name: "sellall", source: sourceTelegram, sent: r.now(), received: r.now()}
	r.command("sellall")
	orders := r.ex.OrderList()
	ids := map[string]bool{}
	for _, o := range orders {
		if ids[o.ClientID] {
			t.Fatalf("client order ID reused: %s", o.ClientID)
		}
		ids[o.ClientID] = true
	}
	if len(orders) != 4 || orders[2].Side != "SELL" || orders[3].Side != "SELL" || r.eng.st.Liquidate || r.eng.st.HaltKind != haltManual {
		t.Fatalf("two buys and exactly two sells expected: %v (liquidate %v)", orders, r.eng.st.Liquidate)
	}
	r.ledgerMatchesExchange()
	if !r.sent("Everything sellable is sold") {
		t.Fatalf("telegram: %q", r.tg.Sent())
	}
}

func TestSellOffKeepsTryingUntilEverythingIsSold(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.advance(10 * time.Minute)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.3)
	}
	r.ex.SetFault(testkit.FaultDropBefore) // the first sell never reaches the exchange
	r.tick()
	if len(r.ex.OrderList()) != 3 || !r.eng.st.Liquidate || len(r.eng.st.Pending) != 1 {
		t.Fatalf("ETH is sold even though the BTC sell failed: %v", r.ex.OrderList())
	}
	for i := 0; i < 8; i++ {
		r.advance(30 * time.Second)
		r.tick()
	}
	if len(r.ex.OrderList()) != 4 || r.eng.st.Liquidate || len(r.eng.st.Pending) != 0 || r.eng.st.HaltKind != haltDrawdown {
		t.Fatalf("the sell-off must retry BTC once its lost order is dropped: %v", r.ex.OrderList())
	}
	r.ledgerMatchesExchange()
}

func TestRecoveredOrderIsBookedOnlyWithItsFees(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultFailAfterFill)
	r.ex.SetTradesDown(true)
	r.tick()
	r.advance(30 * time.Second)
	r.tick()
	if len(r.eng.st.Pending) != 1 || len(r.eng.st.Trades) != 0 {
		t.Fatal("an order whose fees cannot be read must stay pending")
	}
	r.ex.SetTradesDown(false)
	r.advance(30 * time.Second)
	r.tick()
	if len(r.eng.st.Pending) != 0 || len(r.eng.st.Trades) != 1 || r.eng.st.Ledger.FeesUSD.Sign() <= 0 {
		t.Fatalf("pending %v, trades %v", r.eng.st.Pending, r.eng.st.Trades)
	}
	r.ledgerMatchesExchange()
}

func TestNoDecisionWhileAnOrderIsUnresolved(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultDropBefore)
	r.tick()
	r.command("decide")
	if r.api.Count() != 1 || len(r.ex.OrderList()) != 0 || !r.sent("an earlier order is still being checked") {
		t.Fatalf("calls %d, orders %v", r.api.Count(), r.ex.OrderList())
	}
}

func TestLossLimitIsCheckedAgainBeforeOrders(t *testing.T) {
	r := newRig(t)
	r.api.Script = []func(string) (int, any){
		rate("strong_buy", "strong_buy"),
		func(req string) (int, any) {
			// Prices fall while the model is thinking.
			for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
				r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.75)
			}
			return rate("buy", "buy")(req)
		},
	}
	r.tick()
	r.advance(4 * time.Hour)
	r.tick()
	if len(r.ex.OrderList()) != 2 || r.eng.st.HaltKind != haltDaily || !r.sent("No trades: trading is stopped") {
		t.Fatalf("orders %v, halt %q", r.ex.OrderList(), r.eng.st.Halted)
	}
}

func TestManualStopOutlastsAutomaticOnes(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.advance(10 * time.Minute)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.85)
	}
	r.tick()
	r.command("stop")
	r.advance(13 * time.Hour)
	r.tick()
	if r.eng.st.HaltKind != haltManual || r.api.Count() != 1 {
		t.Fatalf("a manual stop must survive the daily reset: %q", r.eng.st.Halted)
	}
}

func TestNewsDuringTheCooldownIsKept(t *testing.T) {
	r := newRig(t, rate("hold", "hold"))
	r.tick()
	r.advance(10 * time.Minute)
	r.headline("SEC sues exchange over Bitcoin ETF", r.now().Add(-time.Minute))
	r.tick()
	if r.api.Count() != 1 || !r.sent("Market news") {
		t.Fatal("the headline is reported at once but waits for the cooldown")
	}
	r.advance(51 * time.Minute)
	r.tick()
	if r.api.Count() != 2 || !strings.HasPrefix(r.eng.st.Last().Trigger, "headline news:") {
		t.Fatalf("the queued headline triggers a decision after the cooldown: %d calls", r.api.Count())
	}
	if b, _ := os.ReadFile(filepath.Join(r.eng.Config.Dir(), "decisions", "2", "briefing.json")); !strings.Contains(string(b), "SEC sues exchange") {
		t.Fatal("the triggering headline must be in the briefing")
	}
}

func TestFeesPaidInBNBAreValued(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFeeAsset("BNB")
	r.ex.SetBalance("BNB", num.Int(1))
	r.tick()
	l := r.eng.st.Ledger
	if l.OffBookFees.Sign() <= 0 || l.FeesUSD.Cmp(l.OffBookFees) != 0 || l.Holdings["BTCUSDT"].Cmp(r.ex.OrderList()[0].Qty) != 0 {
		t.Fatalf("a BNB fee leaves the coins whole and is charged off-book: %+v", l)
	}
	r.ledgerMatchesExchange()
}

func TestPartialFillIsBooked(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultPartial)
	r.tick()
	if len(r.eng.st.Trades) != 1 || r.eng.st.Ledger.Cash.Float() < 75 {
		t.Fatalf("half of a $45 buy: %+v", r.eng.st.Ledger)
	}
	r.ledgerMatchesExchange()
}

func TestModelTextCannotCarryTelegramCommands(t *testing.T) {
	if got := untrusted("Rate it (/sellall) or /resume now; 1/2 is fine"); got != "Rate it (∕sellall) or ∕resume now; 1/2 is fine" {
		t.Fatalf("got %q", got)
	}
}

func TestDrawdownUnderAManualStopSellsNothingButSaysSo(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.command("stop")
	r.advance(10 * time.Minute)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.3)
	}
	r.tick()
	r.tick()
	if len(r.ex.OrderList()) != 2 || r.eng.st.Liquidate || r.eng.st.HaltKind != haltManual || !r.sent("You stopped trading, so nothing is sold") {
		t.Fatalf("a manual stop keeps holdings but reports the drawdown: orders %v, %q", r.ex.OrderList(), r.tg.Sent())
	}
}

func TestResumeKeepsLossBaselinesUnlessALimitFired(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.advance(10 * time.Minute)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.95)
	}
	r.tick()
	r.command("stop")
	r.command("resume")
	if r.eng.st.RiskBase.String() != "100" || r.eng.st.DayEquity.String() != "100" {
		t.Fatalf("resuming a manual stop must not move the loss baselines: %s %s", r.eng.st.RiskBase, r.eng.st.DayEquity)
	}
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		r.ex.SetPrice(s, r.ex.Price(s, r.now())*0.85)
	}
	r.tick()
	if r.eng.st.HaltKind != haltDaily {
		t.Fatalf("expected the daily limit: %q", r.eng.st.Halted)
	}
	r.command("resume")
	if r.eng.st.RiskBase.String() != "100" || r.eng.st.DayEquity.Float() > 90 {
		t.Fatalf("after a daily stop only the day counts from now: %s %s", r.eng.st.RiskBase, r.eng.st.DayEquity)
	}
}

func TestSellOffGivesUpOnACoinBinanceKeepsRejecting(t *testing.T) {
	r := newRig(t, rate("strong_buy", "strong_buy"))
	r.tick()
	r.ex.SetRejecting("BTCUSDT")
	r.command("sellall")
	for i := 0; i < 6; i++ {
		r.advance(30 * time.Second)
		r.tick()
	}
	if r.ex.RejectedCount() != sellTries || !r.sent("Could not sell BTC after 3 tries") || !r.sent("Sell-off finished, except BTC") || r.eng.st.Liquidate {
		t.Fatalf("rejections %d, liquidate %v, %q", r.ex.RejectedCount(), r.eng.st.Liquidate, r.tg.Sent())
	}
	if orders := r.ex.OrderList(); len(orders) != 3 || orders[2].Symbol != "ETHUSDT" {
		t.Fatalf("ETH must still be sold: %v", orders)
	}
}

func TestOrderThatExpiresUnfilledIsReported(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFault(testkit.FaultExpireEmpty)
	r.tick()
	if len(r.eng.st.Pending) != 0 || len(r.eng.st.Trades) != 0 || r.eng.st.Ledger.Cash.String() != "100" || !r.sent("expired without filling") {
		t.Fatalf("pending %v, %q", r.eng.st.Pending, r.tg.Sent())
	}
}

func TestCommandsAreAnsweredWhileTheModelThinks(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.api.Script = []func(string) (int, any){func(req string) (int, any) {
		<-release
		return rate("hold", "hold")(req)
	}}
	r.eng.Config.PollSeconds = 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(ctx) }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %q", what, r.tg.Sent())
			}
		}
	}
	waitFor("the model call", func() bool { return r.api.Count() == 1 })
	r.tg.Command("/status")
	waitFor("the status reply", func() bool { return r.sent("📊 Status") })
	if r.sent("🧠 Decision #1") {
		t.Fatal("the decision cannot be done yet")
	}
	close(release)
	waitFor("the decision", func() bool { return r.sent("🧠 Decision #1") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEngineAPICommandsRunInTheLoopLikeTelegramOnes(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); len(r.ex.OrderList()) != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no first trade: %q", r.tg.Sent())
		}
	}
	// After one answer the loop has said all it had to say.
	if _, err := r.eng.Status(ctx); err != nil {
		t.Fatal(err)
	}
	messages := len(r.tg.Sent())
	st, err := r.eng.Status(ctx)
	if err != nil || st.Trades != 1 || st.Halted != "" || len(r.tg.Sent()) != messages {
		t.Fatalf("status is read quietly: %+v %v", st, err)
	}
	for _, step := range []struct {
		name string
		ok   bool
		want string
	}{
		{"stop", true, "Trading stopped"},
		{"decide", false, "Send /resume first"},
		{"resume", true, "Trading resumed"},
		{"sellall", true, "Selling everything"},
	} {
		got, err := r.eng.Command(ctx, step.name, "telegram")
		if err != nil || got.OK != step.ok || !strings.Contains(got.Text, step.want) {
			t.Fatalf("%s: %+v %v", step.name, got, err)
		}
	}
	// The loop announces before it takes the next command, so once the
	// last answer is on Telegram the earlier ones are too.
	if _, err := r.eng.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.sent("Trading stopped") || !r.sent("Trading resumed") || !r.sent("Selling everything") || r.sent("Send /resume first") {
		t.Fatalf("only commands that took effect are announced on Telegram: %q", r.tg.Sent())
	}
	if orders := r.ex.OrderList(); len(orders) != 2 || orders[1].Side != "SELL" {
		t.Fatalf("the sell-off runs at once: %v", orders)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.Command(context.Background(), "stop", "app"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a stopped agent takes no commands: %v", err)
	}
	events, err := audit.ReadAfter(EventsDir(r.eng.Config), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var fromAPI []string
	for _, ev := range events {
		var c commandReceived
		if ev.Type == evCommand && json.Unmarshal(ev.Body, &c) == nil && c.Source == sourceAPI && c.Via == "telegram" {
			fromAPI = append(fromAPI, c.Name)
		}
	}
	if strings.Join(fromAPI, " ") != "stop decide resume sellall" || events[len(events)-1].Type != evAgentStopped {
		t.Fatalf("commands on record: %v, last event %s", fromAPI, events[len(events)-1].Type)
	}
	// Each message Telegram got is on record as a notice, in the same order.
	var notices []string
	for _, ev := range events {
		var n notice
		if ev.Type == evNotice && json.Unmarshal(ev.Body, &n) == nil {
			notices = append(notices, n.Text)
		}
	}
	if !slices.Equal(notices, r.tg.Sent()) {
		t.Fatalf("notices %q\ndiffer from Telegram %q", notices, r.tg.Sent())
	}
}

func TestPlanTrimsAboveTheExposureCapEvenInsideTheBand(t *testing.T) {
	c := testkit.Config("unused")
	rules := map[string]binance.Symbol{
		"BTCUSDT": {StepSize: num.MustParse("0.00001"), MinNotional: num.Int(5), MinQty: num.MustParse("0.00001")},
		"ETHUSDT": {StepSize: num.MustParse("0.0001"), MinNotional: num.Int(5), MinQty: num.MustParse("0.0001")},
	}
	prices := map[string]num.Dec{"BTCUSDT": num.Int(80000), "ETHUSDT": num.Int(2000)}
	// 46% in each coin, 92% in all, against a 90% cap.
	l := Ledger{Cash: num.Int(800), Holdings: map[string]num.Dec{"BTCUSDT": num.MustParse("0.0575"), "ETHUSDT": num.MustParse("2.3")}}
	hold := map[string]analyst.Signal{"BTCUSDT": {Rating: analyst.RatingHold}, "ETHUSDT": {Rating: analyst.RatingHold}}
	plans, _ := PlanOrders(c, rules, l, prices, hold)
	if len(plans) != 2 || plans[0].Side != "SELL" || plans[1].Side != "SELL" {
		t.Fatalf("both positions are trimmed back under the cap: %+v", plans)
	}
	rules["BTCUSDT"] = binance.Symbol{StepSize: num.MustParse("0.00001"), MinNotional: num.Int(5), MinQty: num.MustParse("0.01")}
	if plans, _ := PlanOrders(c, rules, l, prices, hold); len(plans) != 1 || plans[0].Symbol != "ETHUSDT" {
		t.Fatalf("a sell below the exchange's minimum quantity is skipped: %+v", plans)
	}
}

func TestOldDecisionFoldersArePruned(t *testing.T) {
	r := newRig(t)
	root := filepath.Join(r.eng.Config.Dir(), "decisions")
	for i := 1; i <= keepDecisions+5; i++ {
		os.MkdirAll(filepath.Join(root, strconv.Itoa(i)), 0o755)
	}
	r.eng.prune()
	entries, _ := os.ReadDir(root)
	if len(entries) != keepDecisions {
		t.Fatalf("kept %d folders", len(entries))
	}
	if _, err := os.Stat(filepath.Join(root, "1")); !os.IsNotExist(err) {
		t.Fatal("the oldest folders go first")
	}
}

func TestReportSurvivesZeroEquity(t *testing.T) {
	c := testkit.Config("unused")
	st := State{Name: "x", Mode: config.Testnet, Ledger: Ledger{Holdings: map[string]num.Dec{}}, Benchmark: Ledger{Holdings: map[string]num.Dec{}}}
	r, err := BuildReport(c, st, map[string]num.Dec{"BTCUSDT": num.Int(1), "ETHUSDT": num.Int(1)}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(r); err != nil || strings.Contains(r.Text(), "NaN") {
		t.Fatalf("zero equity must render: %v %q", err, r.Text())
	}
}

func TestTestnetFillsPayTheLiveFeeOnPaper(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.ex.SetFeeAsset("none")
	r.tick()
	o := r.ex.OrderList()[0]
	l := r.eng.st.Ledger
	fee := o.Quote.Mul(simulatedFee)
	if l.FeesUSD.Cmp(fee) != 0 || l.Holdings["BTCUSDT"].Cmp(o.Qty) != 0 {
		t.Fatalf("a 0.1%% fee must be booked: fees %s, want %s", l.FeesUSD, fee)
	}
	if onExchange := r.ex.Balance("USDT").Sub(num.Int(900)); onExchange.Sub(fee).Cmp(l.Cash) != 0 {
		t.Fatalf("cash %s, exchange %s: only the paper fee may differ", l.Cash, onExchange)
	}
	r.open()
	if r.eng.st.Halted != "" {
		t.Fatalf("the paper fee must not look like a missing balance: %q", r.eng.st.Halted)
	}
}

func TestAccountIsValuedWhereOrdersFill(t *testing.T) {
	r := newRig(t, rate("hold", "hold"))
	r.ex.SetVenuePremium(1.01)
	r.tick()
	live := r.ex.Price("BTCUSDT", r.now())
	if got := r.eng.prices["BTCUSDT"].Float(); got < live*1.009 || got > live*1.011 {
		t.Fatalf("valuation must use the venue's price: %v vs live %v", got, live)
	}
	if got := r.eng.st.Last().Prices["BTCUSDT"].Float(); got != live {
		t.Fatalf("the model and the scorecard use the live market: %v vs %v", got, live)
	}
}

func auditTypes(t *testing.T, c config.Config) ([]string, []audit.Event) {
	t.Helper()
	events, err := audit.ReadAfter(EventsDir(c), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types, events
}

func TestEveryStepIsOnRecordBeforeItHappens(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	r.tick()
	types, events := auditTypes(t, r.eng.Config)
	// Each message to the user is a notice, recorded after the step it tells
	// of: the decision, then the trade.
	want := []string{evAccountCreated, evAgentStarted, evEquity, evDecisionStarted, evModelCall, evDecisionDone, evNotice, evOrderIntent, evOrderResult, evTradeBooked, evNotice}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("events:\n got %v\nwant %v", types, want)
	}
	var intent orderIntent
	json.Unmarshal(events[7].Body, &intent)
	if o := r.ex.OrderList()[0]; intent.ClientID != o.ClientID || intent.Side != "BUY" {
		t.Fatalf("the intent must name the order that was sent: %+v vs %+v", intent, o)
	}
	var started decisionStarted
	json.Unmarshal(events[3].Body, &started)
	if started.Briefing.DecisionID != "d1" || len(started.Briefing.Coins) != 2 || strings.Contains(string(events[3].Body), r.eng.Config.Model) {
		t.Fatalf("the briefing is recorded, the model only by fingerprint: %s", events[3].Body[:200])
	}
	if _, err := audit.Verify(EventsDir(r.eng.Config)); err != nil {
		t.Fatal(err)
	}
	// After a restart the chain continues.
	r.eng = newEngine(t, r.eng.Config, r)
	r.open()
	r.command("status")
	if last, err := audit.Verify(EventsDir(r.eng.Config)); err != nil || last.Type != evNotice {
		t.Fatalf("chain after restart, ending with the reply: %v %+v", err, last)
	}
}

func TestNothingHappensThatCannotBeRecorded(t *testing.T) {
	r := newRig(t, rate("strong_buy", "hold"))
	dir := EventsDir(r.eng.Config)
	r.eng.events.Close()
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	r.tick()
	if r.api.Count() != 0 || len(r.ex.OrderList()) != 0 || !r.sent("Decision skipped") {
		t.Fatalf("no model call or order may happen off the record: calls %d, orders %v", r.api.Count(), r.ex.OrderList())
	}
}
