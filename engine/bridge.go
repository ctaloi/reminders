package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Bridge is one connection to Reminders Bridge on the Mac: a process (ssh,
// normally) whose stdin and stdout carry one JSON object per line. Calls
// carry a "seq" that their answer repeats; lines with "event" are the
// bridge's own news ("changed").

type BridgeError struct {
	Code    string // access, notFound, conflict, bad, or "" for the line itself
	Message string
}

func (e *BridgeError) Error() string { return e.Message }

func errCode(err error) string {
	var be *BridgeError
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

type answer struct {
	Seq    *int64          `json:"seq"`
	Event  string          `json:"event"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Code   string          `json:"code"`
}

type Bridge struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	seq     int64
	waiting map[int64]chan answer
	stderr  tail
	Changed chan struct{} // a value whenever the bridge says Reminders changed
	Done    chan struct{} // closed when the connection is gone
	err     error
}

// tail keeps the end of what the process says on stderr, for the reason a
// connection failed (ssh's "Connection refused", the relay's "isn't running").
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func Dial(argv []string) (*Bridge, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to reach the bridge")
	}
	b := &Bridge{
		cmd:     exec.Command(argv[0], argv[1:]...),
		waiting: map[int64]chan answer{},
		Changed: make(chan struct{}, 1),
		Done:    make(chan struct{}),
	}
	b.cmd.Stderr = &b.stderr
	var err error
	if b.stdin, err = b.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := b.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := b.cmd.Start(); err != nil {
		return nil, err
	}
	go b.read(out)
	return b, nil
}

func (b *Bridge) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var a answer
		if json.Unmarshal(line, &a) != nil {
			continue
		}
		if a.Event == "changed" {
			select {
			case b.Changed <- struct{}{}:
			default:
			}
			continue
		}
		if a.Seq == nil {
			continue
		}
		b.mu.Lock()
		ch := b.waiting[*a.Seq]
		delete(b.waiting, *a.Seq)
		b.mu.Unlock()
		if ch != nil {
			ch <- a
		}
	}
	b.cmd.Wait()
	b.mu.Lock()
	why := b.stderr.String()
	if why == "" {
		why = "the connection to the Mac closed"
	}
	b.err = errors.New(why)
	b.mu.Unlock()
	close(b.Done)
}

// Err is why the connection ended, once Done is closed.
func (b *Bridge) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// Call sends one request and waits for its answer.
func (b *Bridge) Call(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	b.mu.Lock()
	if b.err != nil {
		b.mu.Unlock()
		return nil, b.err
	}
	b.seq++
	seq := b.seq
	ch := make(chan answer, 1)
	b.waiting[seq] = ch
	msg := map[string]any{}
	for k, v := range req {
		msg[k] = v
	}
	msg["seq"] = seq
	line, _ := json.Marshal(msg)
	_, err := b.stdin.Write(append(line, '\n'))
	b.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("writing to the bridge: %w", err)
	}
	select {
	case a := <-ch:
		if !a.OK {
			return nil, &BridgeError{Code: a.Code, Message: a.Error}
		}
		return a.Result, nil
	case <-b.Done:
		return nil, b.Err()
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.waiting, seq)
		b.mu.Unlock()
		return nil, fmt.Errorf("the Mac didn't answer in time")
	}
}

func (b *Bridge) Close() {
	b.stdin.Close()
	if b.cmd.Process != nil {
		b.cmd.Process.Kill()
	}
	<-b.Done
}
