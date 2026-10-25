package trader

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// NewsItem is one headline shown to the analyst. Its ID is the evidence ID the
// analyst must cite.
type NewsItem struct {
	ID          string `json:"id"`
	Symbol      string `json:"symbol"`
	Title       string `json:"title"`
	Summary     string `json:"summary,omitempty"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
}

// NewsSource reads Yahoo Finance's per-symbol headline RSS feed.
type NewsSource struct {
	HTTP     *http.Client
	BaseURL  string
	Lookback time.Duration
}

func (n NewsSource) Headlines(ctx context.Context, symbol string, limit int, cutoff time.Time) ([]NewsItem, error) {
	base := n.BaseURL
	if base == "" {
		base = "https://feeds.finance.yahoo.com"
	}
	u := base + "/rss/2.0/headline?s=" + url.QueryEscape(symbol) + "&region=US&lang=en-US"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; trader-agent/1.0)")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("news for %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("news provider returned HTTP %d for %s", resp.StatusCode, symbol)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	lookback := n.Lookback
	if lookback <= 0 {
		lookback = 7 * 24 * time.Hour
	}
	return ParseNewsRSS(raw, symbol, limit, cutoff, lookback)
}

type rssFeed struct {
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

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// ParseNewsRSS keeps items published within the lookback window before cutoff,
// newest first.
func ParseNewsRSS(raw []byte, symbol string, limit int, cutoff time.Time, lookback time.Duration) ([]NewsItem, error) {
	var feed rssFeed
	if err := xml.Unmarshal(raw, &feed); err != nil {
		return nil, fmt.Errorf("invalid news feed for %s", symbol)
	}
	type dated struct {
		item NewsItem
		at   time.Time
	}
	var items []dated
	seen := map[string]bool{}
	for _, it := range feed.Channel.Items {
		published, err := time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate))
		if err != nil {
			published, err = time.Parse(time.RFC1123, strings.TrimSpace(it.PubDate))
		}
		if err != nil || published.After(cutoff) || published.Before(cutoff.Add(-lookback)) {
			continue
		}
		title := cleanText(it.Title, 300)
		if title == "" {
			continue
		}
		key := strings.TrimSpace(it.GUID)
		if key == "" {
			key = strings.TrimSpace(it.Link)
		}
		id := "news:" + Hash([]byte(symbol + "\x00" + key + "\x00" + title))[:12]
		if seen[id] {
			continue
		}
		seen[id] = true
		items = append(items, dated{NewsItem{
			ID:          id,
			Symbol:      symbol,
			Title:       title,
			Summary:     cleanText(it.Description, 500),
			URL:         strings.TrimSpace(it.Link),
			PublishedAt: published.UTC().Format(time.RFC3339),
		}, published})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	out := make([]NewsItem, 0, min(limit, len(items)))
	for _, d := range items {
		if len(out) == limit {
			break
		}
		out = append(out, d.item)
	}
	return out, nil
}

func cleanText(s string, limit int) string {
	s = html.UnescapeString(htmlTag.ReplaceAllString(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}
