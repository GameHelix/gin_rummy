package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client calls the engine API. Keys the app passes through it go straight
// to the engine's vault; the app should not keep them.
type Client struct {
	BaseURL string // for example http://127.0.0.1:8080
	Token   string
	HTTP    *http.Client
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) request(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.request(ctx, method, path, in)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readError(resp *http.Response) error {
	var body struct {
		Error *Error `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if json.Unmarshal(raw, &body) != nil || body.Error == nil {
		return &Error{Status: resp.StatusCode, Code: "internal", Message: http.StatusText(resp.StatusCode)}
	}
	body.Error.Status = resp.StatusCode
	return body.Error
}

func tenantPath(tenant string, rest ...string) string {
	p := "/v1/tenants/" + url.PathEscape(tenant)
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p
}

func (c *Client) CreateTenant(ctx context.Context, t NewTenant) (Tenant, error) {
	var out Tenant
	return out, c.do(ctx, http.MethodPost, "/v1/tenants", t, &out)
}

func (c *Client) Tenant(ctx context.Context, tenant string) (Tenant, error) {
	var out Tenant
	return out, c.do(ctx, http.MethodGet, tenantPath(tenant), nil, &out)
}

// SetPlan replaces a tenant's plan. Agents it no longer allows stop.
func (c *Client) SetPlan(ctx context.Context, tenant string, p Plan) (PlanChange, error) {
	var out PlanChange
	return out, c.do(ctx, http.MethodPut, tenantPath(tenant, "plan"), p, &out)
}

func (c *Client) AddKey(ctx context.Context, tenant string, k NewKey) (Key, error) {
	var out Key
	return out, c.do(ctx, http.MethodPost, tenantPath(tenant, "keys"), k, &out)
}

func (c *Client) Keys(ctx context.Context, tenant string) ([]Key, error) {
	var out KeyList
	return out.Keys, c.do(ctx, http.MethodGet, tenantPath(tenant, "keys"), nil, &out)
}

// RevokeKey revokes a key. Agents that need it stop.
func (c *Client) RevokeKey(ctx context.Context, tenant, key string) (Revoked, error) {
	var out Revoked
	return out, c.do(ctx, http.MethodDelete, tenantPath(tenant, "keys", key), nil, &out)
}

// CloseTenant closes an account for good: its agents stop, its keys are
// erased and its name is gone.
func (c *Client) CloseTenant(ctx context.Context, tenant string) (Closed, error) {
	var out Closed
	return out, c.do(ctx, http.MethodDelete, tenantPath(tenant), nil, &out)
}

func (c *Client) CreateAgent(ctx context.Context, tenant string, a NewAgent) (Agent, error) {
	var out Agent
	return out, c.do(ctx, http.MethodPost, tenantPath(tenant, "agents"), a, &out)
}

func (c *Client) Agents(ctx context.Context, tenant string) ([]Agent, error) {
	var out AgentList
	return out.Agents, c.do(ctx, http.MethodGet, tenantPath(tenant, "agents"), nil, &out)
}

func (c *Client) Agent(ctx context.Context, tenant, agent string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, http.MethodGet, tenantPath(tenant, "agents", agent), nil, &out)
}

// Config returns a version of an agent's config; version 0 is the current
// one.
func (c *Client) Config(ctx context.Context, tenant, agent string, version int) (Config, error) {
	path := tenantPath(tenant, "agents", agent, "config")
	if version > 0 {
		path += "?version=" + strconv.Itoa(version)
	}
	var out Config
	return out, c.do(ctx, http.MethodGet, path, nil, &out)
}

// SetConfig stores a new version of an agent's config. A running agent
// restarts on it.
func (c *Client) SetConfig(ctx context.Context, tenant, agent string, config json.RawMessage) (Config, error) {
	var out Config
	return out, c.do(ctx, http.MethodPut, tenantPath(tenant, "agents", agent, "config"), config, &out)
}

// AttachKey makes the agent use a key in place of its key of the same kind.
func (c *Client) AttachKey(ctx context.Context, tenant, agent, key string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, http.MethodPut, tenantPath(tenant, "agents", agent, "keys", key), nil, &out)
}

// Start tries the agent's keys and marks it active; the engine starts it
// within half a minute.
func (c *Client) Start(ctx context.Context, tenant, agent string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, http.MethodPost, tenantPath(tenant, "agents", agent, "start"), nil, &out)
}

// Stop marks the agent stopped; the engine stops it within half a minute.
// Holdings are kept.
func (c *Client) Stop(ctx context.Context, tenant, agent string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, http.MethodPost, tenantPath(tenant, "agents", agent, "stop"), nil, &out)
}

func (c *Client) Status(ctx context.Context, tenant, agent string) (Status, error) {
	var out Status
	return out, c.do(ctx, http.MethodGet, tenantPath(tenant, "agents", agent, "status"), nil, &out)
}

// Command sends a command to a running agent: Decide, StopTrading,
// ResumeTrading or SellAll.
func (c *Client) Command(ctx context.Context, tenant, agent string, cmd Command) (CommandResult, error) {
	var out CommandResult
	return out, c.do(ctx, http.MethodPost, tenantPath(tenant, "agents", agent, "commands"), cmd, &out)
}

// Events returns up to limit of the agent's events after seq.
func (c *Client) Events(ctx context.Context, tenant, agent string, after int64, limit int) ([]Event, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	var out EventList
	return out.Events, c.do(ctx, http.MethodGet, tenantPath(tenant, "agents", agent, "events")+"?"+q.Encode(), nil, &out)
}

// EventsOfType is Events limited to the given event types.
func (c *Client) EventsOfType(ctx context.Context, tenant, agent string, types []string, after int64, limit int) ([]Event, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	if len(types) > 0 {
		q.Set("type", strings.Join(types, ","))
	}
	var out EventList
	return out.Events, c.do(ctx, http.MethodGet, tenantPath(tenant, "agents", agent, "events")+"?"+q.Encode(), nil, &out)
}

// Stream calls fn with each of the agent's events after seq as it is
// stored, until ctx ends, fn fails or the connection drops. To carry on
// after a drop, call it again with the last seq seen.
func (c *Client) Stream(ctx context.Context, tenant, agent string, after int64, fn func(Event) error) error {
	req, err := c.request(ctx, http.MethodGet, tenantPath(tenant, "agents", agent, "events", "stream")+"?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data []byte
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0 && len(data) > 0:
			var e Event
			if err := json.Unmarshal(data, &e); err != nil {
				return fmt.Errorf("event stream: %w", err)
			}
			if err := fn(e); err != nil {
				return err
			}
			data = data[:0]
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimSpace(line[5:])...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("event stream closed")
}
