// Package news reads crypto headlines from RSS feeds.
package news

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ismatsamadov/trader/internal/store"
)

// Item is one headline. Its ID is the evidence ID the analyst cites.
type Item struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary,omitempty"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Coins       []string  `json:"coins,omitempty"`
}

type Feeds struct {
	HTTP *http.Client
	URLs []string
}

// Fetch reads every feed at once and returns items published within lookback
// before now, newest first, without duplicates across feeds. It fails only
// when no feed answers.
func (f Feeds) Fetch(ctx context.Context, now time.Time, lookback time.Duration, coins []string) ([]Item, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var all []Item
	var errs []error
	for _, u := range f.URLs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := f.one(ctx, u, now, lookback)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			all = append(all, items...)
		}()
	}
	wg.Wait()
	if len(errs) == len(f.URLs) && len(errs) > 0 {
		return nil, fmt.Errorf("no news feed answered: %w", errors.Join(errs...))
	}
	return Tag(dedupe(all), coins), nil
}

func (f Feeds) one(ctx context.Context, u string, now time.Time, lookback time.Duration) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; trader/1.0)")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s: %w", source(u), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", source(u), resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	return Parse(raw, source(u), now, lookback)
}

type rss struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
			GUID        string `xml:"guid"`
		} `xml:"item"`
	} `xml:"channel"`
}

var layouts = []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", time.RFC3339}

func Parse(raw []byte, src string, now time.Time, lookback time.Duration) ([]Item, error) {
	var feed rss
	if err := xml.Unmarshal(raw, &feed); err != nil {
		return nil, fmt.Errorf("%s: invalid RSS", src)
	}
	var out []Item
	for _, it := range feed.Channel.Items {
		var at time.Time
		var err error
		for _, layout := range layouts {
			if at, err = time.Parse(layout, strings.TrimSpace(it.PubDate)); err == nil {
				break
			}
		}
		if err != nil || at.After(now) || at.Before(now.Add(-lookback)) {
			continue
		}
		title := CleanText(it.Title, 300)
		if title == "" {
			continue
		}
		key := strings.TrimSpace(it.GUID)
		if key == "" {
			key = strings.TrimSpace(it.Link)
		}
		out = append(out, Item{
			ID:          "news:" + store.Hash([]byte(src + "\x00" + key + "\x00" + title))[:12],
			Source:      src,
			Title:       title,
			Summary:     CleanText(it.Description, 400),
			URL:         cleanURL(it.Link),
			PublishedAt: at.UTC(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out[:min(len(out), maxPerFeed)], nil
}

// maxPerFeed bounds what one feed, however large, can add to a pass.
const maxPerFeed = 50

// cleanURL keeps short http(s) links only; anything else is dropped.
func cleanURL(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(s) > 500 || strings.ContainsFunc(s, unicode.IsControl) {
		return ""
	}
	return s
}

// dedupe keeps the earliest copy of a story syndicated under the same title
// or link, newest stories first.
func dedupe(items []Item) []Item {
	sort.SliceStable(items, func(i, j int) bool { return items[i].PublishedAt.Before(items[j].PublishedAt) })
	seen := map[string]bool{}
	var out []Item
	for _, it := range items {
		title := strings.ToLower(it.Title)
		if seen[title] || (it.URL != "" && seen[it.URL]) {
			continue
		}
		seen[title], seen[it.URL] = true, true
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out
}

var names = map[string][]string{
	"BTC": {"bitcoin", "btc"},
	"ETH": {"ethereum", "ether", "eth"},
	"SOL": {"solana", "sol"},
	"BNB": {"bnb"},
	"XRP": {"xrp", "ripple"},
}

// Names are the words that mark a headline as being about a coin.
func Names(coin string) []string {
	if n, ok := names[coin]; ok {
		return n
	}
	return []string{strings.ToLower(coin)}
}

// Tag records which coins each item mentions.
func Tag(items []Item, coins []string) []Item {
	for i := range items {
		items[i].Coins = nil
		text := items[i].Title + " " + items[i].Summary
		for _, coin := range coins {
			if Mentions(text, Names(coin)) {
				items[i].Coins = append(items[i].Coins, coin)
			}
		}
	}
	return items
}

// Mentions reports whether text contains any of the words, ignoring case.
func Mentions(text string, words []string) bool {
	for _, w := range words {
		if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(w) + `\b`).MatchString(text) {
			return true
		}
	}
	return false
}

// About returns up to n items mentioning coin, or any items when coin is empty.
func About(items []Item, coin string, n int) []Item {
	out := []Item{}
	for _, it := range items {
		if len(out) == n {
			break
		}
		if coin == "" || containsString(it.Coins, coin) {
			out = append(out, it)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func source(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return u
	}
	return strings.TrimPrefix(p.Host, "www.")
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// CleanText strips markup, control and text-direction characters, collapses
// whitespace and cuts to limit runes.
func CleanText(s string, limit int) string {
	s = html.UnescapeString(htmlTag.ReplaceAllString(s, " "))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}
