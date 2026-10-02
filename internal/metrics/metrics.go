// Package metrics keeps lock-free counters and latency histograms for the whole run.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Histogram buckets: 1ms .. ~65s, power-of-two upper bounds in microseconds.
const nBuckets = 27

type Hist struct {
	count   atomic.Int64
	sumUs   atomic.Int64
	buckets [nBuckets]atomic.Int64
}

func (h *Hist) Observe(d time.Duration) {
	us := d.Microseconds()
	if us < 1 {
		us = 1
	}
	h.count.Add(1)
	h.sumUs.Add(us)
	b := int(math.Ceil(math.Log2(float64(us))))
	if b >= nBuckets {
		b = nBuckets - 1
	}
	h.buckets[b].Add(1)
}

// Quantile returns an upper-bound estimate (bucket edge) of the q-quantile.
func (h *Hist) Quantile(q float64) time.Duration {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	target := int64(math.Ceil(float64(total) * q))
	var acc int64
	for i := 0; i < nBuckets; i++ {
		acc += h.buckets[i].Load()
		if acc >= target {
			return time.Duration(int64(1)<<uint(i)) * time.Microsecond
		}
	}
	return time.Duration(int64(1)<<uint(nBuckets-1)) * time.Microsecond
}

func (h *Hist) Mean() time.Duration {
	n := h.count.Load()
	if n == 0 {
		return 0
	}
	return time.Duration(h.sumUs.Load()/n) * time.Microsecond
}

type Registry struct {
	Start time.Time

	// gauges
	Connected      atomic.Int64
	Ready          atomic.Int64 // connected and boot accepted
	ActiveSessions atomic.Int64
	PendingStops   atomic.Int64 // sessions that ended but whose StopTransaction has not been answered yet
	PeakSessions   atomic.Int64
	PeakConnected  atomic.Int64

	// counters
	ConnectAttempts atomic.Int64
	ConnectFailures atomic.Int64
	Disconnects     atomic.Int64
	InjectedDrops   atomic.Int64
	InjectedFaults  atomic.Int64
	BootAccepted    atomic.Int64
	BootRejected    atomic.Int64
	SessionsPlanned atomic.Int64
	SessionsStarted atomic.Int64
	SessionsDone    atomic.Int64
	SessionsAborted atomic.Int64
	SessionsSkipped atomic.Int64 // no free charger/connector/tag when the plan fired
	AuthRejected    atomic.Int64
	CallErrors      atomic.Int64
	CallTimeouts    atomic.Int64
	MsgSent         atomic.Int64
	MsgRecv         atomic.Int64
	QueuedOffline   atomic.Int64
	FlushedOffline  atomic.Int64

	mu    sync.Mutex
	hists map[string]*Hist
	errs  map[string]*atomic.Int64 // calls by action+outcome
}

func New() *Registry {
	return &Registry{Start: time.Now(), hists: map[string]*Hist{}, errs: map[string]*atomic.Int64{}}
}

func (r *Registry) Latency(action string) *Hist {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hists[action]
	if !ok {
		h = &Hist{}
		r.hists[action] = h
	}
	return h
}

func (r *Registry) Outcome(action, outcome string) {
	key := action + "|" + outcome
	r.mu.Lock()
	c, ok := r.errs[key]
	if !ok {
		c = &atomic.Int64{}
		r.errs[key] = c
	}
	r.mu.Unlock()
	c.Add(1)
}

func (r *Registry) ConnectedDelta(d int64) {
	n := r.Connected.Add(d)
	for {
		p := r.PeakConnected.Load()
		if n <= p || r.PeakConnected.CompareAndSwap(p, n) {
			return
		}
	}
}

func (r *Registry) SessionsDelta(d int64) {
	n := r.ActiveSessions.Add(d)
	for {
		p := r.PeakSessions.Load()
		if n <= p || r.PeakSessions.CompareAndSwap(p, n) {
			return
		}
	}
}

func (r *Registry) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.hists))
	for k := range r.hists {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Line is the one-line periodic progress report.
func (r *Registry) Line() string {
	return fmt.Sprintf("t=%s connected=%d (peak %d) sessions=%d (peak %d) started=%d done=%d aborted=%d skipped=%d auth_rej=%d conn_fail=%d disc=%d callerr=%d timeouts=%d sent=%d recv=%d queued=%d",
		time.Since(r.Start).Truncate(time.Second), r.Connected.Load(), r.PeakConnected.Load(),
		r.ActiveSessions.Load(), r.PeakSessions.Load(), r.SessionsStarted.Load(), r.SessionsDone.Load(),
		r.SessionsAborted.Load(), r.SessionsSkipped.Load(), r.AuthRejected.Load(), r.ConnectFailures.Load(),
		r.Disconnects.Load(), r.CallErrors.Load(), r.CallTimeouts.Load(), r.MsgSent.Load(), r.MsgRecv.Load(),
		r.QueuedOffline.Load())
}

type latSummary struct {
	Count  int64   `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Summary builds the end-of-run JSON document.
func (r *Registry) Summary() map[string]any {
	lat := map[string]latSummary{}
	for _, a := range r.actions() {
		h := r.Latency(a)
		lat[a] = latSummary{h.count.Load(), ms(h.Mean()), ms(h.Quantile(.5)), ms(h.Quantile(.95)), ms(h.Quantile(.99))}
	}
	outcomes := map[string]int64{}
	r.mu.Lock()
	for k, v := range r.errs {
		outcomes[k] = v.Load()
	}
	r.mu.Unlock()
	return map[string]any{
		"elapsed":          time.Since(r.Start).String(),
		"peak_connected":   r.PeakConnected.Load(),
		"peak_sessions":    r.PeakSessions.Load(),
		"sessions_planned": r.SessionsPlanned.Load(),
		"sessions_started": r.SessionsStarted.Load(),
		"sessions_done":    r.SessionsDone.Load(),
		"sessions_aborted": r.SessionsAborted.Load(),
		"sessions_skipped": r.SessionsSkipped.Load(),
		"auth_rejected":    r.AuthRejected.Load(),
		"connect_attempts": r.ConnectAttempts.Load(),
		"connect_failures": r.ConnectFailures.Load(),
		"disconnects":      r.Disconnects.Load(),
		"injected_drops":   r.InjectedDrops.Load(),
		"injected_faults":  r.InjectedFaults.Load(),
		"call_errors":      r.CallErrors.Load(),
		"call_timeouts":    r.CallTimeouts.Load(),
		"messages_sent":    r.MsgSent.Load(),
		"messages_recv":    r.MsgRecv.Load(),
		"queued_offline":   r.QueuedOffline.Load(),
		"flushed_offline":  r.FlushedOffline.Load(),
		"latency":          lat,
		"outcomes":         outcomes,
	}
}

func (r *Registry) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r.Summary())
}

// WritePrometheus renders the registry in Prometheus text format (no client library needed).
func (r *Registry) WritePrometheus(w io.Writer) {
	g := func(name string, v int64) { fmt.Fprintf(w, "sim_%s %d\n", name, v) }
	g("connected_chargers", r.Connected.Load())
	g("active_sessions", r.ActiveSessions.Load())
	g("sessions_started_total", r.SessionsStarted.Load())
	g("sessions_done_total", r.SessionsDone.Load())
	g("sessions_aborted_total", r.SessionsAborted.Load())
	g("sessions_skipped_total", r.SessionsSkipped.Load())
	g("auth_rejected_total", r.AuthRejected.Load())
	g("connect_failures_total", r.ConnectFailures.Load())
	g("disconnects_total", r.Disconnects.Load())
	g("call_errors_total", r.CallErrors.Load())
	g("call_timeouts_total", r.CallTimeouts.Load())
	g("messages_sent_total", r.MsgSent.Load())
	g("messages_received_total", r.MsgRecv.Load())
	for _, a := range r.actions() {
		h := r.Latency(a)
		for _, q := range []float64{.5, .95, .99} {
			fmt.Fprintf(w, "sim_call_latency_seconds{action=%q,quantile=\"%g\"} %g\n", a, q, h.Quantile(q).Seconds())
		}
		fmt.Fprintf(w, "sim_call_latency_count{action=%q} %d\n", a, h.count.Load())
	}
}
