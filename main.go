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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	url    string
	user   string
	pass   string
	client *http.Client
}

func (k *keaClient) call(ctx context.Context, command string) (*keaResponse, error) {
	body := fmt.Sprintf(`{"command":"%s","service":["%s"]}`, command, *scrapeService)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.url, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if k.user != "" {
		req.SetBasicAuth(k.user, k.pass)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kea call %q: %w", command, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
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
	haLocalState *prometheus.Desc
	haPartnerAge *prometheus.Desc
	haCommBroken *prometheus.Desc

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

		haLocalState: prometheus.NewDesc(
			ns+"_ha_local_state_info", "HA state of this peer (info-metric; value always 1; state in label).",
			[]string{"state", "role"}, nil),
		haPartnerAge: prometheus.NewDesc(
			ns+"_ha_partner_last_contact_seconds", "Seconds since the last successful heartbeat from the HA partner.",
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
	ch <- c.haLocalState
	ch <- c.haPartnerAge
	ch <- c.haCommBroken
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), *httpTimeout)
	defer cancel()

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
			continue
		}
		c.emitStat(ch, key, val)
	}
	return nil
}

// mostRecentValue extracts the first element of the most recent sample
// from Kea's `[[value, ts], ...]` shape.
func mostRecentValue(raw json.RawMessage) (float64, bool) {
	var samples [][]json.RawMessage
	if err := json.Unmarshal(raw, &samples); err != nil {
		return 0, false
	}
	if len(samples) == 0 || len(samples[0]) == 0 {
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
	if len(s.HighAvailability) == 0 {
		// HA hook not loaded — quietly skip these metrics.
		return nil
	}
	ha := s.HighAvailability[0]
	ch <- prometheus.MustNewConstMetric(
		c.haLocalState, prometheus.GaugeValue, 1,
		ha.HAServers.Local.State, ha.HAServers.Local.Role)
	ch <- prometheus.MustNewConstMetric(
		c.haPartnerAge, prometheus.GaugeValue, ha.HAServers.Remote.Age)
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

	pass, err := loadPassword(*keaPassFile, *keaPass)
	if err != nil {
		log.Fatal(err)
	}
	if *keaUser != "" && pass == "" {
		log.Fatal("kea-user set but no password provided (use --kea-password-file or --kea-password)")
	}

	k := &keaClient{
		url:  *keaURL,
		user: *keaUser,
		pass: pass,
		client: &http.Client{
			Timeout: *httpTimeout,
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
