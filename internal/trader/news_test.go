package trader

import (
	"testing"
	"time"
)

func TestNewsFeedIsWindowedCleanedAndDeduplicated(t *testing.T) {
	cutoff := time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)
	feed := []byte(`<?xml version="1.0"?><rss version="2.0"><channel>
<item><title>Old story</title><link>https://x/1</link><pubDate>Mon, 05 Oct 2026 10:00:00 +0000</pubDate><guid>1</guid></item>
<item><title>Earnings &amp; guidance</title><description>&lt;b&gt;Beat&lt;/b&gt;   on   revenue</description><link>https://x/2</link><pubDate>Fri, 23 Oct 2026 12:00:00 +0000</pubDate><guid>2</guid></item>
<item><title>Earnings &amp; guidance</title><link>https://x/2</link><pubDate>Fri, 23 Oct 2026 12:00:00 +0000</pubDate><guid>2</guid></item>
<item><title>After the decision</title><link>https://x/3</link><pubDate>Fri, 23 Oct 2026 22:00:00 +0000</pubDate><guid>3</guid></item>
<item><title>Earlier this week</title><link>https://x/4</link><pubDate>Tue, 20 Oct 2026 09:00:00 +0000</pubDate><guid>4</guid></item>
</channel></rss>`)
	items, err := ParseNewsRSS(feed, "AAA", 5, cutoff, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Title != "Earnings & guidance" || items[0].Summary != "Beat on revenue" || items[1].Title != "Earlier this week" {
		t.Fatalf("expected two in-window items, newest first, cleaned: %+v", items)
	}
	again, _ := ParseNewsRSS(feed, "AAA", 1, cutoff, 7*24*time.Hour)
	if len(again) != 1 || again[0].ID != items[0].ID {
		t.Fatal("limit or stable IDs broken")
	}
}
