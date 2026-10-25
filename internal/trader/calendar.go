package trader

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata"
)

// CalendarVersion identifies the reviewed schedule: the years a live agent can
// trade in. Dates were checked against
// https://www.nyse.com/markets/hours-calendars on 2026-10-25.
const CalendarVersion = "xnys-2026-2027-v3"

const firstCalendarYear, lastCalendarYear = 2026, 2027

var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err) // tzdata is embedded, so this cannot fail at runtime
	}
	return loc
}()

type Session struct {
	Date       string `json:"date"`
	Open       string `json:"open"`
	Close      string `json:"close"`
	EarlyClose bool   `json:"early_close"`
}

func (s Session) OpenTime() time.Time  { t, _ := time.Parse(time.RFC3339, s.Open); return t }
func (s Session) CloseTime() time.Time { t, _ := time.Parse(time.RFC3339, s.Close); return t }

func NYSESessions(start, end string) ([]Session, error) {
	from, err := time.Parse(time.DateOnly, start)
	if err != nil {
		return nil, err
	}
	to, err := time.Parse(time.DateOnly, end)
	if err != nil {
		return nil, err
	}
	if from.Year() < firstCalendarYear || to.Year() > lastCalendarYear || to.Before(from) {
		return nil, fmt.Errorf("%s covers %d–%d only; review and extend the calendar before using other years", CalendarVersion, firstCalendarYear, lastCalendarYear)
	}
	var sessions []Session
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || nyseHoliday(d) {
			continue
		}
		early := nyseEarlyClose(d)
		hour := 16
		if early {
			hour = 13
		}
		sessions = append(sessions, Session{
			Date:       d.Format(time.DateOnly),
			Open:       time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, newYork).Format(time.RFC3339),
			Close:      time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, newYork).Format(time.RFC3339),
			EarlyClose: early,
		})
	}
	return sessions, nil
}

// SessionOn reports whether the exchange trades on date.
func SessionOn(date string) (Session, bool, error) {
	sessions, err := NYSESessions(date, date)
	if err != nil || len(sessions) == 0 {
		return Session{}, false, err
	}
	return sessions[0], true, nil
}

// LatestClosedSession returns the most recent session whose close plus the
// buffer is at or before now. The buffer lets the vendor publish final bars.
func LatestClosedSession(now time.Time, buffer time.Duration) (Session, error) {
	day := now.In(newYork)
	for i := 0; i < 15; i++ {
		s, ok, err := SessionOn(day.AddDate(0, 0, -i).Format(time.DateOnly))
		if err != nil {
			return Session{}, err
		}
		if ok && !s.CloseTime().Add(buffer).After(now) {
			return s, nil
		}
	}
	return Session{}, errors.New("no completed session in the last 15 days")
}

// NextSessionAfter returns the first session strictly after date.
func NextSessionAfter(date string) (Session, error) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return Session{}, err
	}
	for i := 1; i <= 15; i++ {
		s, ok, err := SessionOn(d.AddDate(0, 0, i).Format(time.DateOnly))
		if err != nil {
			return Session{}, err
		}
		if ok {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("no session within 15 days after %s", date)
}

// SessionsAfter returns the sessions strictly after `after` through `through`.
func SessionsAfter(after, through string) ([]Session, error) {
	a, err := time.Parse(time.DateOnly, after)
	if err != nil {
		return nil, err
	}
	start := a.AddDate(0, 0, 1).Format(time.DateOnly)
	if start > through {
		return nil, nil
	}
	return NYSESessions(start, through)
}

// NextCycleTime is when the next decision can run: the first session close
// plus buffer that is still in the future.
func NextCycleTime(now time.Time, buffer time.Duration) (time.Time, error) {
	day := now.In(newYork)
	for i := 0; i < 15; i++ {
		s, ok, err := SessionOn(day.AddDate(0, 0, i).Format(time.DateOnly))
		if err != nil {
			return time.Time{}, err
		}
		if ok && s.CloseTime().Add(buffer).After(now) {
			return s.CloseTime().Add(buffer), nil
		}
	}
	return time.Time{}, errors.New("no upcoming session in the next 15 days")
}

func nthWeekday(year int, month time.Month, weekday time.Weekday, n int) time.Time {
	d := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, (int(weekday)-int(d.Weekday())+7)%7+7*(n-1))
}

func observed(d time.Time) time.Time {
	if d.Weekday() == time.Saturday {
		return d.AddDate(0, 0, -1)
	}
	if d.Weekday() == time.Sunday {
		return d.AddDate(0, 0, 1)
	}
	return d
}

func easter(year int) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func nyseHoliday(d time.Time) bool {
	y := d.Year()
	date := d.Format(time.DateOnly)
	newYear := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
	// NYSE does not observe Saturday New Year's Day on the prior Friday.
	if newYear.Weekday() == time.Sunday {
		newYear = newYear.AddDate(0, 0, 1)
	}
	memorial := time.Date(y, time.June, 0, 0, 0, 0, 0, time.UTC)
	for memorial.Weekday() != time.Monday {
		memorial = memorial.AddDate(0, 0, -1)
	}
	holidays := []time.Time{newYear, nthWeekday(y, time.January, time.Monday, 3), nthWeekday(y, time.February, time.Monday, 3), easter(y).AddDate(0, 0, -2), memorial, observed(time.Date(y, time.June, 19, 0, 0, 0, 0, time.UTC)), observed(time.Date(y, time.July, 4, 0, 0, 0, 0, time.UTC)), nthWeekday(y, time.September, time.Monday, 1), nthWeekday(y, time.November, time.Thursday, 4), observed(time.Date(y, time.December, 25, 0, 0, 0, 0, time.UTC))}
	for _, holiday := range holidays {
		if date == holiday.Format(time.DateOnly) {
			return true
		}
	}
	return false
}

func nyseEarlyClose(d time.Time) bool {
	if d.Equal(nthWeekday(d.Year(), time.November, time.Thursday, 4).AddDate(0, 0, 1)) {
		return true
	}
	// The published 2026–2027 schedule has one Christmas Eve early close and
	// no July early close (July 4 falls on a weekend in both years).
	return d.Format(time.DateOnly) == "2026-12-24"
}
