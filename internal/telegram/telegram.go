// Package telegram sends alerts to one chat and reads commands from it.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultURL = "https://api.telegram.org"

// Bot only listens to ChatID; messages from any other chat are ignored.
type Bot struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
	ChatID  int64
}

type Command struct {
	Name string
	Args string
	Sent time.Time
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
			Title     string `json:"title"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
		Date int64  `json:"date"`
	} `json:"message"`
}

func (b *Bot) call(ctx context.Context, method string, q url.Values, out any) error {
	base := b.BaseURL
	if base == "" {
		base = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+b.Token+"/"+method, strings.NewReader(q.Encode()))
	if err != nil {
		return errors.New("telegram: invalid request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		// The URL holds the token, so only the cause is reported.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d", method, resp.StatusCode)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Send posts plain text to the chat, cut to Telegram's message limit.
func (b *Bot) Send(ctx context.Context, text string) error {
	if r := []rune(text); len(r) > 4000 {
		text = string(r[:4000]) + "…"
	}
	return b.call(ctx, "sendMessage", url.Values{
		"chat_id":              {strconv.FormatInt(b.ChatID, 10)},
		"text":                 {text},
		"link_preview_options": {`{"is_disabled":true}`},
	}, nil)
}

// Poll waits up to wait for new messages and returns the commands the
// configured user sent in their private chat with the bot, with the offset
// to pass next time. In a private chat the chat ID is the user's ID, so
// commands posted by others in a group are ignored.
func (b *Bot) Poll(ctx context.Context, offset int64, wait time.Duration) ([]Command, int64, error) {
	ups, next, err := b.updates(ctx, offset, wait)
	var cmds []Command
	for _, u := range ups {
		if u.Message == nil || u.Message.Chat.ID != b.ChatID || u.Message.From == nil || u.Message.From.ID != b.ChatID {
			continue
		}
		text := strings.TrimSpace(u.Message.Text)
		if !strings.HasPrefix(text, "/") {
			continue
		}
		name, args, _ := strings.Cut(text[1:], " ")
		name, _, _ = strings.Cut(name, "@")
		cmds = append(cmds, Command{Name: strings.ToLower(name), Args: strings.TrimSpace(args), Sent: time.Unix(u.Message.Date, 0).UTC()})
	}
	return cmds, next, err
}

func (b *Bot) updates(ctx context.Context, offset int64, wait time.Duration) ([]update, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, wait+20*time.Second)
	defer cancel()
	var ups []update
	err := b.call(ctx, "getUpdates", url.Values{
		"offset":          {strconv.FormatInt(offset, 10)},
		"timeout":         {strconv.Itoa(int(wait.Seconds()))},
		"allowed_updates": {`["message"]`},
	}, &ups)
	for _, u := range ups {
		offset = max(offset, u.UpdateID+1)
	}
	return ups, offset, err
}

// Chats lists chats that have messaged the bot, so the user can find the
// chat ID to put in .env.
func (b *Bot) Chats(ctx context.Context) (map[int64]string, error) {
	ups, _, err := b.updates(ctx, 0, 0)
	out := map[int64]string{}
	for _, u := range ups {
		if u.Message != nil {
			c := u.Message.Chat
			out[c.ID] = strings.TrimSpace(c.Title + " " + c.FirstName + " @" + c.Username)
		}
	}
	return out, err
}
