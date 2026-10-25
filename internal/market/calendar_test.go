package market

import (
	"fmt"
	"testing"
	"time"
)

func TestReviewedExchangeSchedule(t *testing.T) {
	for year, want := range map[int]int{2026: 251, 2027: 251, 2028: 251} {
		sessions, err := NYSESessions(fmt.Sprintf("%d-01-01", year), fmt.Sprintf("%d-12-31", year))
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != want {
			t.Fatalf("%d: got %d sessions, want %d", year, len(sessions), want)
		}
	}
	// Closures as published at nyse.com/markets/hours-calendars.
	closures := []string{"2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19", "2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25",
		"2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18", "2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24",
		"2028-01-17", "2028-02-21", "2028-04-14", "2028-05-29", "2028-06-19", "2028-07-04", "2028-09-04", "2028-11-23", "2028-12-25"}
	for _, date := range closures {
		if _, open, err := SessionOn(date); err != nil || open {
			t.Fatalf("expected closure %s: %v", date, err)
		}
	}
	for _, date := range []string{"2026-11-27", "2026-12-24", "2027-11-26", "2028-07-03", "2028-11-24"} {
		if s, open, _ := SessionOn(date); !open || !s.EarlyClose {
			t.Fatalf("expected 1:00 p.m. close on %s", date)
		}
	}
	for _, date := range []string{"2026-07-02", "2027-07-02", "2027-12-23", "2027-12-31", "2028-12-22"} {
		if s, open, _ := SessionOn(date); !open || s.EarlyClose {
			t.Fatalf("%s must be a full session", date)
		}
	}
	for _, outside := range []string{"2025-12-31", "2029-01-02"} {
		if _, err := NYSESessions(outside, outside); err == nil {
			t.Fatalf("an unreviewed calendar year was accepted: %s", outside)
		}
	}
}

func TestLatestClosedSessionWaitsForCloseAndBuffer(t *testing.T) {
	buffer := 30 * time.Minute
	for now, want := range map[string]string{
		"2026-10-23 17:00": "2026-10-23", // Friday evening
		"2026-10-23 16:20": "2026-10-22", // vendor bars may not be final yet
		"2026-10-24 12:00": "2026-10-23", // Saturday
		"2026-10-26 10:00": "2026-10-23", // Monday before the close
		"2026-11-27 13:31": "2026-11-27", // early close at 1:00 p.m.
		"2026-11-26 18:00": "2026-11-25", // Thanksgiving
	} {
		got, err := LatestClosedSession(at(t, now), buffer)
		if err != nil || got.Date != want {
			t.Fatalf("%s: got %s, want %s (%v)", now, got.Date, want, err)
		}
	}
}

func TestSessionNavigation(t *testing.T) {
	if s, _ := NextSessionAfter("2026-07-02"); s.Date != "2026-07-06" {
		t.Fatalf("July 3, 2026 is a holiday: %s", s.Date)
	}
	if s, _ := NextSessionAfter("2027-12-23"); s.Date != "2027-12-27" {
		t.Fatalf("Christmas 2027 is observed on Friday: %s", s.Date)
	}
	got, _ := SessionsAfter("2026-10-22", "2026-10-27")
	if len(got) != 3 || got[0].Date != "2026-10-23" || got[2].Date != "2026-10-27" {
		t.Fatalf("SessionsAfter: %+v", got)
	}
	next, err := NextCycleTime(at(t, "2026-10-23 17:00"), 30*time.Minute)
	if err != nil || !next.Equal(at(t, "2026-10-26 16:30")) {
		t.Fatalf("next cycle after Friday evening is Monday 16:30: %v %v", next, err)
	}
	if next, _ := NextCycleTime(at(t, "2026-10-23 16:00"), 30*time.Minute); !next.Equal(at(t, "2026-10-23 16:30")) {
		t.Fatalf("same-day cycle: %v", next)
	}
}

func TestCalendarOpenTimesFollowDST(t *testing.T) {
	winter, _ := NYSESessions("2026-01-08", "2026-01-08")
	summer, _ := NYSESessions("2026-07-02", "2026-07-02")
	if winter[0].Open != "2026-01-08T09:30:00-05:00" || summer[0].Open != "2026-07-02T09:30:00-04:00" {
		t.Fatal("exchange time zone is incorrect")
	}
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", value, NewYork)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
