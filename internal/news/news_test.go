package news

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func feed(items ...string) string {
	return `<?xml version="1.0"?><rss version="2.0"><channel>` + strings.Join(items, "") + `</channel></rss>`
}

func item(title, link, date string) string {
	return `<item><title><![CDATA[` + title + `]]></title><link>` + link + `</link><description>&lt;p&gt;Details   here.&lt;/p&gt;</description><pubDate>` + date + `</pubDate><guid>` + link + `</guid></item>`
}

func TestFetchMergesFeedsNewestFirstWithoutDuplicates(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(feed(
			item("Bitcoin ETF sees record inflows", "https://a/1", "Sun, 25 Oct 2026 11:00:00 +0000"),
			item("Ether staking climbs", "https://a/2", "Sun, 25 Oct 2026 09:00:00 +0000"),
			item("Old story", "https://a/3", "Mon, 12 Oct 2026 09:00:00 +0000"),
			item("From the future", "https://a/4", "Sun, 25 Oct 2026 13:00:00 +0000"),
		)))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(feed(item("Bitcoin ETF Sees Record Inflows", "https://b/9", "Sun, 25 Oct 2026 11:05:00 +0000"), item("Solana outage", "https://b/2", "Sun, 25 Oct 2026 10:00:00 +0000"))))
	}))
	defer b.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer down.Close()

	items, err := Feeds{HTTP: http.DefaultClient, URLs: []string{a.URL, b.URL, down.URL}}.Fetch(context.Background(), now, 72*time.Hour, []string{"BTC", "ETH"})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	if strings.Join(titles, " | ") != "Bitcoin ETF sees record inflows | Solana outage | Ether staking climbs" {
		t.Fatalf("got %q", titles)
	}
	if items[0].Summary != "Details here." || len(items[0].Coins) != 1 || items[0].Coins[0] != "BTC" || items[2].Coins[0] != "ETH" || items[1].Coins != nil {
		t.Fatalf("cleaning or coin tags wrong: %+v", items)
	}
	if got := About(items, "ETH", 5); len(got) != 1 || got[0].Title != "Ether staking climbs" {
		t.Fatalf("About(ETH) = %+v", got)
	}
	if _, err := (Feeds{HTTP: http.DefaultClient, URLs: []string{down.URL}}).Fetch(context.Background(), now, time.Hour, nil); err == nil {
		t.Fatal("all feeds down must be an error")
	}
}

func TestMentionsMatchesWholeWords(t *testing.T) {
	if Mentions("Ethena launches", Names("ETH")) || !Mentions("ETH breaks $3k", Names("ETH")) || !Mentions("bitcoin dips", Names("BTC")) {
		t.Fatal("word matching broken")
	}
}
