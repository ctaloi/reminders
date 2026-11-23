// reminders: Apple Reminders on Linux, through Reminders Bridge on a Mac.
//
//	reminders serve        keep the connection, the cache and the notifications (the user service)
//	reminders list …       what's open (from the cache, so it works offline)
//	reminders add …        and done, edit, delete: through the service
//
// Readers (a window, a bar widget) watch ~/.cache/reminders/state.json,
// written after every change, and read reminders.json beside it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "status":
		err = statusCmd(args)
	case "lists":
		err = listsCmd(args)
	case "list", "ls":
		err = listCmd(args)
	case "show":
		err = showCmd(args)
	case "add":
		err = addCmd(args)
	case "done", "undone":
		err = doneCmd(args, cmd == "done")
	case "edit":
		err = editCmd(args)
	case "delete", "rm":
		err = deleteCmd(args)
	case "refresh":
		_, err = call(map[string]any{"op": "refresh"})
	case "version", "--version":
		fmt.Println("reminders", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reminders:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `reminders: Apple Reminders on Linux, through Reminders Bridge on a Mac

  serve                        the service: connection, cache, notifications
  status [--json]              the connection, and how many are open, due today, overdue
  lists [--json]               the lists
  list [LIST] [--all] [--json] what's open (--all: and what was finished lately)
  show REF [--json]            one reminder, everything about it
  add TITLE [--list L] [--due WHEN] [--notes N] [--url U] [--priority high|medium|low]
            [--repeat daily|weekly|monthly|yearly] [--json]
  done REF | undone REF
  edit REF [--title T] [--due WHEN|none] [--notes N|none] [--url U|none] [--list L]
           [--priority P|none] [--repeat R|none] [--json]
  delete REF
  refresh                      ask the Mac for everything now

REF is an id, the start of one (4+ characters), or a title.
WHEN is like: today, tomorrow 9am, fri 3pm, nov 28, 11/28 5pm, in 2h, 2026-12-01.
The Mac is "host" in ~/.config/reminders/config.toml (or $REMINDERS_HOST).
`)
}

// args is a small flag reader: flags anywhere, values after them.
type args struct {
	pos   []string
	flags map[string]string
	bools map[string]bool
}

func parseArgs(in []string, boolFlags ...string) (args, error) {
	a := args{flags: map[string]string{}, bools: map[string]bool{}}
	isBool := map[string]bool{"json": true}
	for _, b := range boolFlags {
		isBool[b] = true
	}
	for i := 0; i < len(in); i++ {
		s := in[i]
		if s == "--" {
			a.pos = append(a.pos, in[i+1:]...)
			break
		}
		if !strings.HasPrefix(s, "--") || len(s) < 3 {
			a.pos = append(a.pos, s)
			continue
		}
		name, val, hasVal := strings.Cut(s[2:], "=")
		if isBool[name] {
			a.bools[name] = true
			continue
		}
		if !hasVal {
			if i+1 >= len(in) {
				return a, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = in[i]
		}
		a.flags[name] = val
	}
	return a, nil
}

func (a args) has(k string) bool { _, ok := a.flags[k]; return ok }

func serve() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	e := newEngine(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	closeSocket, err := e.listen(ctx)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer closeSocket()
	fmt.Fprintf(os.Stderr, "reminders %s: reaching %s\n", version, cfg.Host)
	e.Run(ctx)
	return nil
}

// call goes through the service, or straight to the Mac when the service
// isn't running.
func call(req map[string]any) (json.RawMessage, error) {
	res, err := callService(req, 90*time.Second)
	if errors.Is(err, errNoService) {
		return callDirect(req)
	}
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, &BridgeError{Code: res.Code, Message: res.Error}
	}
	return res.Result, nil
}

func callDirect(req map[string]any) (json.RawMessage, error) {
	if req["op"] == "refresh" {
		return nil, errNoService
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	br, err := Dial(cfg.bridgeCommand())
	if err != nil {
		return nil, err
	}
	defer br.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return br.Call(ctx, req)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func loadSnapshot() (Snapshot, State, error) {
	var snap Snapshot
	var st State
	_ = readJSON(statePath(), &st)
	if err := readJSON(snapshotPath(), &snap); err != nil {
		if os.IsNotExist(err) {
			return snap, st, errors.New("nothing synced yet: is the service running? (systemctl --user status reminders)")
		}
		return snap, st, err
	}
	return snap, st, nil
}

func statusCmd(in []string) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	var st State
	if res, err := callServiceState(); err == nil {
		st = res
	} else if readJSON(statePath(), &st) != nil {
		return errors.New("the service isn't running and has never synced (systemctl --user start reminders)")
	}
	snap, _, _ := loadSnapshot()
	st.Counts.Open, st.Counts.Overdue, st.Counts.Today = counts(snap, time.Now())
	if a.bools["json"] {
		return printJSON(st)
	}
	if st.Connected {
		fmt.Printf("Connected to %s (bridge %s)\n", st.Host, st.Bridge)
	} else {
		fmt.Printf("Not connected to %s\n", st.Host)
	}
	if st.Error != "" {
		fmt.Println("  " + st.Error)
	}
	if !st.SyncedAt.IsZero() {
		fmt.Printf("Synced %s ago\n", time.Since(st.SyncedAt).Round(time.Second))
	}
	fmt.Printf("%d open, %d due today, %d overdue\n", st.Counts.Open, st.Counts.Today, st.Counts.Overdue)
	return nil
}

func listsCmd(in []string) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	snap, _, err := loadSnapshot()
	if err != nil {
		return err
	}
	if a.bools["json"] {
		return printJSON(snap.Lists)
	}
	open := map[string]int{}
	for _, r := range snap.Reminders {
		if !r.Completed {
			open[r.List]++
		}
	}
	for _, l := range snap.Lists {
		mark := ""
		if l.Default {
			mark = "  (default)"
		}
		fmt.Printf("%-28s %3d open%s\n", l.Title, open[l.ID], mark)
	}
	return nil
}

func findList(snap Snapshot, name string) (List, error) {
	var hits []List
	for _, l := range snap.Lists {
		if l.ID == name || strings.EqualFold(l.Title, name) {
			return l, nil
		}
		if strings.HasPrefix(strings.ToLower(l.Title), strings.ToLower(name)) {
			hits = append(hits, l)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return List{}, fmt.Errorf("%q could be more than one list", name)
	}
	return List{}, fmt.Errorf("no list called %q", name)
}

// findReminder: an id, the start of one, or a title (open ones first).
func findReminder(snap Snapshot, ref string) (Reminder, error) {
	ref = strings.TrimSpace(ref)
	for _, r := range snap.Reminders {
		if r.ID == ref {
			return r, nil
		}
	}
	var hits []Reminder
	if len(ref) >= 4 {
		for _, r := range snap.Reminders {
			if strings.HasPrefix(strings.ToLower(r.ID), strings.ToLower(ref)) {
				hits = append(hits, r)
			}
		}
	}
	if len(hits) == 0 {
		for _, done := range []bool{false, true} {
			for _, r := range snap.Reminders {
				if r.Completed == done && strings.EqualFold(r.Title, ref) {
					hits = append(hits, r)
				}
			}
			if len(hits) > 0 {
				break
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Reminder{}, fmt.Errorf("no reminder %q", ref)
	default:
		return Reminder{}, fmt.Errorf("%q matches %d reminders; use its id (reminders list)", ref, len(hits))
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return strings.ToLower(id[:8])
	}
	return strings.ToLower(id)
}

func priorityName(p int) string {
	switch {
	case p == 0:
		return ""
	case p <= 4:
		return "!!!"
	case p == 5:
		return "!!"
	default:
		return "!"
	}
}

func priorityValue(s string) (any, error) {
	switch strings.ToLower(s) {
	case "high", "!!!", "1":
		return 1, nil
	case "medium", "med", "!!", "5":
		return 5, nil
	case "low", "!", "9":
		return 9, nil
	case "none", "0", "":
		return nil, nil
	}
	return nil, fmt.Errorf("priority is high, medium, low or none")
}

func formatDue(r Reminder, now time.Time) string {
	t, allDay, ok := dueTime(r.Due)
	if !ok {
		return ""
	}
	t = t.Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	var d string
	switch days := int(day.Sub(today).Hours() / 24); {
	case days == 0:
		d = "today"
	case days == 1:
		d = "tomorrow"
	case days == -1:
		d = "yesterday"
	case days > 1 && days < 7:
		d = t.Format("Mon")
	case t.Year() == now.Year():
		d = t.Format("Mon Jan 2")
	default:
		d = t.Format("Jan 2 2006")
	}
	if !allDay {
		d += " " + strings.ToLower(t.Format("3:04PM"))
	}
	if !r.Completed && ((allDay && day.Before(today)) || (!allDay && t.Before(now))) {
		d += " (overdue)"
	}
	return d
}

func listCmd(in []string) error {
	a, err := parseArgs(in, "all")
	if err != nil {
		return err
	}
	snap, st, err := loadSnapshot()
	if err != nil {
		return err
	}
	var only *List
	if len(a.pos) > 0 {
		l, err := findList(snap, strings.Join(a.pos, " "))
		if err != nil {
			return err
		}
		only = &l
	}
	var out []Reminder
	for _, r := range snap.Reminders {
		if (only == nil || r.List == only.ID) && (a.bools["all"] || !r.Completed) {
			out = append(out, r)
		}
	}
	if a.bools["json"] {
		if out == nil {
			out = []Reminder{}
		}
		return printJSON(out)
	}
	now := time.Now()
	for _, l := range snap.Lists {
		var mine []Reminder
		for _, r := range out {
			if r.List == l.ID {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			continue
		}
		fmt.Println(l.Title)
		for _, r := range mine {
			box := "○"
			if r.Completed {
				box = "●"
			}
			line := fmt.Sprintf("  %s %s  %s", shortID(r.ID), box, r.Title)
			if p := priorityName(r.Priority); p != "" {
				line += " " + p
			}
			if d := formatDue(r, now); d != "" {
				line += "  · " + d
			}
			if len(r.Recurrence) > 0 {
				line += " ↻"
			}
			fmt.Println(line)
		}
	}
	if !st.Connected && st.Host != "" {
		fmt.Fprintf(os.Stderr, "(not connected to %s; as of %s)\n", st.Host, snap.SyncedAt.Local().Format("Jan 2 3:04 PM"))
	}
	return nil
}

func showCmd(in []string) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	snap, _, err := loadSnapshot()
	if err != nil {
		return err
	}
	r, err := findReminder(snap, strings.Join(a.pos, " "))
	if err != nil {
		return err
	}
	if a.bools["json"] {
		return printJSON(r)
	}
	listName := r.List
	for _, l := range snap.Lists {
		if l.ID == r.List {
			listName = l.Title
		}
	}
	fmt.Println(r.Title)
	row := func(k, v string) {
		if v != "" {
			fmt.Printf("  %-10s %s\n", k, v)
		}
	}
	row("list", listName)
	row("due", formatDue(r, time.Now()))
	if p := priorityName(r.Priority); p != "" {
		row("priority", map[string]string{"!!!": "high", "!!": "medium", "!": "low"}[p])
	}
	for _, rr := range r.Recurrence {
		row("repeats", fmt.Sprintf("%v every %v", rr["frequency"], rr["interval"]))
	}
	for _, al := range r.Alarms {
		switch al.Type {
		case "absolute":
			t, _ := time.Parse(time.RFC3339, al.At)
			row("alert", t.Local().Format("Mon Jan 2 3:04 PM"))
		case "relative":
			row("alert", fmt.Sprintf("%v before due", (-time.Duration(*al.Offset) * time.Second).String()))
		case "location":
			row("alert", fmt.Sprintf("on %s %s (location; rings on the phone)", map[string]string{"arrive": "arriving at", "leave": "leaving"}[al.Proximity], al.Title))
		}
	}
	if r.URL != nil {
		row("url", *r.URL)
	}
	if r.Notes != nil {
		row("notes", strings.ReplaceAll(*r.Notes, "\n", "\n             "))
	}
	if r.Completed && r.CompletedAt != nil {
		row("done", *r.CompletedAt)
	}
	row("id", r.ID)
	return nil
}

func repeatValue(s string) ([]map[string]any, error) {
	switch strings.ToLower(s) {
	case "none", "never", "":
		return []map[string]any{}, nil
	case "daily", "weekly", "monthly", "yearly":
		return []map[string]any{{"frequency": strings.ToLower(s), "interval": 1}}, nil
	}
	return nil, fmt.Errorf("repeat is daily, weekly, monthly, yearly or none")
}

// fields turns the shared flags of add and edit into a bridge request.
func fields(a args, snap Snapshot, req map[string]any) error {
	now := time.Now()
	nullable := func(flag, key string) {
		if v, ok := a.flags[flag]; ok {
			if v == "none" || v == "" {
				req[key] = nil
			} else {
				req[key] = v
			}
		}
	}
	if v, ok := a.flags["title"]; ok {
		req["title"] = v
	}
	nullable("notes", "notes")
	nullable("url", "url")
	if v, ok := a.flags["due"]; ok {
		if v == "none" || v == "" {
			req["due"] = nil
		} else {
			w, err := parseWhen(v, now)
			if err != nil {
				return err
			}
			req["due"] = w
		}
	}
	if v, ok := a.flags["priority"]; ok {
		p, err := priorityValue(v)
		if err != nil {
			return err
		}
		req["priority"] = p
	}
	if v, ok := a.flags["repeat"]; ok {
		r, err := repeatValue(v)
		if err != nil {
			return err
		}
		req["recurrence"] = r
	}
	if v, ok := a.flags["list"]; ok {
		l, err := findList(snap, v)
		if err != nil {
			return err
		}
		req["list"] = l.ID
	}
	return nil
}

func printReminder(raw json.RawMessage, asJSON bool, verb string) error {
	var r Reminder
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if asJSON {
		return printJSON(r)
	}
	line := fmt.Sprintf("%s %s: %s", verb, shortID(r.ID), r.Title)
	if d := formatDue(r, time.Now()); d != "" {
		line += " · " + d
	}
	fmt.Println(line)
	return nil
}

func addCmd(in []string) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(strings.Join(a.pos, " "))
	if title == "" {
		return errors.New("add what? (reminders add Buy milk --due tomorrow)")
	}
	snap, _, _ := loadSnapshot()
	req := map[string]any{"op": "create", "title": title}
	if err := fields(a, snap, req); err != nil {
		return err
	}
	raw, err := call(req)
	if err != nil {
		return err
	}
	return printReminder(raw, a.bools["json"], "Added")
}

func doneCmd(in []string, done bool) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	snap, _, err := loadSnapshot()
	if err != nil {
		return err
	}
	r, err := findReminder(snap, strings.Join(a.pos, " "))
	if err != nil {
		return err
	}
	raw, err := call(map[string]any{"op": "complete", "id": r.ID, "completed": done})
	if err != nil {
		return err
	}
	if a.bools["json"] {
		return printReminder(raw, true, "")
	}
	var after Reminder
	json.Unmarshal(raw, &after)
	switch {
	case !done:
		fmt.Printf("Not done %s: %s\n", shortID(r.ID), r.Title)
	case len(r.Recurrence) > 0 && !after.Completed:
		// A repeating one moves on to its next date rather than finishing.
		fmt.Printf("Done %s: %s; next: %s\n", shortID(r.ID), r.Title, formatDue(after, time.Now()))
	default:
		fmt.Printf("Done %s: %s\n", shortID(r.ID), r.Title)
	}
	return nil
}

func editCmd(in []string) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	snap, _, err := loadSnapshot()
	if err != nil {
		return err
	}
	r, err := findReminder(snap, strings.Join(a.pos, " "))
	if err != nil {
		return err
	}
	req := map[string]any{"op": "update", "id": r.ID}
	if r.ModifiedAt != nil {
		req["ifModified"] = *r.ModifiedAt
	}
	if err := fields(a, snap, req); err != nil {
		return err
	}
	if len(req) <= 3 {
		return errors.New("change what? (--title, --due, --notes, --url, --list, --priority, --repeat)")
	}
	raw, err := call(req)
	if errCode(err) == "conflict" {
		return errors.New("it changed somewhere else since the last sync; run reminders refresh and try again")
	}
	if err != nil {
		return err
	}
	return printReminder(raw, a.bools["json"], "Changed")
}

func deleteCmd(in []string) error {
	a, err := parseArgs(in)
	if err != nil {
		return err
	}
	snap, _, err := loadSnapshot()
	if err != nil {
		return err
	}
	r, err := findReminder(snap, strings.Join(a.pos, " "))
	if err != nil {
		return err
	}
	if _, err := call(map[string]any{"op": "delete", "id": r.ID}); err != nil {
		return err
	}
	fmt.Printf("Deleted %s: %s\n", shortID(r.ID), r.Title)
	return nil
}
