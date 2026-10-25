// Package fleet runs many agents in one engine. Each tenant brings their own
// keys, sealed in the vault; each agent runs a versioned config within the
// limits of its tenant's tier. The engine API will call this package; until
// then the command line does.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/pg"
	"github.com/ismatsamadov/trader/internal/vault"
)

// Entitlements are what a tenant's tier allows. The app sets them; the
// engine enforces them on every config and every start.
type Entitlements struct {
	Symbols                  []string `json:"symbols"`
	MaxAgents                int      `json:"max_agents"`
	MinDecisionIntervalHours float64  `json:"min_decision_interval_hours"`
	MaxEventDecisionsPerDay  int      `json:"max_event_decisions_per_day"`
	Live                     bool     `json:"live"`
	Models                   []string `json:"models,omitempty"`
}

func (en Entitlements) Check(c config.Config) error {
	for _, s := range c.Symbols {
		if !slices.Contains(en.Symbols, s) {
			return fmt.Errorf("%s is not included in this plan", s)
		}
	}
	switch {
	case c.DecisionIntervalHours < en.MinDecisionIntervalHours:
		return fmt.Errorf("this plan decides at most every %g hours", en.MinDecisionIntervalHours)
	case c.MaxEventDecisionsPerDay > en.MaxEventDecisionsPerDay:
		return fmt.Errorf("this plan allows at most %d extra decisions a day", en.MaxEventDecisionsPerDay)
	case c.Mode == config.Live && !en.Live:
		return errors.New("this plan does not include live trading")
	case len(en.Models) > 0 && !slices.Contains(en.Models, c.Model):
		return errors.New("this plan does not include that model")
	}
	return nil
}

// Kinds of keys a tenant can store.
const (
	KindAnthropic = "anthropic"
	KindBinance   = "binance"
	KindTelegram  = "telegram"
)

// Keys are an agent's decrypted secrets, held in memory only while it runs.
type Keys struct {
	Anthropic     string
	BinanceKey    string
	BinanceSecret string
	TelegramToken string
	TelegramChat  int64
}

type secret struct {
	Key    string `json:"key,omitempty"`
	Secret string `json:"secret,omitempty"`
	Token  string `json:"token,omitempty"`
	ChatID int64  `json:"chat_id,omitempty"`
}

// parseSecret checks a key has exactly the fields its kind needs and returns
// a hint safe to show: its last four characters.
func parseSecret(kind string, raw []byte) (secret, string, error) {
	var s secret
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, "", errors.New("the key must be JSON with the fields for its kind")
	}
	visible := s.Key
	switch kind {
	case KindAnthropic:
		if s.Key == "" || s.Secret != "" || s.Token != "" {
			return s, "", errors.New(`an Anthropic key is {"key": "..."}`)
		}
	case KindBinance:
		if s.Key == "" || s.Secret == "" || s.Token != "" {
			return s, "", errors.New(`a Binance key is {"key": "...", "secret": "..."}`)
		}
	case KindTelegram:
		if s.Token == "" || s.ChatID == 0 || s.Key != "" {
			return s, "", errors.New(`a Telegram bot is {"token": "...", "chat_id": 123}`)
		}
		visible = s.Token
	default:
		return s, "", fmt.Errorf("unknown key kind %q", kind)
	}
	return s, "…" + visible[max(0, len(visible)-4):], nil
}

// Service manages tenants, keys and agents. Check, when set, tries an
// agent's keys against Binance and the model before it starts, in the
// agent's own mode.
type Service struct {
	Store   *pg.Store
	Vault   *vault.Vault
	DataDir string
	Model   string
	Check   func(ctx context.Context, spec Spec) error
}

func (s *Service) AddTenant(ctx context.Context, name string, en Entitlements) (uuid.UUID, error) {
	raw, err := json.Marshal(en)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.Must(uuid.NewV7())
	return id, s.Store.CreateTenant(ctx, pg.Tenant{ID: id, Name: name, Entitlements: raw})
}

// SetEntitlements changes a tenant's plan. Active agents the new plan does
// not allow stop; when more are active than it allows, the newest stop. It
// returns why each agent stopped.
func (s *Service) SetEntitlements(ctx context.Context, tenant uuid.UUID, en Entitlements) (map[uuid.UUID]string, error) {
	raw, err := json.Marshal(en)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetEntitlements(ctx, tenant, raw); err != nil {
		return nil, err
	}
	agents, err := s.Store.Agents(ctx, tenant)
	if err != nil {
		return nil, err
	}
	// Version 7 IDs sort by creation time.
	slices.SortFunc(agents, func(a, b pg.AgentRow) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	stopped, active := map[uuid.UUID]string{}, 0
	for _, a := range agents {
		if a.Status != pg.StatusActive {
			continue
		}
		raw, err := s.Store.Version(ctx, tenant, a.ID, a.Version)
		if err != nil {
			return stopped, err
		}
		_, reason := s.config(a, en, raw)
		if reason == nil && active == en.MaxAgents {
			reason = tooMany(en)
		}
		if reason == nil {
			active++
			continue
		}
		if err := s.Stop(ctx, tenant, a.ID); err != nil {
			return stopped, err
		}
		stopped[a.ID] = reason.Error()
	}
	return stopped, nil
}

func tooMany(en Entitlements) error {
	return fmt.Errorf("agent limit reached (%d on this plan)", en.MaxAgents)
}

func (s *Service) entitlements(ctx context.Context, tenant uuid.UUID) (Entitlements, error) {
	t, err := s.Store.Tenant(ctx, tenant)
	if err != nil {
		return Entitlements{}, err
	}
	var en Entitlements
	err = json.Unmarshal(t.Entitlements, &en)
	return en, err
}

func owner(tenant, credential uuid.UUID, kind string) []byte {
	return []byte(tenant.String() + "/" + credential.String() + "/" + kind)
}

func keysOf(kind string, s secret) Keys {
	switch kind {
	case KindAnthropic:
		return Keys{Anthropic: s.Key}
	case KindBinance:
		return Keys{BinanceKey: s.Key, BinanceSecret: s.Secret}
	}
	return Keys{TelegramToken: s.Token, TelegramChat: s.ChatID}
}

// AddKey checks a key, seals it and stores it. The secret is never logged
// or returned; only its hint is.
func (s *Service) AddKey(ctx context.Context, tenant uuid.UUID, kind, label string, raw []byte) (pg.Credential, error) {
	_, hint, err := parseSecret(kind, raw)
	if err != nil {
		return pg.Credential{}, err
	}
	c := pg.Credential{ID: uuid.Must(uuid.NewV7()), Tenant: tenant, Kind: kind, Label: label, Hint: hint}
	if c.Sealed, err = s.Vault.Seal(owner(tenant, c.ID, kind), raw); err != nil {
		return pg.Credential{}, err
	}
	return c, s.Store.PutCredential(ctx, c)
}

// RevokeKey revokes a stored key. Agents that used it restart without it, or
// stop if it was their Anthropic or Binance key; it returns those that
// stopped.
func (s *Service) RevokeKey(ctx context.Context, tenant, key uuid.UUID) ([]uuid.UUID, error) {
	c, err := s.Store.Credential(ctx, tenant, key)
	if err != nil {
		return nil, err
	}
	users, err := s.Store.RevokeCredential(ctx, tenant, key)
	if err != nil || c.Kind == KindTelegram {
		return nil, err
	}
	for _, a := range users {
		if err := s.Stop(ctx, tenant, a); err != nil {
			return nil, err
		}
	}
	return users, nil
}

// AttachKey makes an agent use a stored key for that key's kind.
func (s *Service) AttachKey(ctx context.Context, tenant, agent, credential uuid.UUID) error {
	c, err := s.Store.Credential(ctx, tenant, credential)
	if err != nil {
		return err
	}
	if c.Revoked {
		return errors.New("that key was revoked")
	}
	return s.Store.Attach(ctx, tenant, agent, credential, c.Kind)
}

// AddAgent creates a stopped agent with its first config.
func (s *Service) AddAgent(ctx context.Context, tenant uuid.UUID, name, mode string, raw []byte, by string) (uuid.UUID, error) {
	en, err := s.entitlements(ctx, tenant)
	if err != nil {
		return uuid.Nil, err
	}
	agents, err := s.Store.Agents(ctx, tenant)
	if err != nil {
		return uuid.Nil, err
	}
	if len(agents) >= en.MaxAgents {
		return uuid.Nil, tooMany(en)
	}
	row := pg.AgentRow{ID: uuid.Must(uuid.NewV7()), Tenant: tenant, Name: name, Mode: mode}
	if _, err := s.config(row, en, raw); err != nil {
		return uuid.Nil, err
	}
	if err := s.Store.CreateAgent(ctx, row); err != nil {
		return uuid.Nil, err
	}
	_, err = s.Store.AddVersion(ctx, tenant, row.ID, raw, by)
	return row.ID, err
}

// SetConfig stores a new config version for an agent. A running agent
// restarts on it at the supervisor's next pass.
func (s *Service) SetConfig(ctx context.Context, tenant, agent uuid.UUID, raw []byte, by string) (int, error) {
	row, err := s.Store.Agent(ctx, tenant, agent)
	if err != nil {
		return 0, err
	}
	en, err := s.entitlements(ctx, tenant)
	if err != nil {
		return 0, err
	}
	if _, err := s.config(row, en, raw); err != nil {
		return 0, err
	}
	return s.Store.AddVersion(ctx, tenant, agent, raw, by)
}

// config turns a stored config into the one an agent runs: its own data
// folder, its mode, and the engine's default model if it names none.
func (s *Service) config(row pg.AgentRow, en Entitlements, raw []byte) (config.Config, error) {
	c, err := config.Parse(raw, "model", "data_dir")
	if err != nil {
		return c, err
	}
	c.Mode, c.DataDir = row.Mode, filepath.Join(s.DataDir, "agents", row.ID.String())
	if strings.TrimSpace(c.Model) == "" {
		c.Model = s.Model
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, en.Check(c)
}

func (s *Service) Start(ctx context.Context, tenant, agent uuid.UUID) error {
	spec, err := s.Spec(ctx, tenant, agent)
	if err != nil {
		return err
	}
	if spec.Keys.Anthropic == "" || spec.Keys.BinanceKey == "" {
		return errors.New("attach an Anthropic key and a Binance key first")
	}
	en, err := s.entitlements(ctx, tenant)
	if err != nil {
		return err
	}
	agents, err := s.Store.Agents(ctx, tenant)
	if err != nil {
		return err
	}
	active := 0
	for _, a := range agents {
		if a.Status == pg.StatusActive && a.ID != agent {
			active++
		}
	}
	if active >= en.MaxAgents {
		return tooMany(en)
	}
	if s.Check != nil {
		if err := s.Check(ctx, spec); err != nil {
			return err
		}
	}
	return s.Store.SetStatus(ctx, tenant, agent, pg.StatusActive)
}

// Adopt brings a standalone agent into the fleet under its existing IDs, so
// its audit log continues unbroken. Rows the event shipper already created
// are reused.
func (s *Service) Adopt(ctx context.Context, tenant, agent uuid.UUID, name, mode string, en Entitlements, raw []byte) error {
	entRaw, err := json.Marshal(en)
	if err != nil {
		return err
	}
	if _, err := s.Store.Tenant(ctx, tenant); errors.Is(err, pg.ErrNotFound) {
		err = s.Store.CreateTenant(ctx, pg.Tenant{ID: tenant, Name: name, Entitlements: entRaw})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := s.Store.SetEntitlements(ctx, tenant, entRaw); err != nil {
		return err
	}
	if _, err := s.Store.Agent(ctx, tenant, agent); errors.Is(err, pg.ErrNotFound) {
		if err := s.Store.CreateAgent(ctx, pg.AgentRow{ID: agent, Tenant: tenant, Name: name, Mode: mode}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = s.SetConfig(ctx, tenant, agent, raw, "import")
	return err
}

func (s *Service) Stop(ctx context.Context, tenant, agent uuid.UUID) error {
	return s.Store.SetStatus(ctx, tenant, agent, pg.StatusStopped)
}

// Spec is everything needed to run one agent.
type Spec struct {
	Tenant  uuid.UUID
	Agent   uuid.UUID
	Name    string
	Version int
	Config  config.Config
	Keys    Keys
}

// Spec loads an agent's current config, checked against its tenant's tier
// as it stands now, and decrypts its keys.
func (s *Service) Spec(ctx context.Context, tenant, agent uuid.UUID) (Spec, error) {
	row, err := s.Store.Agent(ctx, tenant, agent)
	if err != nil {
		return Spec{}, err
	}
	if row.Version == 0 {
		return Spec{}, errors.New("the agent has no config yet")
	}
	raw, err := s.Store.Version(ctx, tenant, agent, row.Version)
	if err != nil {
		return Spec{}, err
	}
	en, err := s.entitlements(ctx, tenant)
	if err != nil {
		return Spec{}, err
	}
	c, err := s.config(row, en, raw)
	if err != nil {
		return Spec{}, err
	}
	spec := Spec{Tenant: tenant, Agent: agent, Name: row.Name, Version: row.Version, Config: c}
	attached, err := s.Store.Attached(ctx, tenant, agent)
	if err != nil {
		return Spec{}, err
	}
	for kind, id := range attached {
		cred, err := s.Store.Credential(ctx, tenant, id)
		if err != nil {
			return Spec{}, err
		}
		plain, err := s.Vault.Open(owner(tenant, id, kind), cred.Sealed)
		if err != nil {
			return Spec{}, err
		}
		sec, _, err := parseSecret(kind, plain)
		clear(plain)
		if err != nil {
			return Spec{}, err
		}
		k := keysOf(kind, sec)
		switch kind {
		case KindAnthropic:
			spec.Keys.Anthropic = k.Anthropic
		case KindBinance:
			spec.Keys.BinanceKey, spec.Keys.BinanceSecret = k.BinanceKey, k.BinanceSecret
		case KindTelegram:
			spec.Keys.TelegramToken, spec.Keys.TelegramChat = k.TelegramToken, k.TelegramChat
		}
	}
	return spec, nil
}
