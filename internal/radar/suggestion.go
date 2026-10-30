package radar

import "fmt"

// Suggestion is the trade the history argues for on an alert, with the
// stop that history says it needs. Empty when the record says stay out.
type Suggestion struct {
	Side        string  `json:"side"`          // long or short
	StopLossPct float64 `json:"stop_loss_pct"` // as a fraction of the entry
	Reason      string  `json:"reason"`
}

// String is the suggestion as a chat shows it.
func (s Suggestion) String() string {
	if s.Side == "" {
		return ""
	}
	return fmt.Sprintf("💡 %s with a %.0f%% stop: %s", s.Side, s.StopLossPct*100, s.Reason)
}

// suggest applies what the studies found. Rises on volume have faded, so
// they are shorted; rises on thin volume have continued, so they are left
// alone. A crash after a pump has kept falling; a crash of a coin that
// was not pumped has bounced, after dipping a further 8–10% first. Very
// negative funding has meant the fall continues.
func suggest(a Alert) Suggestion {
	m := a.Change15m
	switch {
	case m >= 0.03 && a.VolumeRate >= 2:
		return Suggestion{Side: "short", StopLossPct: 0.08, Reason: "rises on volume have faded; the few that kept going went far, hence the stop"}
	case m >= 0.03:
		return Suggestion{}
	case m <= -0.10:
		prior := (1+a.Change24h)/(1+a.Change15m) - 1
		if prior >= 0.2 {
			return Suggestion{Side: "short", StopLossPct: 0.10, Reason: "a crash after a pump has kept falling"}
		}
		return Suggestion{Side: "long", StopLossPct: 0.15, Reason: "crashes of coins that were not pumped have bounced, after dipping 8–10% more first"}
	case a.Funding <= fundingVeryNegative:
		return Suggestion{Side: "short", StopLossPct: 0.08, Reason: "very negative funding has meant the fall continues"}
	}
	return Suggestion{}
}
