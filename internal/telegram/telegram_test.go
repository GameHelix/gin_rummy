package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSendAndPollOnlyTrustTheConfiguredChat(t *testing.T) {
	var sent url.Values
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q, _ := url.ParseQuery(string(body))
		switch r.URL.Path {
		case "/botTOKEN/sendMessage":
			sent = q
			w.Write([]byte(`{"ok":true,"result":{}}`))
		case "/botTOKEN/getUpdates":
			if q.Get("offset") != "5" {
				t.Errorf("offset %q", q.Get("offset"))
			}
			w.Write([]byte(`{"ok":true,"result":[
{"update_id":5,"message":{"chat":{"id":42},"from":{"id":42},"text":"/status"}},
{"update_id":6,"message":{"chat":{"id":99},"from":{"id":99},"text":"/sellall"}},
{"update_id":7,"message":{"chat":{"id":42},"from":{"id":42},"text":"hello"}},
{"update_id":8,"message":{"chat":{"id":42},"from":{"id":7},"text":"/resume"}},
{"update_id":9,"message":{"chat":{"id":42},"from":{"id":42},"text":"/Decide@my_bot now please"}}]}`))
		default:
			w.Write([]byte(`{"ok":false,"description":"Not Found"}`))
		}
	}))
	defer s.Close()
	b := &Bot{HTTP: s.Client(), BaseURL: s.URL, Token: "TOKEN", ChatID: 42}
	if err := b.Send(context.Background(), "hello"); err != nil || sent.Get("chat_id") != "42" || sent.Get("text") != "hello" {
		t.Fatalf("send: %v %v", err, sent)
	}
	cmds, next, err := b.Poll(context.Background(), 5, time.Second)
	if err != nil || next != 10 || len(cmds) != 2 || cmds[0].Name != "status" || cmds[1].Name != "decide" || cmds[1].Args != "now please" {
		t.Fatalf("poll: %+v %d %v", cmds, next, err)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	b := &Bot{HTTP: &http.Client{Timeout: time.Second}, BaseURL: "http://127.0.0.1:1", Token: "SECRET-TOKEN", ChatID: 1}
	err := b.Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("error leaks the token or is missing: %v", err)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatal("url.Error carries the URL with the token")
	}
}
