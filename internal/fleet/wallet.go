package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/hyperliquid"
	"github.com/candylario/trader/internal/pg"
)

// KindHyperliquid is an account's agent wallet on Hyperliquid. The engine
// makes it, its key stays sealed in the vault, and the account's own
// wallet approves it from the browser. An agent wallet can trade for the
// account but never withdraw.
const KindHyperliquid = "hyperliquid"

// Modes of a wallet: the exchange it trades on.
const (
	ModeTestnet = "testnet"
	ModeMainnet = "mainnet"
)

// walletSecret is what the vault seals for a wallet. User, Mode and Fee
// are set once the account's approvals are confirmed.
type walletSecret struct {
	Key  string `json:"key"`
	User string `json:"user,omitempty"`
	Mode string `json:"mode,omitempty"`
	Fee  int    `json:"fee,omitempty"`
}

// Wallet is an account's agent wallet and the state of its approvals.
type Wallet struct {
	ID         uuid.UUID
	Agent      string // the agent wallet's address
	User       string // the account it trades for, once approved
	Mode       string
	BuilderFee int // tenths of a basis point the account approved for the builder
}

// Approved says whether the account has approved the agent.
func (w Wallet) Approved() bool { return w.User != "" }

// Approvals is what the account's wallet signs: the agent approval and,
// when the engine has a builder, the builder fee.
type Approvals struct {
	Agent             string
	User              string
	Mode              string
	ApproveAgent      hyperliquid.UserAction
	ApproveBuilderFee *hyperliquid.UserAction
}

// Signed is an account action with the signature the account's wallet gave.
type Signed struct {
	Action    json.RawMessage
	Nonce     uint64
	Signature hyperliquid.Signature
}

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

func walletArgs(user, mode string) (string, string, error) {
	user, mode = strings.ToLower(strings.TrimSpace(user)), strings.ToLower(strings.TrimSpace(mode))
	if !addressPattern.MatchString(user) {
		return "", "", refusef("user must be an address: 0x and 40 hex digits")
	}
	if mode != ModeTestnet && mode != ModeMainnet {
		return "", "", refusef("mode must be testnet or mainnet")
	}
	return user, mode, nil
}

func (s *Service) exchange(mode string) (*hyperliquid.Client, error) {
	if s.Exchange == nil {
		return nil, errors.New("the engine has no exchange")
	}
	return s.Exchange(mode), nil
}

// wallet finds the account's wallet, if it has one, and opens its secret.
func (s *Service) wallet(ctx context.Context, tenant uuid.UUID) (pg.Credential, walletSecret, bool, error) {
	creds, err := s.Store.Credentials(ctx, tenant)
	if err != nil {
		return pg.Credential{}, walletSecret{}, false, err
	}
	for _, c := range creds {
		if c.Kind != KindHyperliquid || c.Revoked {
			continue
		}
		plain, err := s.Vault.Open(owner(tenant, c.ID, KindHyperliquid), c.Sealed)
		if err != nil {
			return pg.Credential{}, walletSecret{}, false, err
		}
		var sec walletSecret
		err = json.Unmarshal(plain, &sec)
		clear(plain)
		if err != nil {
			return pg.Credential{}, walletSecret{}, false, fmt.Errorf("wallet %s: %w", c.ID, err)
		}
		return c, sec, true, nil
	}
	return pg.Credential{}, walletSecret{}, false, nil
}

func walletOf(c pg.Credential, sec walletSecret) Wallet {
	return Wallet{ID: c.ID, Agent: c.Hint, User: sec.User, Mode: sec.Mode, BuilderFee: sec.Fee}
}

// Wallet returns the account's agent wallet, making one the first time.
// The second result says whether it was made now.
func (s *Service) Wallet(ctx context.Context, tenant uuid.UUID) (Wallet, bool, error) {
	if err := s.open(ctx, tenant); err != nil {
		return Wallet{}, false, err
	}
	c, sec, ok, err := s.wallet(ctx, tenant)
	if err != nil || ok {
		return walletOf(c, sec), false, err
	}
	signer, key, err := hyperliquid.GenerateSigner()
	if err != nil {
		return Wallet{}, false, err
	}
	sec = walletSecret{Key: key}
	raw, err := json.Marshal(sec)
	if err != nil {
		return Wallet{}, false, err
	}
	c = pg.Credential{ID: uuid.Must(uuid.NewV7()), Tenant: tenant, Kind: KindHyperliquid, Label: "wallet", Hint: signer.Address()}
	if c.Sealed, err = s.Vault.Seal(owner(tenant, c.ID, KindHyperliquid), raw); err != nil {
		return Wallet{}, false, err
	}
	if err := s.Store.PutCredential(ctx, c); err != nil {
		return Wallet{}, false, err
	}
	return walletOf(c, sec), true, nil
}

// feePercent writes a fee in tenths of a basis point as the exchange's
// percent string: 50 is "0.05%".
func feePercent(tenths int) string {
	return strconv.FormatFloat(float64(tenths)/1000, 'f', -1, 64) + "%"
}

// WalletApprovals prepares the approvals for user's wallet to sign on the
// given exchange. The wallet is made first if the account has none.
func (s *Service) WalletApprovals(ctx context.Context, tenant uuid.UUID, user, mode string) (Approvals, error) {
	user, mode, err := walletArgs(user, mode)
	if err != nil {
		return Approvals{}, err
	}
	w, _, err := s.Wallet(ctx, tenant)
	if err != nil {
		return Approvals{}, err
	}
	ex, err := s.exchange(mode)
	if err != nil {
		return Approvals{}, err
	}
	a := Approvals{Agent: w.Agent, User: user, Mode: mode, ApproveAgent: ex.ApproveAgent(w.Agent, "candylario")}
	if s.Builder.Address != "" && s.Builder.Fee > 0 {
		b := ex.ApproveBuilderFee(s.Builder.Address, feePercent(s.Builder.Fee))
		a.ApproveBuilderFee = &b
	}
	return a, nil
}

// checkAction makes sure a signed action is the one the engine asked for,
// so the engine relays nothing else to the exchange.
func checkAction(raw json.RawMessage, typ, mode, field, want string) error {
	var a map[string]any
	if err := json.Unmarshal(raw, &a); err != nil {
		return refusef("%s: the action must be the JSON the engine gave", typ)
	}
	chain := "Testnet"
	if mode == ModeMainnet {
		chain = "Mainnet"
	}
	if a["type"] != typ || a["hyperliquidChain"] != chain {
		return refusef("%s: not the action the engine gave", typ)
	}
	if got, _ := a[field].(string); !strings.EqualFold(got, want) {
		return refusef("%s: %s is %q, not %s", typ, field, got, want)
	}
	return nil
}

// ApproveWallet submits the approvals user's wallet signed and confirms
// them with the exchange before recording the account.
func (s *Service) ApproveWallet(ctx context.Context, tenant uuid.UUID, user, mode string, agent Signed, builder *Signed) (Wallet, error) {
	user, mode, err := walletArgs(user, mode)
	if err != nil {
		return Wallet{}, err
	}
	if err := s.open(ctx, tenant); err != nil {
		return Wallet{}, err
	}
	c, sec, ok, err := s.wallet(ctx, tenant)
	if err != nil {
		return Wallet{}, err
	}
	if !ok {
		return Wallet{}, refusef("the account has no wallet yet")
	}
	w := walletOf(c, sec)
	ex, err := s.exchange(mode)
	if err != nil {
		return Wallet{}, err
	}
	if err := checkAction(agent.Action, "approveAgent", mode, "agentAddress", w.Agent); err != nil {
		return Wallet{}, err
	}
	if err := ex.SubmitRaw(ctx, agent.Action, agent.Nonce, agent.Signature); err != nil {
		return Wallet{}, refusef("the exchange refused the agent approval: %v", err)
	}
	if builder != nil {
		if err := checkAction(builder.Action, "approveBuilderFee", mode, "builder", s.Builder.Address); err != nil {
			return Wallet{}, err
		}
		if err := ex.SubmitRaw(ctx, builder.Action, builder.Nonce, builder.Signature); err != nil {
			return Wallet{}, refusef("the exchange refused the builder fee approval: %v", err)
		}
	}
	role, err := ex.Role(ctx, w.Agent)
	if err != nil {
		return Wallet{}, err
	}
	if role.Role != "agent" || !strings.EqualFold(role.User, user) {
		return Wallet{}, refusef("the exchange does not show %s as an agent of %s", w.Agent, user)
	}
	fee := 0
	if s.Builder.Address != "" && s.Builder.Fee > 0 {
		if fee, err = ex.MaxBuilderFee(ctx, user, s.Builder.Address); err != nil {
			return Wallet{}, err
		}
		if fee < s.Builder.Fee {
			return Wallet{}, refusef("the account approved a builder fee of %s, below the %s needed", feePercent(fee), feePercent(s.Builder.Fee))
		}
	}
	sec.User, sec.Mode, sec.Fee = user, mode, fee
	raw, err := json.Marshal(sec)
	if err != nil {
		return Wallet{}, err
	}
	sealed, err := s.Vault.Seal(owner(tenant, c.ID, KindHyperliquid), raw)
	if err != nil {
		return Wallet{}, err
	}
	if err := s.Store.ResealCredential(ctx, tenant, c.ID, sealed); err != nil {
		return Wallet{}, err
	}
	return walletOf(c, sec), nil
}

// WalletAccount returns the wallet and, once it is approved, the account
// as the exchange sees it.
func (s *Service) WalletAccount(ctx context.Context, tenant uuid.UUID) (Wallet, *hyperliquid.Account, error) {
	c, sec, ok, err := s.wallet(ctx, tenant)
	if err != nil {
		return Wallet{}, nil, err
	}
	if !ok {
		return Wallet{}, nil, pg.ErrNotFound
	}
	w := walletOf(c, sec)
	if !w.Approved() {
		return w, nil, nil
	}
	ex, err := s.exchange(w.Mode)
	if err != nil {
		return w, nil, err
	}
	acct, err := ex.Account(ctx, w.User)
	if err != nil {
		return w, nil, err
	}
	return w, &acct, nil
}

// WalletSigner opens the wallet's key for trading. The caller clears
// nothing; the key lives only in the signer.
func (s *Service) WalletSigner(ctx context.Context, tenant uuid.UUID) (Wallet, *hyperliquid.Signer, error) {
	c, sec, ok, err := s.wallet(ctx, tenant)
	if err != nil {
		return Wallet{}, nil, err
	}
	if !ok {
		return Wallet{}, nil, pg.ErrNotFound
	}
	signer, err := hyperliquid.NewSigner(sec.Key)
	if err != nil {
		return Wallet{}, nil, err
	}
	return walletOf(c, sec), signer, nil
}
