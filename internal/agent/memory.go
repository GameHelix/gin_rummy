package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/candylario/trader/internal/analyst"
	"github.com/candylario/trader/internal/config"
	"github.com/candylario/trader/internal/store"
)

// The agent's memory: lessons the model draws from graded decisions, kept
// in memory.json beside the account, carried into later briefings and
// graded themselves by how the calls that cited them turn out.
const (
	memoryVersion = "memory-v1"
	briefLessons  = 8    // lessons a briefing carries
	maxLessons    = 60   // lessons kept in force
	keptLessons   = 500  // lessons kept at all, retired ones included
	holdBand      = 0.02 // a hold was right while the price stayed within this
	missedMove    = 0.03 // a hold through a move this big is worth a review
	reviewRetry   = 30 * time.Minute
	reviewTries   = 3
)

type Memory struct {
	Version string           `json:"schema_version"`
	Next    int              `json:"next_lesson"`
	Lessons []analyst.Lesson `json:"lessons"`
}

// MemoryPath is where an account's lessons live.
func MemoryPath(c config.Config) string { return filepath.Join(c.Dir(), "memory.json") }

func loadMemory(c config.Config) (Memory, error) {
	var m Memory
	err := store.ReadJSON(MemoryPath(c), &m)
	if errors.Is(err, os.ErrNotExist) {
		return Memory{Version: memoryVersion, Next: 1}, nil
	}
	if err != nil {
		return m, err
	}
	if m.Version != memoryVersion {
		return m, fmt.Errorf("unsupported memory version %q", m.Version)
	}
	return m, nil
}

// saveMemory writes the memory when it has changed since the last write.
func (e *Engine) saveMemory() error {
	raw, err := json.MarshalIndent(e.mem, "", "  ")
	if err != nil {
		return err
	}
	if bytes.Equal(raw, e.memSaved) {
		return nil
	}
	if err := store.AtomicWrite(MemoryPath(e.Config), append(raw, '\n')); err != nil {
		return err
	}
	e.memSaved = raw
	return nil
}

func (m *Memory) find(id string) *analyst.Lesson {
	for i := range m.Lessons {
		if m.Lessons[i].ID == id {
			return &m.Lessons[i]
		}
	}
	return nil
}

// active lists the lessons in force, oldest first.
func (m *Memory) active() []analyst.Lesson {
	var out []analyst.Lesson
	for _, l := range m.Lessons {
		if l.SupersededBy == "" && !l.Retired {
			out = append(out, l)
		}
	}
	return out
}

// usefulness ranks a lesson by how the calls that cited it turned out.
func usefulness(l analyst.Lesson) int { return l.Right - l.Wrong }

// brief picks the lessons a briefing carries: the most useful, then the
// newest.
func (m *Memory) brief() []analyst.LessonBrief {
	act := m.active()
	slices.SortStableFunc(act, func(a, b analyst.Lesson) int {
		if d := usefulness(b) - usefulness(a); d != 0 {
			return d
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	var out []analyst.LessonBrief
	for _, l := range act[:min(briefLessons, len(act))] {
		out = append(out, l.Brief())
	}
	return out
}

// lessonsCited picks the lessons among a rating's evidence.
func lessonsCited(evidence []string) []string {
	var out []string
	for _, id := range evidence {
		if strings.HasPrefix(id, "lesson:") && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// cite counts a rating relying on lessons.
func (m *Memory) cite(ids []string) {
	for _, id := range ids {
		if l := m.find(id); l != nil {
			l.Cited++
		}
	}
}

// grade counts how a call that relied on lessons turned out. A lesson that
// keeps being wrong is retired.
func (m *Memory) grade(ids []string, right bool) {
	for _, id := range ids {
		l := m.find(id)
		if l == nil {
			continue
		}
		if right {
			l.Right++
		} else {
			l.Wrong++
		}
		if l.Wrong >= l.Right+3 {
			l.Retired = true
		}
	}
}

// add stores the lessons of a review. A lesson that refines an earlier one
// takes its place. When too many are in force the least useful, oldest
// first, retire; when too many are kept at all, retired ones are forgotten.
func (m *Memory) add(decision int, now time.Time, items []analyst.NewLesson) []analyst.Lesson {
	var added []analyst.Lesson
	for _, it := range items {
		l := analyst.Lesson{ID: "lesson:" + strconv.Itoa(m.Next), Text: it.Text, Symbols: it.Symbols, CreatedAt: now, Decisions: []int{decision}}
		m.Next++
		if old := m.find(it.Supersedes); old != nil && old.SupersededBy == "" && !old.Retired {
			old.SupersededBy = l.ID
			l.Replaces = old.ID
			l.Decisions = append(slices.Clone(old.Decisions), decision)
		}
		m.Lessons = append(m.Lessons, l)
		added = append(added, l)
	}
	for act := m.active(); len(act) > maxLessons; act = m.active() {
		slices.SortStableFunc(act, func(a, b analyst.Lesson) int {
			if d := usefulness(a) - usefulness(b); d != 0 {
				return d
			}
			return a.CreatedAt.Compare(b.CreatedAt)
		})
		m.find(act[0].ID).Retired = true
	}
	for len(m.Lessons) > keptLessons {
		i := slices.IndexFunc(m.Lessons, func(l analyst.Lesson) bool { return l.SupersededBy != "" || l.Retired })
		if i < 0 {
			break
		}
		m.Lessons = slices.Delete(m.Lessons, i, i+1)
	}
	return added
}

// hit says whether a rating was right over the horizon: a buy before a
// rise, a sell before a fall, a hold while the price stayed in its band.
func hit(score int, ret float64) bool {
	switch {
	case score > 0:
		return ret > 0
	case score < 0:
		return ret < 0
	}
	return math.Abs(ret) < holdBand
}

// tradeCalls counts the graded buy and sell calls and how many were right.
func tradeCalls(scored []ScoredCall) (graded, right int) {
	for _, sc := range scored {
		if score, _ := sc.Rating.Score(); score == 0 {
			continue
		}
		graded++
		if sc.Hit {
			right++
		}
	}
	return graded, right
}

// worthReview says whether a decision's graded calls can teach something:
// it took a view, or it held through a move it could have caught.
func (e *Engine) worthReview(decision int) bool {
	for _, sc := range e.st.Scored {
		if sc.Decision != decision {
			continue
		}
		if score, _ := sc.Rating.Score(); score != 0 || math.Abs(sc.Return) >= missedMove {
			return true
		}
	}
	return false
}

// review is a graded decision the model is reviewing.
type review struct {
	decision int
	started  time.Time
}

type reviewResult struct {
	job *review
	res analyst.ReviewResult
	err error
}

// review starts the next review while no decision is being made: one
// graded decision at a time, in the background.
func (e *Engine) review(ctx context.Context, now time.Time) {
	if e.reviewing != nil || e.job != nil || len(e.st.Reviews) == 0 || now.Before(e.st.ReviewAt) {
		return
	}
	id := e.st.Reviews[0]
	g, ok := e.graded(id)
	if !ok {
		e.st.Reviews, e.st.ReviewTries = e.st.Reviews[1:], 0
		return
	}
	j := &review{decision: id, started: now}
	e.reviewing = j
	dir := filepath.Join(e.Config.Dir(), "decisions", strconv.Itoa(id))
	rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	go func() {
		defer cancel()
		res, err := e.Analyst.Review(rctx, analyst.ReviewInput{Graded: g, Dir: dir})
		e.reviews <- reviewResult{j, res, err}
	}()
}

// graded describes a decision and how its ratings turned out, for review,
// with every lesson in force so the model can refine one.
func (e *Engine) graded(id int) (analyst.Graded, bool) {
	i := slices.IndexFunc(e.st.Decisions, func(d Decision) bool { return d.ID == id })
	if i < 0 {
		return analyst.Graded{}, false
	}
	d := e.st.Decisions[i]
	g := analyst.Graded{DecisionID: "d" + strconv.Itoa(id), Decision: id, Time: d.Time.Format(time.RFC3339), Trigger: d.Trigger, Summary: d.Summary, HorizonHours: e.Config.ScoreHorizonHours}
	for _, sc := range e.st.Scored {
		if sc.Decision != id {
			continue
		}
		sig := d.Signals[sc.Symbol]
		g.Outcomes = append(g.Outcomes, analyst.Outcome{Symbol: sc.Symbol, Rating: sc.Rating, Confidence: sig.Confidence, Rationale: sig.Rationale, Evidence: sig.Evidence,
			Entry: sc.Entry.Float(), Exit: sc.Exit.Float(), Return: sc.Return, Right: sc.Hit})
	}
	if len(g.Outcomes) == 0 {
		return g, false
	}
	for _, l := range e.mem.active() {
		g.Lessons = append(g.Lessons, l.Brief())
	}
	return g, true
}

// finishReview keeps what a review taught, or arranges a retry. A review
// that fails three times is given up.
func (e *Engine) finishReview(ctx context.Context, r reviewResult) {
	e.reviewing = nil
	id, now := r.job.decision, e.now()
	defer e.saveOrAlert(ctx)
	drop := func() {
		e.st.Reviews = slices.DeleteFunc(e.st.Reviews, func(x int) bool { return x == id })
		e.st.ReviewTries, e.st.ReviewAt = 0, time.Time{}
	}
	if r.err != nil {
		if errors.Is(r.err, analyst.ErrBudgetExceeded) {
			e.st.ReviewAt = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			e.logf("review of decision #%d: %v; reviews wait for the next day", id, r.err)
			return
		}
		e.st.ReviewTries++
		e.st.ReviewAt = now.Add(reviewRetry)
		if e.st.ReviewTries < reviewTries {
			e.logf("review of decision #%d: %v; retrying in %s", id, r.err, reviewRetry)
			return
		}
		drop()
		e.alert(ctx, "review", fmt.Sprintf("⚠️ Gave up reviewing decision #%d: %s", id, untrusted(r.err.Error())))
		return
	}
	drop()
	if r.res.Error != "" {
		e.logf("review of decision #%d: %s", id, r.res.Error)
	}
	added := e.mem.add(id, now, r.res.Lessons)
	for _, l := range added {
		e.emit(evLessonRecorded, lessonRecorded{Decision: id, Lesson: l, CostUSD: r.res.CostUSD})
	}
	if len(added) == 0 {
		e.logf("review of decision #%d: nothing to keep", id)
		return
	}
	e.notify(ctx, e.lessonsText(id, added))
}

// lessonsText announces what a review taught.
func (e *Engine) lessonsText(id int, lessons []analyst.Lesson) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📒 Lessons from decision #%d", id)
	var outcomes []string
	for _, sc := range e.st.Scored {
		if sc.Decision != id {
			continue
		}
		mark := "✓"
		if !sc.Hit {
			mark = "✗"
		}
		outcomes = append(outcomes, fmt.Sprintf("%s %s %s %+.1f%%", e.Config.Base(sc.Symbol), label(sc.Rating), mark, sc.Return*100))
	}
	if len(outcomes) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(outcomes, ", "))
	}
	for _, l := range lessons {
		fmt.Fprintf(&b, "\n• %s", untrusted(l.Text))
		if l.Replaces != "" {
			b.WriteString(" (refines an earlier lesson)")
		}
	}
	return b.String()
}
