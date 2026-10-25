package analyst

import "sort"

// Rating is the analyst's view of a symbol over the configured horizon. The
// analyst never chooses quantities; PlanOrders maps ratings to target weights.
type Rating string

const (
	RatingStrongSell Rating = "strong_sell"
	RatingSell       Rating = "sell"
	RatingHold       Rating = "hold"
	RatingBuy        Rating = "buy"
	RatingStrongBuy  Rating = "strong_buy"
)

var ratingScores = map[Rating]int{RatingStrongSell: -2, RatingSell: -1, RatingHold: 0, RatingBuy: 1, RatingStrongBuy: 2}

func (r Rating) Score() (int, bool) { s, ok := ratingScores[r]; return s, ok }

func ratingFromScore(score int) Rating {
	for r, s := range ratingScores {
		if s == score {
			return r
		}
	}
	return RatingHold
}

var confidenceLevels = []string{"low", "medium", "high"}

type Signal struct {
	Symbol     string   `json:"symbol"`
	Rating     Rating   `json:"rating"`
	Confidence string   `json:"confidence"`
	Rationale  string   `json:"rationale"`
	Evidence   []string `json:"evidence_ids"`
	// Agreement is the share of valid samples that produced this rating.
	Agreement float64 `json:"agreement"`
}

// Aggregate combines independent analyst samples by median rating. With an
// even count the milder of the two middle views wins and opposite views hold,
// so the result is always a rating some sample chose, or hold.
func Aggregate(symbols []string, samples []map[string]Signal) map[string]Signal {
	out := map[string]Signal{}
	for _, symbol := range symbols {
		var scores []int
		for _, sample := range samples {
			if sig, ok := sample[symbol]; ok {
				if score, ok := sig.Rating.Score(); ok {
					scores = append(scores, score)
				}
			}
		}
		if len(scores) == 0 {
			out[symbol] = Signal{Symbol: symbol, Rating: RatingHold, Confidence: "low", Rationale: "No valid analyst sample; keeping the current position."}
			continue
		}
		sort.Ints(scores)
		median := scores[len(scores)/2]
		if len(scores)%2 == 0 {
			lo, hi := scores[len(scores)/2-1], scores[len(scores)/2]
			switch {
			case lo < 0 && hi > 0:
				median = 0
			case hi <= 0:
				median = hi
			default:
				median = lo
			}
		}
		rating := ratingFromScore(median)
		agree := 0
		for _, s := range scores {
			if s == median {
				agree++
			}
		}
		sig := Signal{Symbol: symbol, Rating: rating, Confidence: "low", Rationale: "Samples disagreed; using the median view.", Agreement: float64(agree) / float64(len(scores))}
		for _, sample := range samples {
			if s, ok := sample[symbol]; ok && s.Rating == rating {
				sig.Confidence, sig.Rationale, sig.Evidence = s.Confidence, s.Rationale, s.Evidence
				break
			}
		}
		out[symbol] = sig
	}
	return out
}
