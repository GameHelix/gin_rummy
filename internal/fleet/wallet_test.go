package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/hyperliquid"
	"github.com/candylario/trader/internal/pg"
)

// fakeExchange answers the exchange calls the wallet flow makes.
type fakeExchange struct {
	mu        sync.Mutex
	user      string // the account every agent is reported to act for
	fee       int    // the builder fee the account has approved
	submitted []string
}

func (f *fakeExchange) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/exchange" {
		f.submitted = append(f.submitted, string(body["action"]))
		w.Write([]byte(`{"status":"ok","response":{"type":"default"}}`))
		return
	}
	switch strings.Trim(string(body["type"]), `"`) {
	case "userRole":
		json.NewEncoder(w).Encode(map[string]any{"role": "agent", "data": map[string]string{"user": f.user}})
	case "maxBuilderFee":
		json.NewEncoder(w).Encode(f.fee)
	case "clearinghouseState":
		w.Write([]byte(`{"marginSummary":{"accountValue":"1500.5","totalMarginUsed":"0.0"},"withdrawable":"1200.0","assetPositions":[],"time":1700000000000}`))
	default:
		http.NotFound(w, r)
	}
}

func TestAWalletIsMadeOnceAndApprovedWithTheExchange(t *testing.T) {
	s := service(t)
	fake := &fakeExchange{user: "0x5E9EE1089755c3435139848e47e6635505d5a13a", fee: 50}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s.Exchange = func(mode string) *hyperliquid.Client { return &hyperliquid.Client{BaseURL: srv.URL} }
	s.Builder = hyperliquid.Builder{Address: "0xabcdef0123456789abcdef0123456789abcdef01", Fee: 50}
	ctx := context.Background()
	tenant := uuid.New()
	if err := s.Store.CreateTenant(ctx, pg.Tenant{ID: tenant, Name: "acme", Entitlements: []byte("{}")}); err != nil {
		t.Fatal(err)
	}

	w, isNew, err := s.Wallet(ctx, tenant)
	if err != nil || !isNew || len(w.Agent) != 42 || !strings.HasPrefix(w.Agent, "0x") || w.Approved() {
		t.Fatalf("wallet %+v new=%v: %v", w, isNew, err)
	}
	again, isNew, err := s.Wallet(ctx, tenant)
	if err != nil || isNew || again.ID != w.ID || again.Agent != w.Agent {
		t.Fatalf("second wallet %+v new=%v: %v", again, isNew, err)
	}

	a, err := s.WalletApprovals(ctx, tenant, fake.user, "Testnet")
	if err != nil || a.Agent != w.Agent || a.ApproveBuilderFee == nil {
		t.Fatalf("approvals %+v: %v", a, err)
	}
	raw, _ := json.Marshal(a.ApproveAgent.Action)
	if !strings.Contains(string(raw), `"agentAddress":"`+w.Agent+`"`) || !strings.Contains(string(raw), `"hyperliquidChain":"Testnet"`) || !strings.Contains(string(raw), `"agentName":"candylario"`) {
		t.Fatalf("agent approval %s", raw)
	}
	rawB, _ := json.Marshal(a.ApproveBuilderFee.Action)
	if !strings.Contains(string(rawB), `"maxFeeRate":"0.05%"`) || !strings.Contains(string(rawB), `"builder":"0xabcdef0123456789abcdef0123456789abcdef01"`) {
		t.Fatalf("builder approval %s", rawB)
	}

	// The account's own wallet signs; here a throwaway key plays it.
	account, _ := hyperliquid.NewSigner("0x0123456789012345678901234567890123456789012345678901234567890123")
	sigA, _ := a.ApproveAgent.Sign(account)
	sigB, _ := a.ApproveBuilderFee.Sign(account)
	agent := Signed{Action: raw, Nonce: a.ApproveAgent.Nonce, Signature: sigA}
	builder := &Signed{Action: rawB, Nonce: a.ApproveBuilderFee.Nonce, Signature: sigB}

	// An action for another agent is refused before anything is sent.
	forged := Signed{Action: json.RawMessage(strings.Replace(string(raw), w.Agent, "0x"+strings.Repeat("1", 40), 1)), Nonce: agent.Nonce, Signature: sigA}
	var refused *Refused
	if _, err := s.ApproveWallet(ctx, tenant, fake.user, "testnet", forged, builder); !errors.As(err, &refused) || len(fake.submitted) != 0 {
		t.Fatalf("forged approval: %v, submitted %v", err, fake.submitted)
	}

	approved, err := s.ApproveWallet(ctx, tenant, fake.user, "testnet", agent, builder)
	if err != nil || !approved.Approved() || approved.User != strings.ToLower(fake.user) || approved.Mode != ModeTestnet || approved.BuilderFee != 50 {
		t.Fatalf("approved %+v: %v", approved, err)
	}
	if len(fake.submitted) != 2 || fake.submitted[0] != string(raw) || fake.submitted[1] != string(rawB) {
		t.Fatalf("submitted %v", fake.submitted)
	}

	// Too small a builder fee is refused, and the record stays as it was.
	fake.fee = 10
	if _, err := s.ApproveWallet(ctx, tenant, fake.user, "testnet", agent, builder); !errors.As(err, &refused) || !strings.Contains(err.Error(), "0.01%") {
		t.Fatalf("low fee: %v", err)
	}
	fake.fee = 50

	w2, acct, err := s.WalletAccount(ctx, tenant)
	if err != nil || !w2.Approved() || acct == nil || acct.Value != 1500.5 || acct.Withdrawable != 1200 {
		t.Fatalf("account %+v %+v: %v", w2, acct, err)
	}
	_, signer, err := s.WalletSigner(ctx, tenant)
	if err != nil || signer.Address() != w.Agent {
		t.Fatalf("signer: %v", err)
	}
	if _, _, err := s.AddKey(ctx, tenant, NewKey{Kind: KindHyperliquid, Secret: []byte(`{"key":"x"}`)}); !errors.As(err, &refused) {
		t.Fatalf("AddKey of a wallet: %v", err)
	}
	if _, _, err := s.WalletAccount(ctx, uuid.New()); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("no wallet: %v", err)
	}
}
