package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// Engine is the running service: one connection to the bridge, kept up;
// the cache, rewritten whenever Reminders changes; notifications; and a
// socket where the window and the CLI send their calls.

type Engine struct {
	cfg  Config
	mu   sync.Mutex
	// One sync at a time: a write's own and the one the bridge's
	// "changed" sets off would otherwise race on the same files.
	syncMu sync.Mutex
	// One state.json write at a time, in the order the changes happened.
	stateMu sync.Mutex
	br   *Bridge
	st   State
	snap Snapshot
	kick chan struct{}
	now  func() time.Time
}

func newEngine(cfg Config) *Engine {
	e := &Engine{cfg: cfg, kick: make(chan struct{}, 1), now: time.Now}
	e.st.Host = cfg.Host
	_ = readJSON(snapshotPath(), &e.snap) // what's known from last time, until the Mac answers
	return e
}

const pollEvery = 5 * time.Minute

func (e *Engine) Run(ctx context.Context) {
	go e.notifyLoop(ctx)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		err := e.session(ctx)
		if ctx.Err() != nil {
			return
		}
		e.setState(func(s *State) {
			s.Connected = false
			if err != nil {
				s.Error = err.Error()
			}
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "bridge:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
			backoff = 2 * time.Second
		case <-time.After(backoff):
			backoff = min(backoff*2, time.Minute)
		}
	}
}

// session is one connection, from dial to drop.
func (e *Engine) session(ctx context.Context) error {
	br, err := Dial(e.cfg.bridgeCommand())
	if err != nil {
		return err
	}
	defer br.Close()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	raw, err := br.Call(wctx, map[string]any{"op": "watch"})
	cancel()
	if err != nil {
		return err
	}
	var hello struct {
		Agent         string `json:"agent"`
		Authorization string `json:"authorization"`
	}
	_ = json.Unmarshal(raw, &hello)
	e.mu.Lock()
	e.br = br
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.br = nil
		e.mu.Unlock()
	}()
	e.setState(func(s *State) {
		s.Connected = true
		s.Error = ""
		s.Bridge = hello.Agent
		s.Authorization = hello.Authorization
	})
	if hello.Authorization != "full" {
		e.setState(func(s *State) {
			s.Error = "The Mac hasn't given Reminders Bridge access to Reminders (" + hello.Authorization + ")."
		})
	}
	fmt.Fprintf(os.Stderr, "connected to %s (bridge %s, access %s)\n", e.cfg.Host, hello.Agent, hello.Authorization)

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		if err := e.sync(ctx, br); err != nil {
			fmt.Fprintln(os.Stderr, "sync:", err)
			e.setState(func(s *State) { s.Error = err.Error() })
		}
		select {
		case <-ctx.Done():
			return nil
		case <-br.Done:
			return br.Err()
		case <-br.Changed:
			// Changes come in bursts; let them settle.
			time.Sleep(300 * time.Millisecond)
			select {
			case <-br.Changed:
			default:
			}
		case <-e.kick:
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			br.Call(rctx, map[string]any{"op": "refresh"})
			cancel()
		case <-tick.C:
		}
	}
}

func (e *Engine) sync(ctx context.Context, br *Bridge) error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	raw, err := br.Call(ctx, map[string]any{"op": "lists"})
	if err != nil {
		return err
	}
	var lists []List
	if err := json.Unmarshal(raw, &lists); err != nil {
		return err
	}
	raw, err = br.Call(ctx, map[string]any{"op": "reminders", "completedDays": e.cfg.completedDays()})
	if err != nil {
		return err
	}
	var rems []Reminder
	if err := json.Unmarshal(raw, &rems); err != nil {
		return err
	}
	sortReminders(rems)
	snap := Snapshot{SyncedAt: e.now(), Lists: lists, Reminders: rems}
	if err := writeJSON(snapshotPath(), snap); err != nil {
		return err
	}
	e.mu.Lock()
	e.snap = snap
	e.mu.Unlock()
	e.setState(func(s *State) {
		s.SyncedAt = snap.SyncedAt
		if s.Authorization == "full" {
			s.Error = ""
		}
	})
	return nil
}

// sortReminders: open before done; by due date (none last); then by when made.
func sortReminders(rs []Reminder) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Completed != b.Completed {
			return !a.Completed
		}
		ta, _, oka := dueTime(a.Due)
		tb, _, okb := dueTime(b.Due)
		if oka != okb {
			return oka
		}
		if oka && !ta.Equal(tb) {
			return ta.Before(tb)
		}
		ca, cb := "", ""
		if a.CreatedAt != nil {
			ca = *a.CreatedAt
		}
		if b.CreatedAt != nil {
			cb = *b.CreatedAt
		}
		return ca < cb
	})
}

func counts(snap Snapshot, now time.Time) (open, overdue, today int) {
	y, m, d := now.Date()
	endOfDay := time.Date(y, m, d+1, 0, 0, 0, 0, time.Local)
	startOfDay := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	for _, r := range snap.Reminders {
		if r.Completed {
			continue
		}
		open++
		t, allDay, ok := dueTime(r.Due)
		if !ok {
			continue
		}
		if (allDay && t.Before(startOfDay)) || (!allDay && t.Before(now)) {
			overdue++
		} else if t.Before(endOfDay) {
			today++
		}
	}
	return
}

func (e *Engine) setState(f func(*State)) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.mu.Lock()
	f(&e.st)
	e.st.Counts.Open, e.st.Counts.Overdue, e.st.Counts.Today = counts(e.snap, e.now())
	st := e.st
	e.mu.Unlock()
	if err := writeJSON(statePath(), st); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
	}
}

// Call goes to the bridge over the open connection. A write is followed by
// a sync, so the cache has it before the answer goes back.
func (e *Engine) Call(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	e.mu.Lock()
	br := e.br
	e.mu.Unlock()
	if br == nil {
		msg := "Not connected to " + e.cfg.Host
		e.mu.Lock()
		if e.st.Error != "" {
			msg += ": " + e.st.Error
		}
		e.mu.Unlock()
		return nil, &BridgeError{Code: "offline", Message: msg}
	}
	raw, err := br.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	switch req["op"] {
	case "create", "update", "complete", "delete":
		if serr := e.sync(ctx, br); serr != nil {
			fmt.Fprintln(os.Stderr, "sync after write:", serr)
		}
	}
	return raw, nil
}

var allowedOps = map[string]bool{"status": true, "lists": true, "reminders": true, "get": true,
	"create": true, "update": true, "complete": true, "delete": true, "refresh": true}

type callResult struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// listen serves the socket: POST /call with a bridge request; GET /state.
func (e *Engine) listen(ctx context.Context) (func(), error) {
	path := socketPath()
	if err := os.MkdirAll(cacheDir(), 0o700); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		op, _ := req["op"].(string)
		var res callResult
		if !allowedOps[op] {
			res = callResult{Code: "bad", Error: "no such op: " + op}
		} else if op == "refresh" {
			e.Refresh()
			res = callResult{OK: true, Result: json.RawMessage("true")}
		} else {
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			raw, err := e.Call(cctx, req)
			cancel()
			if err != nil {
				res = callResult{Code: errCode(err), Error: err.Error()}
			} else {
				res = callResult{OK: true, Result: raw}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		st := e.st
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	return func() { srv.Close(); os.Remove(path) }, nil
}

// Refresh asks for a sync now (and a reconnect now, if the Mac was away).
func (e *Engine) Refresh() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

var errNoService = errors.New("the reminders service isn't running")

// callService sends a call to the running service's socket.
func callService(req map[string]any, timeout time.Duration) (callResult, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath())
		}},
	}
	body, _ := json.Marshal(req)
	resp, err := client.Post("http://reminders/call", "application/json", bytesReader(body))
	if err != nil {
		var ne *net.OpError
		if errors.As(err, &ne) {
			return callResult{}, errNoService
		}
		return callResult{}, err
	}
	defer resp.Body.Close()
	var res callResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return callResult{}, fmt.Errorf("the service's answer: %w", err)
	}
	return res, nil
}
