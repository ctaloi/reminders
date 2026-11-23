package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// callServiceState is the running service's state, fresher than the file.
func callServiceState() (State, error) {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath())
		}},
	}
	var st State
	resp, err := client.Get("http://reminders/state")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	return st, json.NewDecoder(resp.Body).Decode(&st)
}
