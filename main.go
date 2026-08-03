// Prometheus exporter for ISC Kea DHCPv4 (Kea 3.x).
//
// On each /metrics scrape the exporter POSTs `statistic-get-all` and
// `status-get` to the kea-dhcp4 daemon's HTTP control socket (basic
// auth) and translates the responses into Prometheus metrics.
//
// Scope: a headline metric set covering lease pool utilisation, packet
// counters, and HA peer state -- not full statistic coverage. Forward-compatible
// by design — an unknown statistic key is logged once and ignored
// rather than failing the scrape, so a Kea release that adds new
// statistics degrades to missing metrics instead of no metrics.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddr    = flag.String("listen", envOr("LISTEN", ":9547"), "Prometheus metrics listen address (also LISTEN env).")
	keaURL        = flag.String("kea-url", envOr("KEA_URL", "http://127.0.0.1:8001/"), "Kea HTTP control socket URL (also KEA_URL env).")
	keaUser       = flag.String("kea-user", envOr("KEA_USER", ""), "Kea HTTP basic auth user (also KEA_USER env). Empty disables auth.")
	keaPassFile   = flag.String("kea-password-file", envOr("KEA_PASSWORD_FILE", ""), "Path to file containing Kea HTTP basic auth password (also KEA_PASSWORD_FILE env). Precedence over --kea-password.")
	keaPass       = flag.String("kea-password", envOr("KEA_PASSWORD", ""), "Kea HTTP basic auth password (also KEA_PASSWORD env). Prefer --kea-password-file for production.")
	httpTimeout   = flag.Duration("kea-timeout", 5*time.Second, "Kea API request timeout.")
	scrapeService = flag.String("kea-service", "dhcp4", "Kea service name to scrape (currently only dhcp4 is implemented).")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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
	timeout := k.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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

// collector implements prometheus.Collector. Each /metrics scrape
// triggers a single statistic-get-all + status-get round trip — we
// emit ConstMetrics inline rather than maintaining a pre-registered
// gauge cache, which avoids label-set drift surprises when Kea adds new
// statistic dimensions between releases.
type collector struct {
	kea *keaClient

	// Scrape health.
	up           *prometheus.Desc
	commandUp    *prometheus.Desc
	scrapeErrors *prometheus.Desc

	// dhcp4 lease metrics, subnet-scoped.
	addressesAssigned *prometheus.Desc
	addressesCapacity *prometheus.Desc

	// dhcp4 packet counters.
	pkt4Received *prometheus.Desc
	pkt4Sent     *prometheus.Desc

	// HA state.
	haEnabled        *prometheus.Desc
	haLocalState     *prometheus.Desc
	haPartnerAge     *prometheus.Desc
	haPartnerInTouch *prometheus.Desc
	haCommBroken     *prometheus.Desc

	// Cumulative scrape failures per command. Atomic and per-collector, not
	// package globals: Prometheus may call Collect concurrently for
	// overlapping scrapes, and a plain counter increment there is a data race.
	statErrorCount atomic.Uint64
	haErrorCount   atomic.Uint64

	// Track stat keys we don't have a mapping for. Logged exactly
	// once each — keeps scrape noise low while still surfacing drift.
	unhandledMu sync.Mutex
	unhandled   map[string]struct{}
}

func newCollector(k *keaClient) *collector {
	const ns = "kea_dhcp4"
	return &collector{
		kea:       k,
		unhandled: make(map[string]struct{}),

		up: prometheus.NewDesc(
			"kea_up", "1 if the last scrape of the Kea control socket succeeded, else 0.",
			nil, nil),
		commandUp: prometheus.NewDesc(
			"kea_command_up", "1 if this Kea control command succeeded on the last scrape, else 0.",
			[]string{"command"}, nil),
		scrapeErrors: prometheus.NewDesc(
			"kea_scrape_errors_total", "Cumulative scrape errors since exporter start, by command.",
			[]string{"command"}, nil),

		addressesAssigned: prometheus.NewDesc(
			ns+"_addresses_assigned", "Currently assigned IPv4 addresses in the pool.",
			[]string{"subnet"}, nil),
		addressesCapacity: prometheus.NewDesc(
			ns+"_addresses_capacity", "Pool size: total IPv4 addresses available in the subnet.",
			[]string{"subnet"}, nil),

		pkt4Received: prometheus.NewDesc(
			ns+"_packets_received_total", "Total DHCPv4 packets received by type.",
			[]string{"type"}, nil),
		pkt4Sent: prometheus.NewDesc(
			ns+"_packets_sent_total", "Total DHCPv4 packets sent by type.",
			[]string{"type"}, nil),

		haEnabled: prometheus.NewDesc(
			ns+"_ha_enabled", "1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.",
			nil, nil),
		haLocalState: prometheus.NewDesc(
			ns+"_ha_local_state_info", "HA state of this peer (info-metric; value always 1; state in label).",
			[]string{"state", "role", "mode"}, nil),
		haPartnerAge: prometheus.NewDesc(
			ns+"_ha_partner_last_contact_seconds",
			"Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.",
			nil, nil),
		haPartnerInTouch: prometheus.NewDesc(
			ns+"_ha_partner_in_touch", "1 once this peer has been in contact with its HA partner, else 0.",
			nil, nil),
		haCommBroken: prometheus.NewDesc(
			ns+"_ha_communication_interrupted", "1 if HA communication with the partner is interrupted, else 0.",
			nil, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.commandUp
	ch <- c.scrapeErrors
	ch <- c.addressesAssigned
	ch <- c.addressesCapacity
	ch <- c.pkt4Received
	ch <- c.pkt4Sent
	ch <- c.haEnabled
	ch <- c.haLocalState
	ch <- c.haPartnerAge
	ch <- c.haPartnerInTouch
	ch <- c.haCommBroken
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// No shared deadline here: keaClient.call bounds each request on its own,
	// so a slow first command cannot starve the second.
	ctx := context.Background()

	statErr := c.collectStats(ctx, ch)
	haErr := c.collectHA(ctx, ch)

	if statErr != nil {
		log.Printf("statistic-get-all failed: %v", statErr)
	}
	if haErr != nil {
		log.Printf("status-get failed: %v", haErr)
	}

	// kea_up is 0 if ANY command failed. Reporting 1 while half the metric
	// set is silently missing is worse than reporting down: an operator
	// alerting on kea_up == 0 would never see a half-broken scrape.
	upValue := 1.0
	if statErr != nil || haErr != nil {
		upValue = 0
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, upValue)

	// Per-command health and error counters, so the two failure modes are
	// distinguishable without reading logs.
	c.emitCommandHealth(ch, "statistic-get-all", statErr, &c.statErrorCount)
	c.emitCommandHealth(ch, "status-get", haErr, &c.haErrorCount)
}

func (c *collector) emitCommandHealth(
	ch chan<- prometheus.Metric, command string, err error, counter *atomic.Uint64,
) {
	value := 1.0
	count := counter.Load()
	if err != nil {
		value = 0
		count = counter.Add(1)
	}
	ch <- prometheus.MustNewConstMetric(c.commandUp, prometheus.GaugeValue, value, command)
	ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, float64(count), command)
}

func (c *collector) collectStats(ctx context.Context, ch chan<- prometheus.Metric) error {
	resp, err := c.kea.call(ctx, "statistic-get-all")
	if err != nil {
		return err
	}
	// Kea returns: {"<stat-name>": [[value, "timestamp"], ...]}.
	// Numeric values may decode as float64 or as JSON number.
	var stats map[string]json.RawMessage
	if err := json.Unmarshal(resp.Arguments, &stats); err != nil {
		return fmt.Errorf("decode statistic-get-all arguments: %w", err)
	}
	for key, raw := range stats {
		val, ok := mostRecentValue(raw)
		if !ok {
			// Kea also reports string- and duration-typed statistics. Route
			// them through the same once-only log as unmapped keys: dropping
			// them silently left an absent metric with nothing anywhere
			// explaining why.
			c.noteUnhandled(key)
			continue
		}
		c.emitStat(ch, key, val)
	}
	return nil
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

func (c *collector) emitStat(ch chan<- prometheus.Metric, key string, value float64) {
	// subnet-scoped stats, e.g. "subnet[1].assigned-addresses" and
	// (Kea 3.x) "subnet[1].pool[0].assigned-addresses". We
	// roll up to the subnet level and ignore the per-pool variants —
	// the per-subnet keys still exist alongside per-pool in 3.x.
	if strings.HasPrefix(key, "subnet[") {
		end := strings.Index(key, "]")
		if end < 0 {
			return
		}
		subnetID := key[len("subnet["):end]
		if end+2 > len(key) {
			return
		}
		rest := key[end+2:]
		if strings.HasPrefix(rest, "pool[") {
			return
		}
		switch rest {
		case "assigned-addresses":
			ch <- prometheus.MustNewConstMetric(c.addressesAssigned, prometheus.GaugeValue, value, subnetID)
		case "total-addresses":
			ch <- prometheus.MustNewConstMetric(c.addressesCapacity, prometheus.GaugeValue, value, subnetID)
		default:
			c.noteUnhandled(key)
		}
		return
	}
	// Global packet counters: pkt4-<op>-{sent,received}.
	//
	// "pkt4-received" and "pkt4-sent" are Kea's GRAND TOTALS, not a packet
	// type -- pkt4-sent equals pkt4-offer-sent + pkt4-ack-sent exactly.
	// Emitting them beside the per-type series made
	// sum(kea_dhcp4_packets_sent_total) return twice the real count. They are
	// dropped: the per-type series already add up to them.
	if key == "pkt4-received" || key == "pkt4-sent" {
		return
	}
	if strings.HasPrefix(key, "pkt4-") {
		switch {
		case strings.HasSuffix(key, "-received"):
			op := strings.TrimSuffix(strings.TrimPrefix(key, "pkt4-"), "-received")
			ch <- prometheus.MustNewConstMetric(c.pkt4Received, prometheus.CounterValue, value, op)
		case strings.HasSuffix(key, "-sent"):
			op := strings.TrimSuffix(strings.TrimPrefix(key, "pkt4-"), "-sent")
			ch <- prometheus.MustNewConstMetric(c.pkt4Sent, prometheus.CounterValue, value, op)
		default:
			c.noteUnhandled(key)
		}
		return
	}
	c.noteUnhandled(key)
}

func (c *collector) noteUnhandled(key string) {
	c.unhandledMu.Lock()
	defer c.unhandledMu.Unlock()
	if _, seen := c.unhandled[key]; seen {
		return
	}
	c.unhandled[key] = struct{}{}
	log.Printf("unhandled statistic %q (logged once; extend collector to map it)", key)
}

// haStatus is the trimmed status-get response we care about. The full
// response is much richer; we only decode the fields used below.
type haStatus struct {
	HighAvailability []struct {
		HAMode    string `json:"ha-mode"`
		HAServers struct {
			Local struct {
				Role  string `json:"role"`
				State string `json:"state"`
			} `json:"local"`
			Remote struct {
				Age                    float64 `json:"age"`
				CommunicationInterrupt bool    `json:"communication-interrupted"`
				InTouch                bool    `json:"in-touch"`
			} `json:"remote"`
		} `json:"ha-servers"`
	} `json:"high-availability"`
}

func (c *collector) collectHA(ctx context.Context, ch chan<- prometheus.Metric) error {
	resp, err := c.kea.call(ctx, "status-get")
	if err != nil {
		return err
	}
	var s haStatus
	if err := json.Unmarshal(resp.Arguments, &s); err != nil {
		return fmt.Errorf("decode status-get arguments: %w", err)
	}
	// Absent HA metrics could mean the hook is not loaded, or that status-get
	// failed. This makes the first case explicit so the two are
	// distinguishable without reading logs.
	if len(s.HighAvailability) == 0 {
		ch <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 0)
		return nil
	}
	ch <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 1)

	if len(s.HighAvailability) > 1 {
		log.Printf("status-get reported %d HA relationships; only the first is exported",
			len(s.HighAvailability))
	}
	ha := s.HighAvailability[0]
	ch <- prometheus.MustNewConstMetric(
		c.haLocalState, prometheus.GaugeValue, 1,
		ha.HAServers.Local.State, ha.HAServers.Local.Role, ha.HAMode)

	// Kea reports age 0 when it has never been in touch with the partner.
	// Exporting that unconditionally reads on a dashboard as "contacted 0
	// seconds ago" — the exact inverse of the truth — so the gauge is only
	// emitted when it means something, alongside an explicit in-touch signal.
	inTouch := 0.0
	if ha.HAServers.Remote.InTouch {
		inTouch = 1
		ch <- prometheus.MustNewConstMetric(
			c.haPartnerAge, prometheus.GaugeValue, ha.HAServers.Remote.Age)
	}
	ch <- prometheus.MustNewConstMetric(c.haPartnerInTouch, prometheus.GaugeValue, inTouch)

	commVal := 0.0
	if ha.HAServers.Remote.CommunicationInterrupt {
		commVal = 1
	}
	ch <- prometheus.MustNewConstMetric(c.haCommBroken, prometheus.GaugeValue, commVal)
	return nil
}

func loadPassword(file, inline string) (string, error) {
	if file != "" {
		// #nosec G304 -- the path is supplied by the operator via
		// --kea-password-file / KEA_PASSWORD_FILE. Reading an operator-named
		// file is the feature; there is no untrusted input on this path.
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read kea-password-file %q: %w", file, err)
		}
		return strings.TrimRight(string(b), "\n\r"), nil
	}
	return inline, nil
}

func main() {
	flag.Parse()

	secret, err := loadPassword(*keaPassFile, *keaPass)
	if err != nil {
		log.Fatal(err)
	}
	if *keaUser != "" && secret == "" {
		log.Fatal("kea-user set but no password provided (use --kea-password-file or --kea-password)")
	}
	// Only dhcp4 is mapped. Without this, --kea-service=dhcp6 returns a valid
	// response whose every key is unmapped: an empty metric set under a
	// kea_dhcp4_ namespace, reported as kea_up 1.
	if *scrapeService != "dhcp4" {
		log.Fatalf("--kea-service=%q is not supported; only dhcp4 is implemented", *scrapeService)
	}

	k := &keaClient{
		url:      *keaURL,
		user:     *keaUser,
		password: secret,
		service:  *scrapeService,
		timeout:  *httpTimeout,
		client: &http.Client{
			// Generous relative to the per-request context deadline, which is
			// what actually bounds a call; this is a backstop.
			Timeout: *httpTimeout * 2,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		newCollector(k),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "kea-prom-exporter — see /metrics")
	})

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("kea-prom-exporter listening on %s, target=%s", *listenAddr, *keaURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
