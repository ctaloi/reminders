package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Notifications, from the cache, so they still come when the Mac is out of
// reach for a while. When a reminder rings:
//   - at each of its alarms (absolute, or relative to its due time);
//   - with no alarm, at its due time; a day with no time rings at
//     notify.all_day_at (09:00), like Reminders' Today Notification.
// Location alarms can't ring here and are left to the phone.
// Each ring is remembered (by reminder and time) so it comes once; one
// missed while asleep still comes if it's under an hour late.

const lateLimit = time.Hour

type ring struct {
	Reminder Reminder
	At       time.Time
}

func ringTimes(r Reminder, allDayAt string) []time.Time {
	if r.Completed {
		return nil
	}
	due, allDay, hasDue := dueTime(r.Due)
	var out []time.Time
	any := false
	for _, a := range r.Alarms {
		switch a.Type {
		case "absolute":
			if t, err := time.Parse(time.RFC3339, a.At); err == nil {
				out = append(out, t)
				any = true
			}
		case "relative":
			if hasDue && a.Offset != nil {
				out = append(out, due.Add(time.Duration(*a.Offset)*time.Second))
				any = true
			}
		case "location":
			any = true // it has an alert; just not one that can ring here
		}
	}
	if any || !hasDue {
		return out
	}
	if !allDay {
		return []time.Time{due}
	}
	if allDayAt == "" {
		return nil
	}
	hm, err := time.Parse("15:04", allDayAt)
	if err != nil {
		return nil
	}
	y, m, d := due.Date()
	return []time.Time{time.Date(y, m, d, hm.Hour(), hm.Minute(), 0, 0, time.Local)}
}

func ringKey(r Reminder, at time.Time) string { return r.ID + "@" + at.UTC().Format(time.RFC3339) }

// dueRings: what should ring now and hasn't. It also prunes `done` of
// anything over a day old.
func dueRings(snap Snapshot, done map[string]time.Time, now time.Time, allDayAt string) []ring {
	for k, at := range done {
		if now.Sub(at) > 24*time.Hour {
			delete(done, k)
		}
	}
	var out []ring
	for _, r := range snap.Reminders {
		for _, at := range ringTimes(r, allDayAt) {
			if at.After(now) || now.Sub(at) > lateLimit {
				continue
			}
			k := ringKey(r, at)
			if _, seen := done[k]; seen {
				continue
			}
			done[k] = at
			out = append(out, ring{r, at})
		}
	}
	return out
}

func (e *Engine) notifyLoop(ctx context.Context) {
	if !e.cfg.notifyOn() {
		return
	}
	done := map[string]time.Time{}
	if readJSON(notifiedPath(), &done) != nil {
		// First run: don't ring for everything in the last hour at once.
		e.mu.Lock()
		dueRings(e.snap, done, e.now(), e.cfg.allDayAt())
		e.mu.Unlock()
		writeJSON(notifiedPath(), done)
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		e.mu.Lock()
		rings := dueRings(e.snap, done, e.now(), e.cfg.allDayAt())
		lists := map[string]string{}
		for _, l := range e.snap.Lists {
			lists[l.ID] = l.Title
		}
		e.mu.Unlock()
		if len(rings) > 0 {
			writeJSON(notifiedPath(), done)
			for _, r := range rings {
				e.notify(ctx, r, lists[r.Reminder.List])
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func describeDue(r Reminder, now time.Time) string {
	t, allDay, ok := dueTime(r.Due)
	if !ok {
		return ""
	}
	if allDay {
		y1, m1, d1 := t.Date()
		y2, m2, d2 := now.Date()
		if y1 == y2 && m1 == m2 && d1 == d2 {
			return "Today"
		}
		return t.Format("Mon Jan 2")
	}
	return t.Local().Format("3:04 PM")
}

func (e *Engine) notify(ctx context.Context, rg ring, list string) {
	r := rg.Reminder
	var body []string
	if d := describeDue(r, e.now()); d != "" {
		body = append(body, d)
	}
	if r.Notes != nil && *r.Notes != "" {
		body = append(body, strings.SplitN(*r.Notes, "\n", 2)[0])
	}
	if list != "" {
		body = append(body, list)
	}
	args := []string{"-a", "Reminders", "-i", "reminders", "-u", "normal",
		"-A", "done=Done", "-A", "later=In 1 hour"}
	if len(e.cfg.Notify.Open) > 0 {
		args = append(args, "-A", "open=Open")
	}
	args = append(args, "--wait", r.Title, strings.Join(body, "\n"))
	go func() {
		out, err := exec.CommandContext(ctx, notifyCommand(), args...).Output()
		if err != nil {
			fmt.Fprintln(os.Stderr, "notify:", err)
			return
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		switch strings.TrimSpace(string(out)) {
		case "done":
			if _, err := e.Call(cctx, map[string]any{"op": "complete", "id": r.ID}); err != nil {
				fmt.Fprintln(os.Stderr, "done from notification:", err)
			}
		case "later":
			// Another alarm an hour from now, beside the ones it has.
			alarms := []map[string]any{{"type": "absolute", "at": e.now().Add(time.Hour).Format(time.RFC3339)}}
			for _, a := range r.Alarms {
				switch a.Type {
				case "absolute":
					alarms = append(alarms, map[string]any{"type": "absolute", "at": a.At})
				case "relative":
					alarms = append(alarms, map[string]any{"type": "relative", "offset": *a.Offset})
				}
			}
			if _, err := e.Call(cctx, map[string]any{"op": "update", "id": r.ID, "alarms": alarms}); err != nil {
				fmt.Fprintln(os.Stderr, "later from notification:", err)
			}
		case "open":
			cmd := make([]string, len(e.cfg.Notify.Open))
			for i, a := range e.cfg.Notify.Open {
				cmd[i] = strings.ReplaceAll(a, "{id}", r.ID)
			}
			exec.Command(cmd[0], cmd[1:]...).Start()
		}
	}()
}

func notifyCommand() string {
	if c := os.Getenv("REMINDERS_NOTIFY"); c != "" {
		return c
	}
	return "notify-send"
}
