package analyst

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/candylario/trader/internal/testkit"
)

func TestReviewValidatesLessonsAndKeepsReceipts(t *testing.T) {
	dir := t.TempDir()
	c := testkit.Config(dir)
	api := &testkit.API{Script: []func(string) (int, any){
		testkit.LessonsStep(map[string]any{"text": "Doge pumps fade.", "symbols": []string{"DOGEUSDT"}, "supersedes": ""}),
		testkit.LessonsStep(
			map[string]any{"text": "A BTC buy on momentum alone needs volume above the weekly average, or it stalls.", "symbols": []string{"BTCUSDT", "BTCUSDT"}, "supersedes": "lesson:1"},
			map[string]any{"text": "  Hold through weekend chop. ", "symbols": []string{}, "supersedes": ""}),
	}}
	a := newTestAnalyst(t, c, api.Server(t).URL)
	g := Graded{DecisionID: "d1", Decision: 1, HorizonHours: 24,
		Outcomes: []Outcome{{Symbol: "BTCUSDT", Rating: RatingBuy, Confidence: "medium", Rationale: "Momentum.", Evidence: []string{"prices:BTCUSDT"}, Entry: 80000, Exit: 78400, Return: -0.02}},
		Lessons:  []LessonBrief{{ID: "lesson:1", Text: "Momentum buys work."}}}
	res, err := a.Review(context.Background(), ReviewInput{Graded: g, Dir: dir})
	if err != nil || res.Turns != 2 || len(res.Lessons) != 2 {
		t.Fatalf("review: %+v %v", res, err)
	}
	if l := res.Lessons[0]; l.Supersedes != "lesson:1" || len(l.Symbols) != 1 || !strings.HasPrefix(l.Text, "A BTC buy") {
		t.Fatalf("first lesson: %+v", l)
	}
	if l := res.Lessons[1]; l.Text != "Hold through weekend chop." || len(l.Symbols) != 0 || l.Supersedes != "" {
		t.Fatalf("second lesson: %+v", l)
	}
	if !strings.Contains(api.Bodies[1], "is not a traded symbol") || !strings.Contains(api.Bodies[1], `"is_error":true`) {
		t.Fatal("a lesson about a coin the agent does not trade is rejected as a tool error")
	}
	first := api.Requests[0]
	if tc := first["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Fatalf("tool choice %v", tc)
	}
	tool := first["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "submit_lessons" || tool["strict"] != true {
		t.Fatalf("tool %v", tool)
	}
	if !strings.Contains(first["system"].([]any)[0].(map[string]any)["text"].(string), "24 hours that followed") {
		t.Fatal("the prompt must state the horizon")
	}
	if sum, _ := a.Budget.Summary(context.Background()); sum.MonthCalls != 2 || sum.UnresolvedUSD != 0 || res.CostUSD <= 0 {
		t.Fatalf("budget: %+v, cost %v", sum, res.CostUSD)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls", "review-turn-2.json")); err != nil {
		t.Fatal("review receipts are archived")
	}
}

func TestReviewRejectsTooManyOrDuplicateLessons(t *testing.T) {
	known := map[string]bool{"lesson:1": true}
	for name, raw := range map[string]string{
		"four lessons":        `{"lessons":[{"text":"a","symbols":[],"supersedes":""},{"text":"b","symbols":[],"supersedes":""},{"text":"c","symbols":[],"supersedes":""},{"text":"d","symbols":[],"supersedes":""}]}`,
		"a repeated lesson":   `{"lessons":[{"text":"a","symbols":[],"supersedes":""},{"text":"a","symbols":[],"supersedes":""}]}`,
		"an unknown lesson":   `{"lessons":[{"text":"a","symbols":[],"supersedes":"lesson:9"}]}`,
		"superseded twice":    `{"lessons":[{"text":"a","symbols":[],"supersedes":"lesson:1"},{"text":"b","symbols":[],"supersedes":"lesson:1"}]}`,
		"an empty lesson":     `{"lessons":[{"text":"   ","symbols":[],"supersedes":""}]}`,
		"an unexpected field": `{"lessons":[],"note":"x"}`,
	} {
		if _, err := parseLessons(raw, []string{"BTCUSDT"}, known); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	if out, err := parseLessons(`{"lessons":[]}`, []string{"BTCUSDT"}, known); err != nil || len(out) != 0 {
		t.Fatalf("no lessons is a valid answer: %v %v", out, err)
	}
}
