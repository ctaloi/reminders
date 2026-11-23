package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseWhen reads a due date the way it'd be said, and returns it as the
// bridge takes it: "2026-11-28" for a day, RFC 3339 for a time.
//
//	today, tomorrow, fri, next fri, nov 28, 11/28, 2026-11-28
//	any of those with a time: 9am, 9:30pm, 17:00, noon
//	a time alone: today, or tomorrow if it has passed
//	in 30m, in 2h, in 3 days, in 2 weeks
func parseWhen(s string, now time.Time) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", fmt.Errorf("when?")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format(time.RFC3339), nil
	}
	if m := regexp.MustCompile(`^in (\d+) ?(m|min|mins|minutes?|h|hrs?|hours?|d|days?|w|weeks?)$`).FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2][0] {
		case 'm':
			return now.Add(time.Duration(n) * time.Minute).Truncate(time.Minute).Format(time.RFC3339), nil
		case 'h':
			return now.Add(time.Duration(n) * time.Hour).Truncate(time.Minute).Format(time.RFC3339), nil
		case 'd':
			return now.AddDate(0, 0, n).Format("2006-01-02"), nil
		case 'w':
			return now.AddDate(0, 0, 7*n).Format("2006-01-02"), nil
		}
	}

	words := strings.Fields(strings.NewReplacer(" at ", " ", ",", " ").Replace(" " + s + " "))
	var day *time.Time
	hour, minute := -1, 0
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	setDay := func(t time.Time) error {
		if day != nil {
			return fmt.Errorf("two days in %q", s)
		}
		day = &t
		return nil
	}
	next := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "next" {
			next = true
			continue
		}
		if h, mi, ok := parseClock(w); ok {
			hour, minute = h, mi
			continue
		}
		var err error
		switch {
		case w == "today":
			err = setDay(today)
		case w == "tomorrow" || w == "tmrw":
			err = setDay(today.AddDate(0, 0, 1))
		case weekday(w) >= 0:
			// "fri" is the coming Friday (today, on a Friday); "next fri"
			// the same, except a week out when today is Friday.
			d := (weekday(w) - int(today.Weekday()) + 7) % 7
			if d == 0 && next {
				d = 7
			}
			err = setDay(today.AddDate(0, 0, d))
		case month(w) > 0 && i+1 < len(words):
			n, aerr := strconv.Atoi(strings.TrimRight(words[i+1], "stndrh"))
			if aerr != nil {
				return "", fmt.Errorf("which day of %s?", w)
			}
			i++
			err = setDay(nextDate(today, time.Month(month(w)), n))
		case regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(w):
			t, perr := time.ParseInLocation("2006-01-02", w, time.Local)
			if perr != nil {
				return "", perr
			}
			err = setDay(t)
		case regexp.MustCompile(`^\d{1,2}/\d{1,2}(/\d{2,4})?$`).MatchString(w):
			p := strings.Split(w, "/")
			mo, _ := strconv.Atoi(p[0])
			d, _ := strconv.Atoi(p[1])
			if len(p) == 3 {
				y, _ := strconv.Atoi(p[2])
				if y < 100 {
					y += 2000
				}
				err = setDay(time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.Local))
			} else {
				err = setDay(nextDate(today, time.Month(mo), d))
			}
		default:
			return "", fmt.Errorf("didn't understand %q in %q", w, s)
		}
		if err != nil {
			return "", err
		}
	}
	if day == nil && hour < 0 {
		return "", fmt.Errorf("when is %q?", s)
	}
	if day == nil {
		t := time.Date(today.Year(), today.Month(), today.Day(), hour, minute, 0, 0, time.Local)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t.Format(time.RFC3339), nil
	}
	if hour < 0 {
		return day.Format("2006-01-02"), nil
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, time.Local).Format(time.RFC3339), nil
}

// nextDate is month/day this year, or next year if that's already past.
func nextDate(today time.Time, m time.Month, d int) time.Time {
	t := time.Date(today.Year(), m, d, 0, 0, 0, 0, time.Local)
	if t.Before(today) {
		t = t.AddDate(1, 0, 0)
	}
	return t
}

var clockRe = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm|a|p)?$`)

func parseClock(w string) (int, int, bool) {
	switch w {
	case "noon":
		return 12, 0, true
	case "midnight":
		return 0, 0, true
	}
	m := clockRe.FindStringSubmatch(w)
	if m == nil || (m[2] == "" && m[3] == "") {
		return 0, 0, false // a bare number isn't a time
	}
	h, _ := strconv.Atoi(m[1])
	mi := 0
	if m[2] != "" {
		mi, _ = strconv.Atoi(m[2])
	}
	if h > 23 || mi > 59 {
		return 0, 0, false
	}
	switch m[3] {
	case "pm", "p":
		if h < 12 {
			h += 12
		}
	case "am", "a":
		if h == 12 {
			h = 0
		}
	}
	return h, mi, true
}

func weekday(w string) int {
	for i, n := range []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"} {
		full := strings.ToLower(time.Weekday(i).String())
		if w == n || w == full || (len(w) >= 3 && strings.HasPrefix(full, w)) {
			return i
		}
	}
	return -1
}

func month(w string) int {
	if len(w) < 3 {
		return 0
	}
	for i := 1; i <= 12; i++ {
		full := strings.ToLower(time.Month(i).String())
		if strings.HasPrefix(full, w) {
			return i
		}
	}
	return 0
}
