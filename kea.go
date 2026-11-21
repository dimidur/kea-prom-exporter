package main

// Client for Kea's HTTP control socket, and the wire shapes its responses
// decode into, including the [[value, timestamp], ...] sample encoding.
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
// Kea wraps every command response in an array, with one entry per service
// the command was forwarded to.
type keaResponse struct {
	Result    int             `json:"result"`
	Text      string          `json:"text,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// keaSettings is how to reach one Kea control socket. validateConfig decides
// which settings are refused.
type keaSettings struct {
	url      string
	user     string
	password string
	service  string
	timeout  time.Duration
}

// keaClient talks to one Kea control socket.
type keaClient struct {
	keaSettings
	client *http.Client
}

// newKeaClient returns a client for settings whose requests go through
// transport; nil means http.DefaultTransport. The timeout bounds each
// command, and the HTTP client's limit is twice that, a backstop for anything
// the per-command deadline does not cover.
func newKeaClient(settings keaSettings, transport http.RoundTripper) *keaClient {
	return &keaClient{
		keaSettings: settings,
		client:      &http.Client{Transport: transport, Timeout: 2 * effectiveTimeout(settings.timeout)},
	}
}

// defaultKeaTimeout is the --kea-timeout default, and the fallback when the
// timeout is zero or negative. Either of those on context.WithTimeout is a
// deadline already past, so this is not a "no limit" fallback: it is what stops
// such a setting turning every call into an instant failure.
const defaultKeaTimeout = 5 * time.Second

// effectiveTimeout is the one place the timeout fallback is decided. The
// per-command deadline, the HTTP client's backstop and the scrape bound behind
// the shutdown grace all go through it, so they cannot disagree about what a
// zero timeout means.
func effectiveTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultKeaTimeout
	}
	return timeout
}

// keaCommand is marshalled rather than formatted into a string. The service
// name comes from a flag, and hand-built JSON would let a stray quote change
// the command actually sent.
type keaCommand struct {
	Command string   `json:"command"`
	Service []string `json:"service"`
}

// errBodyLimit caps how much of an error response is quoted back. Kea
// explains 400/401/403 in the body, and a bare "HTTP 401" is the least
// useful possible message for the most common misconfiguration.
const errBodyLimit = 512

// call sends command to Kea and returns its reply, as interpretResponse reads
// it.
func (k *keaClient) call(ctx context.Context, command string) (*keaResponse, error) {
	req, cancel, err := k.newRequest(ctx, command)
	if err != nil {
		return nil, err
	}
	defer cancel()
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
	return interpretResponse(command, resp.StatusCode, resp.Body)
}

// newRequest builds the request that sends command to Kea, under a deadline of
// its own. The caller calls cancel once the reply has been read.
func (k *keaClient) newRequest(ctx context.Context, command string) (*http.Request, context.CancelFunc, error) {
	// Marshal cannot fail here: every field of keaCommand is a string or a
	// []string.
	body, _ := json.Marshal(keaCommand{Command: command, Service: []string{k.service}})

	// A deadline per request, as keaCaller requires of every call.
	ctx, cancel := context.WithTimeout(ctx, effectiveTimeout(k.timeout))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if k.user != "" {
		req.SetBasicAuth(k.user, k.password)
	}
	return req, cancel, nil
}

// interpretResponse reads Kea's reply to command: the HTTP status and body it
// answered with. It returns the reply's first response, or an error saying
// why there is no usable one.
func interpretResponse(command string, status int, body io.Reader) (*keaResponse, error) {
	if status < 200 || status > 299 {
		detail, _ := io.ReadAll(io.LimitReader(body, errBodyLimit))
		if explanation := strings.TrimSpace(string(detail)); explanation != "" {
			return nil, fmt.Errorf("kea call %q: HTTP %d: %s", command, status, explanation)
		}
		return nil, fmt.Errorf("kea call %q: HTTP %d", command, status)
	}

	var replies []keaResponse
	if err := json.NewDecoder(body).Decode(&replies); err != nil {
		return nil, fmt.Errorf("decode response for %q: %w", command, err)
	}
	if len(replies) == 0 {
		return nil, fmt.Errorf("empty response array for %q", command)
	}
	first := replies[0]
	if first.Result != 0 {
		return nil, fmt.Errorf("kea %q returned result=%d: %s", command, first.Result, first.Text)
	}
	return &first, nil
}

// sampleKind is what the newest sample of a statistic holds.
type sampleKind int

const (
	sampleUnreadable sampleKind = iota // not Kea's sample shape, or a value that is not a number
	sampleNumber
	sampleMissing // no value: an empty sample list, or a null
)

// newestSample reads the newest sample from Kea's `[[value, ts], ...]` shape,
// and says what it holds; the value is meaningful only for sampleNumber. Kea
// reports samples newest-first, so the newest is index 0.
func newestSample(raw json.RawMessage) (float64, sampleKind) {
	var samples [][]json.RawMessage
	if err := json.Unmarshal(raw, &samples); err != nil {
		return 0, sampleUnreadable
	}
	// An empty list or a null is no value: docs/design.md, "Statistics the
	// exporter cannot export".
	if len(samples) == 0 {
		return 0, sampleMissing
	}
	if len(samples[0]) == 0 {
		return 0, sampleUnreadable
	}
	// json.Unmarshal of null into a float64 succeeds and leaves 0, so a null
	// from anything but Kea would otherwise read as a confident zero.
	if string(samples[0][0]) == "null" {
		return 0, sampleMissing
	}
	var value float64
	if err := json.Unmarshal(samples[0][0], &value); err != nil {
		return 0, sampleUnreadable
	}
	return value, sampleNumber
}
