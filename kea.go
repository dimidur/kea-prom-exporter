package main

// Client for Kea's HTTP control socket, and the wire shapes its responses
// decode into -- including the [[value, timestamp], ...] sample encoding.
//
// One reason to change: Kea's control protocol or its JSON encoding.
// What the values *mean* is statmap.go's problem, not this file's.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// keaResponse is the shape of a single element in Kea's response array.
// Kea wraps every command response in an array — one entry per service
// the command was forwarded to.
type keaResponse struct {
	Result    int             `json:"result"`
	Text      string          `json:"text,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type keaClient struct {
	url      string
	user     string
	password string
	service  string
	timeout  time.Duration
	client   *http.Client
}

// defaultKeaTimeout applies when a client is constructed without one. A zero
// duration on context.WithTimeout is a deadline already past, and a negative
// one likewise, so this is not a "no limit" fallback -- it is what stops an
// unset field turning every call into an instant failure.
const defaultKeaTimeout = 5 * time.Second

// effectiveTimeout exists so the fallback is assertable without a test that
// waits for it to elapse.
func effectiveTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultKeaTimeout
	}
	return d
}

// keaCommand is marshalled rather than formatted into a string. The service
// name is operator-supplied, and hand-built JSON would let a stray quote
// change the command actually sent.
type keaCommand struct {
	Command string   `json:"command"`
	Service []string `json:"service"`
}

// errBodyLimit caps how much of an error response is quoted back. Kea
// explains 400/401/403 in the body, and a bare "HTTP 401" is the least
// useful possible message for the most common misconfiguration.
const errBodyLimit = 512

func (k *keaClient) call(ctx context.Context, command string) (*keaResponse, error) {
	service := k.service
	if service == "" {
		service = "dhcp4"
	}
	body, err := json.Marshal(keaCommand{Command: command, Service: []string{service}})
	if err != nil {
		return nil, fmt.Errorf("encode command %q: %w", command, err)
	}

	// Per-request deadline. One deadline shared across the whole scrape let a
	// slow first command starve the second, whose failure was then reported
	// as if the second command were at fault.
	ctx, cancel := context.WithTimeout(ctx, effectiveTimeout(k.timeout))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if k.user != "" {
		req.SetBasicAuth(k.user, k.password)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kea call %q: %w", command, err)
	}
	defer func() {
		// Drain before closing so the connection can be reused: the JSON
		// decoder stops after the first value and may leave bytes behind.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		if msg := strings.TrimSpace(string(detail)); msg != "" {
			return nil, fmt.Errorf("kea call %q: HTTP %d: %s", command, resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("kea call %q: HTTP %d", command, resp.StatusCode)
	}

	var arr []keaResponse
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, fmt.Errorf("decode response for %q: %w", command, err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("empty response array for %q", command)
	}
	r := arr[0]
	if r.Result != 0 {
		return nil, fmt.Errorf("kea %q returned result=%d: %s", command, r.Result, r.Text)
	}
	return &r, nil
}

// mostRecentValue extracts the value of the newest sample from Kea's
// `[[value, ts], ...]` shape. Kea reports samples newest-first, so the newest
// is index 0.
func mostRecentValue(raw json.RawMessage) (float64, bool) {
	var samples [][]json.RawMessage
	if err := json.Unmarshal(raw, &samples); err != nil {
		return 0, false
	}
	if len(samples) == 0 || len(samples[0]) == 0 {
		return 0, false
	}
	// json.Unmarshal of `null` into a float64 succeeds and leaves 0, so a
	// null sample would silently report zero rather than being skipped.
	if string(bytes.TrimSpace(samples[0][0])) == "null" {
		return 0, false
	}
	var v float64
	if err := json.Unmarshal(samples[0][0], &v); err != nil {
		return 0, false
	}
	return v, true
}
