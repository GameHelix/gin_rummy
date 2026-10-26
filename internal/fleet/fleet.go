// Package fleet runs many agents in one engine. Each tenant brings their own
// keys, sealed in the vault; each agent runs a versioned config within the
// limits of its tenant's tier. The engine API and the command line call it.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/config"
	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/vault"
)

// Refused is an error the user can fix, such as a config the plan does not
// allow or a key that does not work. Its message is safe to show them.
type Refused struct{ Err error }

func (r *Refused) Error() string { return r.Err.Error() }
func (r *Refused) Unwrap() error { return r.Err }

func refuse(err error) error {
	if err == nil {
		return nil
	}
	return &Refused{err}
}

func refusef(format string, args ...any) error { return &Refused{fmt.Errorf(format, args...)} }

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

var symbolPattern = regexp.MustCompile(`^[A-Z0-9]{2,20}$`)

// Validate checks a plan before it is stored.
func (en Entitlements) Validate() error {
	for _, s := range en.Symbols {
		if !symbolPattern.MatchString(s) {
			return fmt.Errorf("%q is not a Binance symbol", s)
		}
	}
	if en.MaxAgents < 0 || en.MinDecisionIntervalHours < 0 || en.MaxEventDecisionsPerDay < 0 {
		return errors.New("plan limits cannot be negative")
	}
	return nil
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
)

// Keys are an agent's decrypted secrets, held in memory only while it runs.
type Keys struct {
	Anthropic     string
	BinanceKey    string
	BinanceSecret string
}

type secret struct {
	Key    string `json:"key,omitempty"`
	Secret string `json:"secret,omitempty"`
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
	switch kind {
	case KindAnthropic:
		if s.Key == "" || s.Secret != "" {
			return s, "", errors.New(`an Anthropic key is {"key": "..."}`)
		}
	case KindBinance:
		if s.Key == "" || s.Secret == "" {
			return s, "", errors.New(`a Binance key is {"key": "...", "secret": "..."}`)
		}
	default:
		return s, "", fmt.Errorf("unknown key kind %q", kind)
	}
	return s, "…" + s.Key[max(0, len(s.Key)-4):], nil
}

// Service manages tenants, keys and agents. Check, when set, tries an
// agent's keys against Binance and the model before it starts, in the
// agent's own mode; CheckKey tries each key as it is added.
type Service struct {
	Store    *pg.Store
	Vault    *vault.Vault
	DataDir  string
	Model    string
	Check    func(ctx context.Context, spec Spec) error
	CheckKey func(ctx context.Context, kind, mode string, k Keys) error
}

// AddTenant creates a tenant, under the given ID or a new one. An ID that
// is taken returns that tenant unchanged, so a retried request is safe.
func (s *Service) AddTenant(ctx context.Context, id uuid.UUID, name string, en Entitlements) (pg.Tenant, bool, error) {
	if strings.TrimSpace(name) == "" {
		return pg.Tenant{}, false, refusef("a tenant needs a name")
	}
	if err := en.Validate(); err != nil {
		return pg.Tenant{}, false, refuse(err)
	}
	raw, err := json.Marshal(en)
	if err != nil {
		return pg.Tenant{}, false, err
	}
	if id == uuid.Nil {
		id = uuid.Must(uuid.NewV7())
	}
	err = s.Store.CreateTenant(ctx, pg.Tenant{ID: id, Name: name, Entitlements: raw})
	if err != nil && !errors.Is(err, pg.ErrExists) {
		return pg.Tenant{}, false, err
	}
	t, terr := s.Store.Tenant(ctx, id)
	return t, err == nil, terr
}

// SetEntitlements changes a tenant's plan. Active agents the new plan does
// not allow stop; when more are active than it allows, the newest stop. It
// returns why each agent stopped.
func (s *Service) SetEntitlements(ctx context.Context, tenant uuid.UUID, en Entitlements) (map[uuid.UUID]string, error) {
	if err := en.Validate(); err != nil {
		return nil, refuse(err)
	}
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
	return refusef("agent limit reached (%d on this plan)", en.MaxAgents)
}

// Plan is a tenant's current entitlements.
func (s *Service) Plan(ctx context.Context, tenant uuid.UUID) (Entitlements, error) {
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
	if kind == KindAnthropic {
		return Keys{Anthropic: s.Key}
	}
	return Keys{BinanceKey: s.Key, BinanceSecret: s.Secret}
}

// NewKey is a key to store. ID is optional: a retry with the same ID
// returns the key stored the first time. Mode says where an exchange key is
// tried: testnet or live.
type NewKey struct {
	ID     uuid.UUID
	Kind   string
	Label  string
	Mode   string
	Secret []byte
}

// AddKey checks a key's shape and, with CheckKey set, that it works, then
// seals and stores it. The secret is never logged or returned; only a hint
// of its last characters is.
func (s *Service) AddKey(ctx context.Context, tenant uuid.UUID, k NewKey) (pg.Credential, bool, error) {
	if k.ID == uuid.Nil {
		k.ID = uuid.Must(uuid.NewV7())
	} else if c, err := s.Store.Credential(ctx, tenant, k.ID); !errors.Is(err, pg.ErrNotFound) {
		return c, false, err
	}
	sec, hint, err := parseSecret(k.Kind, k.Secret)
	if err != nil {
		return pg.Credential{}, false, refuse(err)
	}
	if k.Kind == KindBinance && k.Mode != config.Testnet && k.Mode != config.Live {
		return pg.Credential{}, false, refusef("say whether the Binance key is for testnet or live")
	}
	if s.CheckKey != nil {
		if err := s.CheckKey(ctx, k.Kind, k.Mode, keysOf(k.Kind, sec)); err != nil {
			return pg.Credential{}, false, refuse(err)
		}
	}
	c := pg.Credential{ID: k.ID, Tenant: tenant, Kind: k.Kind, Label: k.Label, Hint: hint}
	if c.Sealed, err = s.Vault.Seal(owner(tenant, c.ID, k.Kind), k.Secret); err != nil {
		return pg.Credential{}, false, err
	}
	err = s.Store.PutCredential(ctx, c)
	if err != nil && !errors.Is(err, pg.ErrExists) {
		return pg.Credential{}, false, err
	}
	// After a lost race with a retry, the key stored first is returned.
	stored, serr := s.Store.Credential(ctx, tenant, k.ID)
	if errors.Is(serr, pg.ErrNotFound) {
		serr = pg.ErrExists
	}
	return stored, err == nil, serr
}

// RevokeKey revokes a stored key. Agents that used it stop, and are
// returned. Revoking a revoked key does nothing.
func (s *Service) RevokeKey(ctx context.Context, tenant, key uuid.UUID) ([]uuid.UUID, error) {
	c, err := s.Store.Credential(ctx, tenant, key)
	if err != nil || c.Revoked {
		return nil, err
	}
	users, err := s.Store.RevokeCredential(ctx, tenant, key)
	if err != nil {
		return nil, err
	}
	for _, a := range users {
		if err := s.Stop(ctx, tenant, a); err != nil {
			return nil, err
		}
	}
	return users, nil
}

// usable returns a stored key that may be attached to an agent.
func (s *Service) usable(ctx context.Context, tenant, key uuid.UUID) (pg.Credential, error) {
	c, err := s.Store.Credential(ctx, tenant, key)
	switch {
	case errors.Is(err, pg.ErrNotFound):
		return c, refusef("key %s does not exist", key)
	case err != nil:
		return c, err
	case c.Revoked:
		return c, refusef("key %s was revoked", key)
	}
	return c, nil
}

// AttachKey makes an agent use a stored key for that key's kind.
func (s *Service) AttachKey(ctx context.Context, tenant, agent, credential uuid.UUID) error {
	c, err := s.usable(ctx, tenant, credential)
	if err != nil {
		return err
	}
	if _, err := s.Store.Agent(ctx, tenant, agent); err != nil {
		return err
	}
	return inUse(s.Store.Attach(ctx, tenant, agent, credential, c.Kind))
}

func inUse(err error) error {
	if errors.Is(err, pg.ErrKeyInUse) {
		return refuse(err)
	}
	return err
}

// NewAgent is an agent to create, stopped, with its first config and its
// keys. ID is optional: a retry with the same ID returns the agent created
// the first time.
type NewAgent struct {
	ID     uuid.UUID
	Name   string
	Mode   string
	Config []byte
	Keys   []uuid.UUID
	By     string
}

// AddAgent creates a stopped agent with its first config and keys.
func (s *Service) AddAgent(ctx context.Context, tenant uuid.UUID, a NewAgent) (pg.AgentRow, bool, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.Must(uuid.NewV7())
	} else if row, err := s.Store.Agent(ctx, tenant, a.ID); !errors.Is(err, pg.ErrNotFound) {
		return row, false, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return pg.AgentRow{}, false, refusef("an agent needs a name")
	}
	if a.Mode != config.Testnet && a.Mode != config.Live {
		return pg.AgentRow{}, false, refusef("mode must be testnet or live")
	}
	en, err := s.Plan(ctx, tenant)
	if err != nil {
		return pg.AgentRow{}, false, err
	}
	agents, err := s.Store.Agents(ctx, tenant)
	if err != nil {
		return pg.AgentRow{}, false, err
	}
	if len(agents) >= en.MaxAgents {
		return pg.AgentRow{}, false, tooMany(en)
	}
	row := pg.AgentRow{ID: a.ID, Tenant: tenant, Name: a.Name, Mode: a.Mode}
	if _, err := s.config(row, en, a.Config); err != nil {
		return pg.AgentRow{}, false, refuse(err)
	}
	keys := map[string]uuid.UUID{}
	for _, id := range a.Keys {
		c, err := s.usable(ctx, tenant, id)
		if err != nil {
			return pg.AgentRow{}, false, err
		}
		if _, dup := keys[c.Kind]; dup {
			return pg.AgentRow{}, false, refusef("an agent uses one %s key", c.Kind)
		}
		keys[c.Kind] = id
	}
	err = s.Store.CreateAgent(ctx, row, a.Config, a.By, keys)
	if errors.Is(err, pg.ErrExists) {
		// A retry with the same ID may have won the race.
		if row, err := s.Store.Agent(ctx, tenant, a.ID); err == nil {
			return row, false, nil
		}
		return pg.AgentRow{}, false, refusef("an agent named %q already exists", a.Name)
	}
	if err != nil {
		return pg.AgentRow{}, false, inUse(err)
	}
	row, err = s.Store.Agent(ctx, tenant, a.ID)
	return row, err == nil, err
}

// SetConfig stores a new config version for an agent. A running agent
// restarts on it at the supervisor's next pass.
func (s *Service) SetConfig(ctx context.Context, tenant, agent uuid.UUID, raw []byte, by string) (int, error) {
	row, err := s.Store.Agent(ctx, tenant, agent)
	if err != nil {
		return 0, err
	}
	en, err := s.Plan(ctx, tenant)
	if err != nil {
		return 0, err
	}
	if _, err := s.config(row, en, raw); err != nil {
		return 0, refuse(err)
	}
	return s.Store.AddVersion(ctx, tenant, agent, raw, by)
}

// config turns a stored config into the one an agent runs: its own data
// folder, its mode, and the engine's default model if it names none.
func (s *Service) config(row pg.AgentRow, en Entitlements, raw []byte) (config.Config, error) {
	c, err := s.settings(row, raw)
	if err != nil {
		return c, err
	}
	return c, en.Check(c)
}

func (s *Service) settings(row pg.AgentRow, raw []byte) (config.Config, error) {
	c, err := config.Parse(raw, "model", "data_dir")
	if err != nil {
		return c, err
	}
	c.Mode, c.DataDir = row.Mode, filepath.Join(s.DataDir, "agents", row.ID.String())
	if strings.TrimSpace(c.Model) == "" {
		c.Model = s.Model
	}
	return c, c.Validate()
}

func (s *Service) Start(ctx context.Context, tenant, agent uuid.UUID) error {
	spec, err := s.Spec(ctx, tenant, agent)
	if err != nil {
		return err
	}
	if spec.Keys.Anthropic == "" || spec.Keys.BinanceKey == "" {
		return refusef("attach an Anthropic key and a Binance key first")
	}
	en, err := s.Plan(ctx, tenant)
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
			return refuse(err)
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
		if err := s.Store.CreateAgent(ctx, pg.AgentRow{ID: agent, Tenant: tenant, Name: name, Mode: mode}, nil, "", nil); err != nil {
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
	spec, row, err := s.load(ctx, tenant, agent)
	if err != nil {
		return Spec{}, err
	}
	en, err := s.Plan(ctx, tenant)
	if err != nil {
		return Spec{}, err
	}
	if err := en.Check(spec.Config); err != nil {
		return Spec{}, refuse(err)
	}
	attached, err := s.Store.Attached(ctx, tenant, row.ID)
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
		if kind == KindAnthropic {
			spec.Keys.Anthropic = k.Anthropic
		} else {
			spec.Keys.BinanceKey, spec.Keys.BinanceSecret = k.BinanceKey, k.BinanceSecret
		}
	}
	return spec, nil
}

// Settings is an agent's current config without its keys, for reading its
// account. Unlike Spec it does not check the plan, so an agent the plan no
// longer allows can still be looked at.
func (s *Service) Settings(ctx context.Context, tenant, agent uuid.UUID) (Spec, error) {
	spec, _, err := s.load(ctx, tenant, agent)
	return spec, err
}

func (s *Service) load(ctx context.Context, tenant, agent uuid.UUID) (Spec, pg.AgentRow, error) {
	row, err := s.Store.Agent(ctx, tenant, agent)
	if err != nil {
		return Spec{}, row, err
	}
	if row.Version == 0 {
		return Spec{}, row, refusef("the agent has no config yet")
	}
	raw, err := s.Store.Version(ctx, tenant, agent, row.Version)
	if err != nil {
		return Spec{}, row, err
	}
	c, err := s.settings(row, raw)
	if err != nil {
		return Spec{}, row, refuse(err)
	}
	return Spec{Tenant: tenant, Agent: agent, Name: row.Name, Version: row.Version, Config: c}, row, nil
}
