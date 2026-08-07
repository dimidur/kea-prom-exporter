package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for kea.go: the control-socket client and its wire shapes.

func TestServiceDefaultsToDhcp4WhenUnset(t *testing.T) {
	// keaClient is constructed directly in tests and could be in future code.
	// An empty service must not reach the wire as `"service":[""]`, which Kea
	// rejects.

	// Arrange
	var got keaCommand
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	defer srv.Close()
	k := &keaClient{url: srv.URL, client: srv.Client()} // service deliberately empty

	// Act
	if _, err := k.call(t.Context(), "status-get"); err != nil {
		t.Fatalf("call: %v", err)
	}

	// Assert
	if len(got.Service) != 1 || got.Service[0] != "dhcp4" {
		t.Errorf("service = %#v, want [dhcp4] when unset", got.Service)
	}
}

func TestMostRecentValue(t *testing.T) {
	// Arrange
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
		// guard, and Registry.Gather does not recover -- the process dies on
		// the scrape rather than reporting a bad value.
		{"empty inner sample", `[[]]`, 0, false},
		{"not a list", `{"nope": 1}`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got, ok := mostRecentValue(json.RawMessage(tc.in))
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

	// Act + Assert
	if got, ok := mostRecentValue(json.RawMessage(nullSample)); ok {
		t.Errorf("mostRecentValue(null sample) = (%v, true), want ok=false", got)
	}
}

func TestCommandIsValidJSONWhateverTheServiceName(t *testing.T) {
	// The service name reaches the wire from a flag. Built with fmt.Sprintf it
	// was possible to inject a quote and change the command actually sent.
	// Arrange
	var got keaCommand
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not valid JSON: %v (%s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, service: `dhcp4","injected":"`, client: srv.Client()}
	// Act
	if _, err := k.call(t.Context(), "status-get"); err != nil {
		t.Fatalf("call: %v", err)
	}
	// Assert
	if got.Command != "status-get" {
		t.Errorf("command = %q, want status-get", got.Command)
	}
	if len(got.Service) != 1 || got.Service[0] != `dhcp4","injected":"` {
		t.Errorf("service = %q, want the literal flag value carried as one element", got.Service)
	}
}

func TestHTTPErrorBodyIsReportedAndBounded(t *testing.T) {
	// Kea explains 401/403 in the body. "HTTP 401" alone is the least useful
	// possible message for the most common misconfiguration -- but an
	// unbounded body would let a proxy's HTML error page into the logs.
	// Arrange
	long := strings.Repeat("x", errBodyLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, long, http.StatusUnauthorized)
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	// Act
	_, err := k.call(t.Context(), "status-get")
	// Assert
	if err == nil {
		t.Fatal("call succeeded against a 401")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error does not name the status: %v", err)
	}
	if !strings.Contains(err.Error(), "xxxx") {
		t.Errorf("error does not quote the response body: %v", err)
	}
	if n := strings.Count(err.Error(), "x"); n > errBodyLimit {
		t.Errorf("error quotes %d body bytes, want at most %d", n, errBodyLimit)
	}
}

func TestNon200SuccessStatusIsAccepted(t *testing.T) {
	// Kea itself answers 200, but a reverse proxy in front of the control
	// socket may legitimately answer 204/206 or similar. Rejecting anything
	// but exactly 200 turned a working deployment into kea_up 0.
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}

	// Act + Assert
	if _, err := k.call(t.Context(), "status-get"); err != nil {
		t.Errorf("call on HTTP 202: %v", err)
	}
}

func TestRedirectStatusIsRejectedRatherThanDecoded(t *testing.T) {
	// The mirror of the test above, and the reason the accepted range has an
	// upper bound. A 3xx from an auth proxy in front of the control socket is
	// not a Kea response: widening the check to accept it means a login page
	// gets decoded as statistics, and a scrape that should have failed loudly
	// reports whatever happened to parse.
	//
	// CheckRedirect is set not to follow, so the 302 is what call() sees
	// rather than whatever it points at.

	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://sso.example/login")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	k := &keaClient{url: srv.URL, client: client}

	// Act
	_, err := k.call(t.Context(), "status-get")

	// Assert
	if err == nil {
		t.Fatal("call on HTTP 302 returned no error; a redirect was treated as a Kea response")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("error %q does not name the status code", err)
	}
}

func TestTimeoutFallsBackRatherThanExpiringImmediately(t *testing.T) {
	// A zero timeout is not "no limit": context.WithTimeout(ctx, 0) is a
	// deadline already past, so every call would fail before being sent. The
	// fallback is what stops an unset field doing that, and naming the
	// constant lets this pin the value rather than the presence of a branch.

	// Arrange
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
			// Act
			got := effectiveTimeout(tc.in)

			// Assert
			if got != tc.want {
				t.Errorf("effectiveTimeout(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	// And it is the documented 5s, not merely something positive.
	if defaultKeaTimeout != 5*time.Second {
		t.Errorf("defaultKeaTimeout = %v, want 5s -- the --kea-timeout help says 5s", defaultKeaTimeout)
	}
}

func TestKeaResultErrorIsSurfaced(t *testing.T) {
	// A non-zero `result` is HTTP 200 with a failure inside. Kea returns 1 for
	// an error and 2 for an unsupported command; both must fail the scrape.
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":2,"text":"'status-get' command not supported"}]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	// Act
	_, err := k.call(t.Context(), "status-get")
	// Assert
	if err == nil {
		t.Fatal("call succeeded against result=2")
	}
	if !strings.Contains(err.Error(), "result=2") || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error drops Kea's own explanation: %v", err)
	}
}

func TestBasicAuthIsSentOnlyWhenAUserIsConfigured(t *testing.T) {
	// Arrange
	cases := []struct {
		name     string
		user     string
		password string
		wantAuth bool
	}{
		{"credentials configured", "kea", "s3cret", true},
		{"no user means no auth header", "", "s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange -- the handler only records what arrived. Asserting
			// inside it would put the checks on the server goroutine during
			// the Act, where a failure to call at all reads as a pass.
			var (
				gotUser, gotPassword string
				gotAuth, served      bool
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotUser, gotPassword, gotAuth = r.BasicAuth()
				served = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
			}))
			defer srv.Close()
			k := &keaClient{url: srv.URL, user: tc.user, password: tc.password, client: srv.Client()}

			// Act
			if _, err := k.call(t.Context(), "status-get"); err != nil {
				t.Fatalf("call: %v", err)
			}

			// Assert
			if !served {
				t.Fatal("the stub was never called; the assertions below would be vacuous")
			}
			if gotAuth != tc.wantAuth {
				t.Errorf("basic auth present = %v, want %v", gotAuth, tc.wantAuth)
			}
			if gotAuth && (gotUser != tc.user || gotPassword != tc.password) {
				t.Errorf("credentials = %q/%q, want %q/%q", gotUser, gotPassword, tc.user, tc.password)
			}
		})
	}
}

func TestPerRequestTimeoutBoundsEachCallSeparately(t *testing.T) {
	// One deadline shared across the scrape let a slow first command consume
	// the budget and made the second fail as if it were at fault. Each call
	// now carries its own.
	// The first handler must outlive the first call's deadline, then be
	// released explicitly. Waiting on r.Context() instead would deadlock:
	// net/http only starts watching for a client disconnect once the request
	// body has been consumed, and this handler never reads it.
	// Arrange
	// Released via Cleanup, not after the wait: releasing afterwards means
	// a shutdown that ignores the grace hangs until the package timeout
	// instead of failing in milliseconds.
	release := make(chan struct{})
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	// Registered after Close so it runs before it: the handler has to be let
	// go before Close will stop waiting on it.
	defer srv.Close()
	defer close(release)

	k := &keaClient{url: srv.URL, timeout: 50 * time.Millisecond, client: srv.Client()}
	ctx := t.Context()
	// Act
	if _, err := k.call(ctx, "statistic-get-all"); err == nil {
		t.Fatal("first call succeeded, expected it to time out")
	}
	// Assert
	if _, err := k.call(ctx, "status-get"); err != nil {
		t.Errorf("second call inherited the first call's exhausted deadline: %v", err)
	}
}

func TestEmptyResponseArrayIsAnError(t *testing.T) {
	// Kea wraps every response in an array with one entry per service the
	// command reached. A zero-length list therefore carries no result at all
	// and must not be read as "succeeded with nothing to report".
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	// Act
	_, err := k.call(t.Context(), "status-get")
	// Assert
	if err == nil {
		t.Fatal("call succeeded against an empty response array")
	}
	if !strings.Contains(err.Error(), "empty response array") {
		t.Errorf("error does not explain the empty array: %v", err)
	}
}
