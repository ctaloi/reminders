package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake bridge: run with REMINDERS_FAKE_BRIDGE=1
// it speaks the bridge's protocol on stdin and stdout, over a made-up
// store, the way `ssh mac reminders-bridge relay` would.
func TestMain(m *testing.M) {
	if os.Getenv("REMINDERS_FAKE_BRIDGE") == "1" {
		fakeBridge()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeBridge() {
	lists := []map[string]any{
		{"id": "L1", "title": "Reminders", "color": "#1badf8", "source": "iCloud", "writable": true, "default": true},
		{"id": "L2", "title": "Groceries", "color": "#0088ff", "source": "iCloud", "writable": true, "default": false},
	}
	stamp := "2026-11-20T10:00:00.000-05:00"
	rems := map[string]map[string]any{
		"AAAA1111": {"id": "AAAA1111", "list": "L1", "title": "Pay the plow guy", "priority": 0, "completed": false,
			"due": "2026-11-24", "alarms": []any{}, "recurrence": []any{}, "modifiedAt": stamp, "createdAt": stamp},
		"BBBB2222": {"id": "BBBB2222", "list": "L2", "title": "Milk", "priority": 1, "completed": false,
			"due": nil, "alarms": []any{}, "recurrence": []any{}, "modifiedAt": stamp, "createdAt": stamp},
	}
	n := 0
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req map[string]any
		json.Unmarshal(sc.Bytes(), &req)
		reply := map[string]any{"seq": req["seq"], "ok": true}
		changed := false
		switch req["op"] {
		case "watch", "status":
			reply["result"] = map[string]any{"agent": "fake", "authorization": "full"}
		case "lists":
			reply["result"] = lists
		case "reminders":
			var all []any
			for _, r := range rems {
				all = append(all, r)
			}
			reply["result"] = all
		case "create":
			n++
			id := fmt.Sprintf("NEW%05d", n)
			r := map[string]any{"id": id, "list": "L1", "priority": 0, "completed": false, "alarms": []any{},
				"recurrence": []any{}, "modifiedAt": stamp, "createdAt": stamp, "due": nil}
			for k, v := range req {
				if k != "op" && k != "seq" {
					r[k] = v
				}
			}
			rems[id] = r
			reply["result"] = r
			changed = true
		case "update", "complete", "delete":
			id, _ := req["id"].(string)
			r := rems[id]
			if r == nil {
				reply = map[string]any{"seq": req["seq"], "ok": false, "code": "notFound", "error": "gone"}
				break
			}
			if m, ok := req["ifModified"].(string); ok && m != r["modifiedAt"] {
				reply = map[string]any{"seq": req["seq"], "ok": false, "code": "conflict", "error": "changed"}
				break
			}
			switch req["op"] {
			case "delete":
				delete(rems, id)
				reply["result"] = true
			case "complete":
				c, ok := req["completed"].(bool)
				r["completed"] = c || !ok
				reply["result"] = r
			default:
				for k, v := range req {
					if k != "op" && k != "seq" && k != "id" && k != "ifModified" {
						r[k] = v
					}
				}
				r["modifiedAt"] = "2026-11-21T10:00:00.000-05:00"
				reply["result"] = r
			}
			changed = true
		default:
			reply = map[string]any{"seq": req["seq"], "ok": false, "code": "bad", "error": "no such op"}
		}
		out.Encode(reply)
		if changed {
			out.Encode(map[string]any{"event": "changed"})
		}
	}
}

func useFake(t *testing.T) {
	t.Helper()
	t.Setenv("REMINDERS_FAKE_BRIDGE", "1")
	t.Setenv("REMINDERS_BRIDGE_CMD", os.Args[0])
	t.Setenv("REMINDERS_CACHE", t.TempDir())
	t.Setenv("REMINDERS_CONFIG", "/nonexistent")
	t.Setenv("REMINDERS_HOST", "fake")
}

func TestBridgeCallsAndEvents(t *testing.T) {
	useFake(t)
	cfg, _ := loadConfig()
	br, err := Dial(cfg.bridgeCommand())
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	ctx := context.Background()
	raw, err := br.Call(ctx, map[string]any{"op": "create", "title": "Bins out", "id": "not-a-seq"})
	if err != nil {
		t.Fatal(err)
	}
	var r Reminder
	json.Unmarshal(raw, &r)
	if r.Title != "Bins out" {
		t.Fatalf("created %+v", r)
	}
	select {
	case <-br.Changed:
	case <-time.After(2 * time.Second):
		t.Fatal("no changed event")
	}
	_, err = br.Call(ctx, map[string]any{"op": "update", "id": "AAAA1111", "ifModified": "2020-01-01T00:00:00.000Z", "title": "x"})
	if errCode(err) != "conflict" {
		t.Fatalf("stale update: %v", err)
	}
	_, err = br.Call(ctx, map[string]any{"op": "delete", "id": "nope"})
	if errCode(err) != "notFound" {
		t.Fatalf("missing delete: %v", err)
	}
}

func TestBridgeGoneReportsWhy(t *testing.T) {
	br, err := Dial([]string{"sh", "-c", "echo 'ssh: connect to host mac port 22: Connection refused' >&2; exit 255"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = br.Call(context.Background(), map[string]any{"op": "status"})
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("want ssh's reason, got %v", err)
	}
}

func TestEngineSyncsAndWrites(t *testing.T) {
	useFake(t)
	cfg, _ := loadConfig()
	f := false
	cfg.Notify.Enabled = &f
	e := newEngine(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, func() bool {
		var st State
		return readJSON(statePath(), &st) == nil && st.Connected && !st.SyncedAt.IsZero()
	})
	snap, st, err := loadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Lists) != 2 || len(snap.Reminders) != 2 || st.Bridge != "fake" {
		t.Fatalf("snapshot %d lists %d reminders, state %+v", len(snap.Lists), len(snap.Reminders), st)
	}
	// Open ones with a due date come first.
	if snap.Reminders[0].ID != "AAAA1111" {
		t.Fatalf("order: %s first", snap.Reminders[0].Title)
	}
	if _, err := e.Call(ctx, map[string]any{"op": "create", "title": "Call the vet"}); err != nil {
		t.Fatal(err)
	}
	snap, _, _ = loadSnapshot()
	if len(snap.Reminders) != 3 {
		t.Fatalf("a write should be in the cache before it answers; have %d", len(snap.Reminders))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func ptr[T any](v T) *T { return &v }

func TestRingTimes(t *testing.T) {
	loc := time.Local
	timed := Reminder{Due: ptr("2026-11-24T15:00:00-05:00")}
	if got := ringTimes(timed, "09:00"); len(got) != 1 || !got[0].Equal(time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("a time with no alarm rings at the time: %v", got)
	}
	allDay := Reminder{Due: ptr("2026-11-24")}
	if got := ringTimes(allDay, "09:00"); len(got) != 1 || !got[0].Equal(time.Date(2026, 11, 24, 9, 0, 0, 0, loc)) {
		t.Fatalf("a day rings at all_day_at: %v", got)
	}
	if got := ringTimes(allDay, ""); len(got) != 0 {
		t.Fatalf("all_day_at empty: never: %v", got)
	}
	rel := Reminder{Due: ptr("2026-11-24T15:00:00-05:00"), Alarms: []Alarm{{Type: "relative", Offset: ptr(-900)}}}
	if got := ringTimes(rel, "09:00"); len(got) != 1 || !got[0].Equal(time.Date(2026, 11, 24, 19, 45, 0, 0, time.UTC)) {
		t.Fatalf("15 minutes before, and only that: %v", got)
	}
	loc2 := Reminder{Due: ptr("2026-11-24T15:00:00-05:00"), Alarms: []Alarm{{Type: "location", Proximity: "arrive"}}}
	if got := ringTimes(loc2, "09:00"); len(got) != 0 {
		t.Fatalf("a location alarm is the phone's: %v", got)
	}
	if got := ringTimes(Reminder{Completed: true, Due: ptr("2026-11-24")}, "09:00"); got != nil {
		t.Fatalf("done rings never: %v", got)
	}
	if got := ringTimes(Reminder{}, "09:00"); len(got) != 0 {
		t.Fatalf("nothing due rings never: %v", got)
	}
}

func TestDueRingsOnceAndNotTooLate(t *testing.T) {
	now := time.Date(2026, 11, 24, 15, 1, 0, 0, time.Local)
	snap := Snapshot{Reminders: []Reminder{
		{ID: "a", Title: "now", Due: ptr(now.Add(-time.Minute).Format(time.RFC3339))},
		{ID: "b", Title: "later", Due: ptr(now.Add(time.Hour).Format(time.RFC3339))},
		{ID: "c", Title: "long ago", Due: ptr(now.Add(-3 * time.Hour).Format(time.RFC3339))},
	}}
	done := map[string]time.Time{}
	if r := dueRings(snap, done, now, "09:00"); len(r) != 1 || r[0].Reminder.ID != "a" {
		t.Fatalf("first pass: %+v", r)
	}
	if r := dueRings(snap, done, now, "09:00"); len(r) != 0 {
		t.Fatalf("second pass should be quiet: %+v", r)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 11, 23, 13, 30, 0, 0, time.Local) // a Monday
	at := func(y int, m time.Month, d, h, mi int) string {
		return time.Date(y, m, d, h, mi, 0, 0, time.Local).Format(time.RFC3339)
	}
	cases := map[string]string{
		"today":           "2026-11-23",
		"tomorrow":        "2026-11-24",
		"tomorrow 9am":    at(2026, 11, 24, 9, 0),
		"fri 3pm":         at(2026, 11, 27, 15, 0),
		"friday at 3:30p": at(2026, 11, 27, 15, 30),
		"mon":             "2026-11-23",
		"next mon":        "2026-11-30",
		"nov 28":          "2026-11-28",
		"jan 5":           "2027-01-05",
		"11/28 5pm":       at(2026, 11, 28, 17, 0),
		"2026-12-01":      "2026-12-01",
		"5pm":             at(2026, 11, 23, 17, 0),
		"9am":             at(2026, 11, 24, 9, 0),
		"noon":            at(2026, 11, 24, 12, 0),
		"in 30m":          at(2026, 11, 23, 14, 0),
		"in 2 days":       "2026-11-25",
	}
	for in, want := range cases {
		got, err := parseWhen(in, now)
		if err != nil || got != want {
			t.Errorf("%q: got %q (%v), want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "someday", "fri sat", "13"} {
		if got, err := parseWhen(bad, now); err == nil {
			t.Errorf("%q should be refused, got %q", bad, got)
		}
	}
}

func TestFindReminder(t *testing.T) {
	snap := Snapshot{Reminders: []Reminder{
		{ID: "ABCD1234-0000", Title: "Milk"},
		{ID: "ABCE9999-0000", Title: "Eggs"},
		{ID: "ZZZZ0000-0000", Title: "Milk", Completed: true},
	}}
	if r, err := findReminder(snap, "abcd"); err != nil || r.Title != "Milk" {
		t.Fatalf("id prefix: %v %v", r, err)
	}
	if _, err := findReminder(snap, "abc"); err == nil {
		t.Fatal("3 characters is too short to be an id, and no title is abc")
	}
	if r, err := findReminder(snap, "milk"); err != nil || r.Completed {
		t.Fatalf("a title finds the open one first: %v %v", r, err)
	}
}
