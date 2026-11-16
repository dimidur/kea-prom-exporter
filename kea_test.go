package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for kea.go: the control-socket client and its wire shapes.

func TestMostRecentValue(t *testing.T) {
	// Kea reports each statistic as [value, timestamp] samples, newest first.
	// The newest is the value; anything that is not a usable sample must be
	// refused rather than read as one.
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{"kea sample shape", `[[42, "2026-08-02 20:04:57.000000"]]`, 42, true},
		{"first sample is taken (Kea reports newest first)", `[[7, "2026-08-02 20:04:57"], [3, "2026-08-02 19:00:00"]]`, 7, true},
		{"float value", `[[1.5, "2026-08-02 20:04:57"]]`, 1.5, true},
		{"empty list", `[]`, 0, false},
		// An empty inner sample indexes out of range without the length
		// guard, and Registry.Gather does not recover: the process dies on
		// the scrape rather than reporting a bad value.
		{"empty inner sample", `[[]]`, 0, false},
		{"not a list", `{"nope": 1}`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			raw := json.RawMessage(tc.in)

			// Act
			got, ok := mostRecentValue(raw)

			// Assert
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("mostRecentValue(%s) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNullSampleIsSkippedRatherThanReportedAsZero(t *testing.T) {
	// Kea emits `[[null, "<ts>"]]` for a statistic it has no value for yet.
	// json.Unmarshal of null into a float64 succeeds and leaves 0, so without
	// an explicit guard the exporter reports a confident zero.

	// Arrange
	const nullSample = `[[null, "2026-08-02 20:04:57"]]`

	// Act
	got, ok := mostRecentValue(json.RawMessage(nullSample))

	// Assert
	if ok {
		t.Errorf("mostRecentValue(null sample) = (%v, true), want ok=false", got)
	}
}

func TestCommandIsValidJSONWhateverTheServiceName(t *testing.T) {
	// The service name reaches the wire from a flag. Formatted into a string,
	// a quote in it would change the command actually sent.

	// Arrange
	kea := &keaClient{url: "http://kea.example/", service: `dhcp4","injected":"`}

	// Act
	req, cancel, err := kea.newRequest(t.Context(), "status-get")
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	defer cancel()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Assert
	var got keaCommand
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not valid JSON: %v (%s)", err, body)
	}
	if got.Command != "status-get" {
		t.Errorf("command = %q, want status-get", got.Command)
	}
	if len(got.Service) != 1 || got.Service[0] != `dhcp4","injected":"` {
		t.Errorf("service = %q, want the literal flag value carried as one element", got.Service)
	}
}

func TestMalformedURLFailsTheCallBeforeAnythingIsSent(t *testing.T) {
	// --kea-url is not parsed until the first scrape, so a typo in it must
	// surface as a failed command that says what went wrong.

	// Arrange
	kea := &keaClient{url: "://no-scheme", service: supportedService}

	// Act
	_, err := kea.call(t.Context(), "status-get")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Errorf("call error = %v, want it to report the unusable URL", err)
	}
}

func TestRequestIsPostedAsJSONUnderItsOwnDeadline(t *testing.T) {
	// Kea's control socket takes a JSON POST, and each command must carry a
	// deadline even when the caller's context has none, or a hung socket
	// holds the scrape forever.

	// Arrange
	kea := &keaClient{url: "http://kea.example/", service: supportedService, timeout: time.Minute}
	before := time.Now()

	// Act
	req, cancel, err := kea.newRequest(t.Context(), "status-get")
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	defer cancel()

	// Assert
	if req.Method != http.MethodPost || req.URL.String() != "http://kea.example/" {
		t.Errorf("request = %s %s, want POST to the configured URL", req.Method, req.URL)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	deadline, ok := req.Context().Deadline()
	if !ok {
		t.Fatal("request has no deadline")
	}
	if remaining := deadline.Sub(before); remaining < time.Minute || remaining > time.Minute+time.Second {
		t.Errorf("deadline is %v after the call, want the configured minute", remaining)
	}
}

func TestCallersCancellationReachesTheRequest(t *testing.T) {
	// The request's deadline is layered on the caller's context, not on a
	// fresh one, so a caller that gives up stops the request with it.

	// Arrange
	kea := &keaClient{url: "http://kea.example/", service: supportedService, timeout: time.Minute}
	parent, cancelParent := context.WithCancel(t.Context())
	req, cancel, err := kea.newRequest(parent, "status-get")
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	defer cancel()

	// Act
	cancelParent()

	// Assert
	if req.Context().Err() == nil {
		t.Error("the request outlived its caller's cancellation")
	}
}

// trackedBody is a response body that records whether it was closed and how
// much of it was still unread at that moment. A read after Close is what a
// real response body refuses, so only bytes consumed before Close count.
type trackedBody struct {
	*strings.Reader
	closed        bool
	unreadAtClose int
}

func newTrackedBody(content string) *trackedBody {
	return &trackedBody{Reader: strings.NewReader(content)}
}

func (b *trackedBody) Close() error {
	if !b.closed {
		b.unreadAtClose = b.Len()
	}
	b.closed = true
	return nil
}

// stubTransport stands in for the network under an http.Client. A round trip
// fails with the request context's error once that context is done, as a real
// transport does, and with err when it is set; otherwise it answers with
// status and body.
type stubTransport struct {
	status int
	body   *trackedBody
	err    error
}

func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     http.Header{},
		Body:       s.body,
		Request:    req,
	}, nil
}

func TestTransportFailureNamesTheCommand(t *testing.T) {
	// A refused connection or a timeout never reaches interpretResponse, so
	// call itself must say which command failed and why; it is all the log
	// shows for a Kea that is down.

	// Arrange
	refused := errors.New("connection refused")
	kea := &keaClient{url: "http://kea.example/", client: &http.Client{Transport: stubTransport{err: refused}}}

	// Act
	_, err := kea.call(t.Context(), cmdStatusGet)

	// Assert
	if !errors.Is(err, refused) {
		t.Errorf("call error = %v, want it to wrap the transport's error", err)
	}
	if err == nil || !strings.Contains(err.Error(), cmdStatusGet) {
		t.Errorf("call error = %v, want it to name %s", err, cmdStatusGet)
	}
}

func TestCallDrainsAndClosesTheReply(t *testing.T) {
	// The JSON decoder stops after the first value, and a body left unread or
	// unclosed keeps its connection out of the pool, so every scrape would
	// open a new one to Kea's control socket.

	// Arrange: more trailing bytes than the decoder's first read takes, and
	// no more than errBodyLimit, so only the drain can consume them.
	body := newTrackedBody(`[{"result":0,"arguments":{}}]` + strings.Repeat(" ", errBodyLimit))
	transport := stubTransport{status: http.StatusOK, body: body}
	kea := &keaClient{url: "http://kea.example/", client: &http.Client{Transport: transport}}

	// Act
	_, err := kea.call(t.Context(), cmdStatusGet)

	// Assert
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !body.closed {
		t.Fatal("the reply body was not closed")
	}
	if body.unreadAtClose != 0 {
		t.Errorf("%d bytes of the reply were still unread when it was closed", body.unreadAtClose)
	}
}

func TestCallHandsKeasReplyToInterpretResponse(t *testing.T) {
	// call only sends and receives; what the reply means is
	// interpretResponse's decision, so the status and body Kea answered with
	// must reach it unchanged.

	// Arrange
	transport := stubTransport{status: http.StatusUnauthorized, body: newTrackedBody("credentials rejected\n")}
	kea := &keaClient{url: "http://kea.example/", client: &http.Client{Transport: transport}}

	// Act
	_, err := kea.call(t.Context(), "status-get")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "credentials rejected") {
		t.Errorf("call error = %v, want the 401 and Kea's explanation", err)
	}
}

func TestHTTPErrorBodyIsReportedAndBounded(t *testing.T) {
	// Kea explains 401/403 in the body. "HTTP 401" alone is the least useful
	// possible message for the most common misconfiguration, but an
	// unbounded body would let a proxy's HTML error page into the logs.

	// Arrange
	long := strings.Repeat("x", errBodyLimit*4)

	// Act
	_, err := interpretResponse("status-get", http.StatusUnauthorized, strings.NewReader(long))

	// Assert
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "status-get") {
		t.Errorf("error does not name the status and the command: %v", err)
	}
	if !strings.Contains(err.Error(), "xxxx") {
		t.Errorf("error does not quote the response body: %v", err)
	}
	if count := strings.Count(err.Error(), "x"); count > errBodyLimit {
		t.Errorf("error quotes %d body bytes, want at most %d", count, errBodyLimit)
	}
}

func TestUnreadableReplyIsAnErrorThatSaysWhy(t *testing.T) {
	// A reply with nothing usable in it is a failure, and its error must still
	// say which command and what was wrong, since it is all the log will show.
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"error status with an empty body", http.StatusBadGateway, "", "HTTP 502"},
		{"success status with a body that is not JSON", http.StatusOK, "<html>", "decode response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			body := strings.NewReader(tc.body)

			// Act
			_, err := interpretResponse("status-get", tc.status, body)

			// Assert
			if err == nil {
				t.Fatal("the reply was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "status-get") {
				t.Errorf("error = %v, want it to name status-get and contain %q", err, tc.want)
			}
		})
	}
}

func TestOnlyA2xxStatusIsReadAsKeasReply(t *testing.T) {
	// Kea itself answers 200, but a reverse proxy in front of the control
	// socket may answer 204/206 or similar, and rejecting those would turn a
	// working deployment into kea_up 0. Outside 2xx the body is not Kea's: a
	// 1xx is not a final answer, and a 3xx from an auth proxy is a login page
	// that would otherwise be decoded as statistics. Every case sends a body
	// that parses, so only the status decides.
	tests := []struct {
		status int
		accept bool
	}{
		{http.StatusContinue, false},
		{199, false},
		{http.StatusOK, true},
		{http.StatusAccepted, true},
		{299, true},
		{http.StatusMultipleChoices, false},
		{http.StatusFound, false},
	}
	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			// Arrange
			reply := strings.NewReader(`[{"result":0,"arguments":{}}]`)

			// Act
			_, err := interpretResponse("status-get", tc.status, reply)

			// Assert
			switch {
			case tc.accept && err != nil:
				t.Errorf("HTTP %d refused: %v", tc.status, err)
			case !tc.accept && err == nil:
				t.Errorf("HTTP %d was read as Kea's reply", tc.status)
			case !tc.accept && !strings.Contains(err.Error(), strconv.Itoa(tc.status)):
				t.Errorf("error %q does not name the status code", err)
			}
		})
	}
}

func TestTimeoutFallsBackRatherThanExpiringImmediately(t *testing.T) {
	// A zero timeout is not "no limit": context.WithTimeout(ctx, 0) is a
	// deadline already past, so every call would fail before being sent. The
	// fallback is what stops an unset field doing that, and naming the
	// constant lets this pin the value rather than the presence of a branch.
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"unset", 0, defaultKeaTimeout},
		{"negative", -time.Second, defaultKeaTimeout},
		{"configured", 2 * time.Second, 2 * time.Second},
		// The boundary: one nanosecond is a real setting, however useless.
		{"smallest positive", time.Nanosecond, time.Nanosecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			timeout := tc.in

			// Act
			got := effectiveTimeout(timeout)

			// Assert
			if got != tc.want {
				t.Errorf("effectiveTimeout(%v) = %v, want %v", timeout, got, tc.want)
			}
		})
	}
}

func TestNewKeaClientCarriesItsSettingsAndABackstop(t *testing.T) {
	// The settings must reach the client unchanged, and the HTTP client's own
	// limit must outlast the per-command deadline: a zero there would mean no
	// limit at all for anything the deadline does not cover.
	tests := []struct {
		name        string
		timeout     time.Duration
		wantTimeout time.Duration
	}{
		{"configured timeout", 3 * time.Second, 6 * time.Second},
		{"unset timeout", 0, 2 * defaultKeaTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			settings := keaClient{
				url:      "http://kea.example:8001/",
				user:     "exporter",
				password: "kea-password",
				service:  supportedService,
				timeout:  tc.timeout,
			}

			// Act
			client := newKeaClient(settings)

			// Assert
			if client.client == nil || client.client.Timeout != tc.wantTimeout {
				t.Fatalf("HTTP client = %+v, want a Timeout of %v", client.client, tc.wantTimeout)
			}
			withoutHTTP := *client
			withoutHTTP.client = nil
			if withoutHTTP != settings {
				t.Errorf("settings = %+v, want %+v carried unchanged", withoutHTTP, settings)
			}
		})
	}
}

func TestKeaResultErrorIsSurfaced(t *testing.T) {
	// A non-zero `result` is HTTP 200 with a failure inside, and must fail the
	// scrape with Kea's own explanation rather than be decoded as data.
	tests := []struct {
		name   string
		result int
		text   string
	}{
		{"error", 1, "unable to forward command"},
		{"unsupported command", 2, "'status-get' command not supported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			reply := fmt.Sprintf(`[{"result":%d,"text":%q}]`, tc.result, tc.text)

			// Act
			_, err := interpretResponse("status-get", http.StatusOK, strings.NewReader(reply))

			// Assert
			if err == nil {
				t.Fatalf("result=%d was accepted", tc.result)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("result=%d", tc.result)) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error drops Kea's own explanation: %v", err)
			}
		})
	}
}

func TestBasicAuthIsSentOnlyWhenAUserIsConfigured(t *testing.T) {
	// The configured credentials must reach Kea, or every scrape fails with a
	// 401; with no user configured, no Authorization header is sent at all.
	cases := []struct {
		name     string
		user     string
		password string
		wantAuth bool
	}{
		{"credentials configured", "kea", "kea-password", true},
		{"no user means no auth header", "", "kea-password", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			kea := &keaClient{url: "http://kea.example/", user: tc.user, password: tc.password}

			// Act
			req, cancel, err := kea.newRequest(t.Context(), "status-get")
			if err != nil {
				t.Fatalf("newRequest: %v", err)
			}
			defer cancel()

			// Assert
			gotUser, gotPassword, gotAuth := req.BasicAuth()
			if gotAuth != tc.wantAuth {
				t.Errorf("basic auth present = %v, want %v", gotAuth, tc.wantAuth)
			}
			if gotAuth && (gotUser != tc.user || gotPassword != tc.password) {
				t.Errorf("credentials = %q/%q, want %q/%q", gotUser, gotPassword, tc.user, tc.password)
			}
		})
	}
}

func TestEmptyResponseArrayIsAnError(t *testing.T) {
	// Kea wraps every response in an array with one entry per service the
	// command reached. A zero-length list therefore carries no result at all
	// and must not be read as "succeeded with nothing to report".

	// Arrange
	const reply = `[]`

	// Act
	_, err := interpretResponse("status-get", http.StatusOK, strings.NewReader(reply))

	// Assert
	if err == nil {
		t.Fatal("an empty response array was accepted")
	}
	if !strings.Contains(err.Error(), "empty response array") {
		t.Errorf("error does not explain the empty array: %v", err)
	}
}

func TestFirstResponseInTheArrayIsTheReply(t *testing.T) {
	// The exporter names one service, so the reply that answers it is the
	// first entry; any entry after it must not decide the outcome.

	// Arrange
	const reply = `[{"result":0,"text":"first"},{"result":1,"text":"second"}]`

	// Act
	got, err := interpretResponse("status-get", http.StatusOK, strings.NewReader(reply))

	// Assert
	if err != nil {
		t.Fatalf("interpretResponse: %v, want the first entry's success", err)
	}
	if got.Text != "first" {
		t.Errorf("reply text = %q, want the first entry's", got.Text)
	}
}
