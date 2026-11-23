package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// What the bridge on the Mac says a list and a reminder are. The engine
// keeps these as they come; readers of the cache see the same shapes.

type List struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Color    string `json:"color"`
	Source   string `json:"source"`
	Writable bool   `json:"writable"`
	Default  bool   `json:"default"`
}

type Alarm struct {
	Type      string  `json:"type"` // absolute, relative, location
	At        string  `json:"at,omitempty"`
	Offset    *int    `json:"offset,omitempty"` // seconds from due; negative is before
	Title     string  `json:"title,omitempty"`
	Proximity string  `json:"proximity,omitempty"` // arrive, leave
	Lat       float64 `json:"lat,omitempty"`
	Lon       float64 `json:"lon,omitempty"`
	Radius    float64 `json:"radius,omitempty"`
}

type Reminder struct {
	ID          string           `json:"id"`
	ExternalID  *string          `json:"externalId"`
	List        string           `json:"list"`
	Title       string           `json:"title"`
	Notes       *string          `json:"notes"`
	URL         *string          `json:"url"`
	Location    string           `json:"location,omitempty"`
	Priority    int              `json:"priority"` // 0 none, 1 high, 5 medium, 9 low
	Completed   bool             `json:"completed"`
	CompletedAt *string          `json:"completedAt"`
	Due         *string          `json:"due"` // "2026-11-24" or RFC 3339
	Alarms      []Alarm          `json:"alarms"`
	Recurrence  []map[string]any `json:"recurrence"`
	CreatedAt   *string          `json:"createdAt"`
	ModifiedAt  *string          `json:"modifiedAt"`
	Extras      map[string]any   `json:"extras"`
}

// Snapshot is reminders.json: everything, as of SyncedAt.
type Snapshot struct {
	SyncedAt  time.Time  `json:"syncedAt"`
	Lists     []List     `json:"lists"`
	Reminders []Reminder `json:"reminders"`
}

// State is state.json, written after every change to the snapshot or the
// connection, so it's the file to watch.
type State struct {
	Host          string    `json:"host"`
	Connected     bool      `json:"connected"`
	Authorization string    `json:"authorization,omitempty"`
	Bridge        string    `json:"bridge,omitempty"`
	Error         string    `json:"error,omitempty"`
	SyncedAt      time.Time `json:"syncedAt,omitzero"`
	Counts        struct {
		Open    int `json:"open"`
		Overdue int `json:"overdue"`
		Today   int `json:"today"`
	} `json:"counts"`
}

type Config struct {
	// The Mac, as ssh knows it.
	Host string `toml:"host"`
	// The whole command that reaches the bridge, if ssh to Host won't do.
	Command []string `toml:"command"`
	// How many days of finished reminders to keep (30).
	CompletedDays *int `toml:"completed_days"`
	Notify struct {
		Enabled *bool `toml:"enabled"`
		// When a reminder due on a day, with no time and no alarm, notifies
		// ("09:00", like Reminders' Today Notification; "" for never).
		AllDayAt *string `toml:"all_day_at"`
		// Run for a notification's Open button; {id} is the reminder's id.
		Open []string `toml:"open"`
	} `toml:"notify"`
}

func (c Config) completedDays() int {
	if c.CompletedDays != nil {
		return *c.CompletedDays
	}
	return 30
}

func (c Config) notifyOn() bool { return c.Notify.Enabled == nil || *c.Notify.Enabled }

func (c Config) allDayAt() string {
	if c.Notify.AllDayAt != nil {
		return *c.Notify.AllDayAt
	}
	return "09:00"
}

func xdg(env, fallback string) string {
	if d := os.Getenv(env); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func cacheDir() string {
	if d := os.Getenv("REMINDERS_CACHE"); d != "" {
		return d
	}
	return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "reminders")
}

func configPath() string {
	if p := os.Getenv("REMINDERS_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "reminders", "config.toml")
}

func snapshotPath() string { return filepath.Join(cacheDir(), "reminders.json") }
func statePath() string    { return filepath.Join(cacheDir(), "state.json") }
func notifiedPath() string { return filepath.Join(cacheDir(), "notified.json") }
func socketPath() string   { return filepath.Join(cacheDir(), "engine.sock") }

func loadConfig() (Config, error) {
	var c Config
	if _, err := os.Stat(configPath()); err == nil {
		if _, err := toml.DecodeFile(configPath(), &c); err != nil {
			return c, err
		}
	}
	if h := os.Getenv("REMINDERS_HOST"); h != "" {
		c.Host = h
	}
	if c.Host == "" {
		c.Host = "mac"
	}
	return c, nil
}

// bridgeCommand is what's run to talk to the bridge: by default ssh to the
// Mac, running the bridge's relay there.
func (c Config) bridgeCommand() []string {
	if s := os.Getenv("REMINDERS_BRIDGE_CMD"); s != "" {
		return strings.Fields(s)
	}
	if len(c.Command) > 0 {
		return c.Command
	}
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", c.Host,
		`"$HOME/Applications/Reminders Bridge.app/Contents/MacOS/reminders-bridge" relay`}
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// dueTime reads a due date. allDay is true for a day without a time, which
// comes back as local midnight.
func dueTime(s *string) (t time.Time, allDay bool, ok bool) {
	if s == nil || *s == "" {
		return time.Time{}, false, false
	}
	if len(*s) == 10 {
		d, err := time.ParseInLocation("2006-01-02", *s, time.Local)
		return d, true, err == nil
	}
	d, err := time.Parse(time.RFC3339, *s)
	return d, false, err == nil
}
