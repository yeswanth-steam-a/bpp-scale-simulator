// Package charger implements one simulated OCPP 1.6J charge point.
//
// Concurrency model: each charger owns a single goroutine (Run) that executes every state change,
// so no locks are needed. A per-connection reader goroutine feeds decoded frames into that loop;
// timers post closures into c.events. The fleet only touches atomics (Ready/FreeSlots) and cmds.
package charger

import (
	"context"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/steam-a/bpp-load-simulator/internal/config"
	"github.com/steam-a/bpp-load-simulator/internal/metrics"
	"github.com/steam-a/bpp-load-simulator/internal/ocpp"
)

type CmdKind int

const (
	CmdStartSession CmdKind = iota
	CmdDrop                 // abruptly kill the socket (network failure)
	CmdFault                // put an idle connector into Faulted for a while
)

type Cmd struct {
	Kind     CmdKind
	Duration time.Duration // StartSession: simulated session length. Fault: simulated fault length.
	IDTag    string
	Release  func() // StartSession: called exactly once when the session slot (and tag) is free again
}

type connector struct {
	id     int
	status string
	sess   *session
}

type stage int

const (
	stPreparing stage = iota
	stAuthorizing
	stStarting
	stCharging
)

type session struct {
	idTag     string
	stage     stage
	txID      int
	meterWh   float64
	socPct    float64
	capWh     float64 // battery capacity
	powerFrac float64 // vehicle's share of the charger's max power
	plannedAt time.Duration
	elapsed   time.Duration // simulated time charging
	release   func()
}

type pending struct {
	action string
	sent   time.Time
	cb     func(*ocpp.Frame, error)
}

type queued struct {
	action  string
	payload any
}

type Charger struct {
	ID   string
	prof *config.Profile
	cfg  *config.Config
	m    *metrics.Registry
	rng  *rand.Rand
	gate *rate.Limiter

	Cmds chan Cmd

	ready atomic.Bool  // connected and boot accepted
	free  atomic.Int32 // connectors that can take a new session

	ctx    context.Context
	events chan func()
	conns  []*connector

	// per-connection state (loop goroutine only)
	conn      *websocket.Conn
	online    bool
	booted    bool
	hbEvery   time.Duration
	pend      map[string]*pending
	nextID    int
	queue     []queued
	meterBase float64 // cumulative energy register across sessions (Wh)
}

func New(id string, prof *config.Profile, connectors int, cfg *config.Config, m *metrics.Registry, gate *rate.Limiter, seed int64) *Charger {
	c := &Charger{
		ID: id, prof: prof, cfg: cfg, m: m, gate: gate,
		rng:    rand.New(rand.NewSource(seed)),
		Cmds:   make(chan Cmd, 4),
		events: make(chan func(), 64),
		pend:   map[string]*pending{},
	}
	for i := 1; i <= connectors; i++ {
		c.conns = append(c.conns, &connector{id: i, status: "Available"})
	}
	c.meterBase = float64(c.rng.Intn(5_000_000))
	return c
}

func (c *Charger) Ready() bool      { return c.ready.Load() }
func (c *Charger) FreeSlots() int32 { return c.free.Load() }

func (c *Charger) real(d time.Duration) time.Duration {
	return time.Duration(float64(d) / c.cfg.TimeScale)
}

func (c *Charger) jitter(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(c.rng.Int63n(int64(hi-lo)))
}

func (c *Charger) after(d time.Duration, f func()) {
	time.AfterFunc(d, func() {
		select {
		case c.events <- f:
		case <-c.ctx.Done():
		}
	})
}

func (c *Charger) updateFree() {
	var n int32
	for _, k := range c.conns {
		if k.status == "Available" && k.sess == nil {
			n++
		}
	}
	c.free.Store(n)
}

// Run drives the charger until ctx is cancelled; connection attempts are paced by the shared gate.
func (c *Charger) Run(ctx context.Context) {
	c.ctx = ctx
	backoff := c.cfg.ReconnectMin
	for ctx.Err() == nil {
		// reserve a connect slot but keep servicing timers while waiting (an offline session keeps running)
		r := c.gate.Reserve()
		if !r.OK() {
			return
		}
		c.idle(r.Delay())
		if ctx.Err() != nil {
			return
		}
		c.m.ConnectAttempts.Add(1)
		d := websocket.Dialer{
			HandshakeTimeout: 20 * time.Second,
			Subprotocols:     []string{ocpp.Subprotocol},
			ReadBufferSize:   1024,
			WriteBufferSize:  1024,
		}
		conn, _, err := d.DialContext(ctx, c.cfg.Target+c.ID, http.Header{})
		if err != nil {
			c.m.ConnectFailures.Add(1)
			c.idle(c.jitter(backoff/2, backoff))
			if backoff *= 2; backoff > c.cfg.ReconnectMax {
				backoff = c.cfg.ReconnectMax
			}
			continue
		}
		backoff = c.cfg.ReconnectMin
		c.serve(conn)
		if ctx.Err() != nil {
			return
		}
		c.m.Disconnects.Add(1)
		c.idle(c.jitter(c.cfg.ReconnectMin, 3*c.cfg.ReconnectMin+time.Second))
	}
}

// idle waits for d while still servicing timers (a session keeps "charging" locally while offline).
func (c *Charger) idle(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return
		case f := <-c.events:
			f()
		case cmd := <-c.Cmds:
			c.handleCmd(cmd)
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Charger) serve(conn *websocket.Conn) {
	c.conn = conn
	c.online = true
	c.booted = false
	c.m.ConnectedDelta(1)
	in := make(chan *ocpp.Frame, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, b, err := conn.ReadMessage()
			if err != nil {
				return
			}
			c.m.MsgRecv.Add(1)
			f, err := ocpp.Decode(b)
			if err != nil {
				continue
			}
			select {
			case in <- f:
			case <-c.ctx.Done():
				return
			}
		}
	}()

	hb := time.NewTimer(time.Hour)
	hb.Stop()
	sweep := time.NewTicker(c.cfg.CallTimeout/2 + time.Second)
	defer sweep.Stop()
	defer hb.Stop()

	c.sendBoot(hb)

loop:
	for {
		select {
		case f := <-in:
			c.handleFrame(f)
		case f := <-c.events:
			f()
		case cmd := <-c.Cmds:
			c.handleCmd(cmd)
		case <-hb.C:
			if c.booted {
				c.call("Heartbeat", struct{}{}, nil)
				hb.Reset(c.real(c.hbEvery))
			}
		case <-sweep.C:
			c.expirePending()
		case <-done:
			break loop
		case <-c.ctx.Done():
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			break loop
		}
	}

	_ = conn.Close()
	c.online, c.booted = false, false
	if c.ready.Swap(false) {
		c.m.Ready.Add(-1)
	}
	c.m.ConnectedDelta(-1)
	c.failPending()
}

// ---- outbound ----

func (c *Charger) call(action string, payload any, cb func(*ocpp.Frame, error)) bool {
	if !c.online {
		if cb != nil {
			cb(nil, errOffline)
		}
		return false
	}
	c.nextID++
	id := strconv.Itoa(c.nextID)
	b, err := ocpp.EncodeCall(id, action, payload)
	if err != nil {
		return false
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
		_ = c.conn.Close() // reader goroutine notices and ends the connection
		if cb != nil {
			cb(nil, err)
		}
		return false
	}
	c.m.MsgSent.Add(1)
	c.pend[id] = &pending{action: action, sent: time.Now(), cb: cb}
	return true
}

// callQueueable sends the message, or buffers it for after the next successful boot.
func (c *Charger) callQueueable(action string, payload any, cb func(*ocpp.Frame, error)) {
	if c.online && c.booted {
		c.call(action, payload, cb)
		return
	}
	if len(c.queue) < 5000 {
		c.queue = append(c.queue, queued{action, payload})
		c.m.QueuedOffline.Add(1)
	}
	if cb != nil {
		cb(nil, errOffline)
	}
}

func (c *Charger) flushQueue() {
	q := c.queue
	c.queue = nil
	for _, m := range q {
		if !c.call(m.action, m.payload, nil) {
			break
		}
		c.m.FlushedOffline.Add(1)
	}
}

func (c *Charger) status(k *connector, status, errCode string) {
	k.status = status
	c.updateFree()
	if c.online && c.booted {
		c.call("StatusNotification", ocpp.StatusNotificationReq{
			ConnectorID: k.id, ErrorCode: errCode, Status: status, Timestamp: ocpp.Now(),
		}, nil)
	}
}

func (c *Charger) sendBoot(hb *time.Timer) {
	c.call("BootNotification", ocpp.BootNotificationReq{
		ChargePointVendor: c.prof.Vendor, ChargePointModel: c.prof.Model,
		ChargePointSerialNumber: c.ID, FirmwareVersion: "1.0.0-loadsim",
	}, func(f *ocpp.Frame, err error) {
		if err != nil || f == nil {
			return
		}
		var res ocpp.BootNotificationRes
		if f.Type != ocpp.CallResult || jsonUnmarshal(f.Payload, &res) != nil {
			return
		}
		if res.Interval <= 0 {
			res.Interval = 60
		}
		c.hbEvery = time.Duration(res.Interval) * time.Second
		if res.Status != "Accepted" {
			c.m.BootRejected.Add(1)
			c.after(c.real(c.hbEvery), func() {
				if c.online {
					c.sendBoot(hb)
				}
			})
			return
		}
		c.m.BootAccepted.Add(1)
		c.booted = true
		if !c.ready.Swap(true) {
			c.m.Ready.Add(1)
		}
		c.call("StatusNotification", ocpp.StatusNotificationReq{
			ConnectorID: 0, ErrorCode: "NoError", Status: "Available", Timestamp: ocpp.Now()}, nil)
		for _, k := range c.conns {
			ec := "NoError"
			if k.status == "Faulted" {
				ec = "GroundFailure"
			}
			c.call("StatusNotification", ocpp.StatusNotificationReq{
				ConnectorID: k.id, ErrorCode: ec, Status: k.status, Timestamp: ocpp.Now()}, nil)
		}
		c.updateFree()
		c.flushQueue()
		hb.Reset(c.real(c.hbEvery) + c.jitter(0, c.real(c.hbEvery)/4))
	})
}

// ---- inbound ----

func (c *Charger) handleFrame(f *ocpp.Frame) {
	switch f.Type {
	case ocpp.CallResult, ocpp.CallError:
		p, ok := c.pend[f.ID]
		if !ok {
			return
		}
		delete(c.pend, f.ID)
		c.m.Latency(p.action).Observe(time.Since(p.sent))
		if f.Type == ocpp.CallError {
			c.m.CallErrors.Add(1)
			c.m.Outcome(p.action, "callerror:"+f.ErrCode)
		} else {
			c.m.Outcome(p.action, "ok")
		}
		if p.cb != nil {
			p.cb(f, nil)
		}
	case ocpp.Call:
		c.answerServerCall(f)
	}
}

// answerServerCall gives plausible replies to CSMS-initiated calls so chargers stay well-behaved.
func (c *Charger) answerServerCall(f *ocpp.Frame) {
	var res any
	switch f.Action {
	case "ChangeAvailability", "ChangeConfiguration", "ClearCache", "Reset":
		res = map[string]string{"status": "Accepted"}
	case "UnlockConnector":
		res = map[string]string{"status": "Unlocked"}
	case "GetConfiguration":
		res = map[string]any{"configurationKey": []any{}}
	case "RemoteStartTransaction", "RemoteStopTransaction":
		res = map[string]string{"status": "Rejected"}
	default:
		b := []byte(`[4,"` + f.ID + `","NotSupported","simulator does not implement ` + f.Action + `",{}]`)
		_ = c.conn.WriteMessage(websocket.TextMessage, b)
		return
	}
	b, _ := ocpp.EncodeResult(f.ID, res)
	if c.conn.WriteMessage(websocket.TextMessage, b) == nil {
		c.m.MsgSent.Add(1)
	}
}

func (c *Charger) expirePending() {
	cut := time.Now().Add(-c.cfg.CallTimeout)
	for id, p := range c.pend {
		if p.sent.Before(cut) {
			delete(c.pend, id)
			c.m.CallTimeouts.Add(1)
			c.m.Outcome(p.action, "timeout")
			if p.cb != nil {
				p.cb(nil, errTimeout)
			}
		}
	}
}

func (c *Charger) failPending() {
	for id, p := range c.pend {
		delete(c.pend, id)
		c.m.Outcome(p.action, "conn_lost")
		if p.cb != nil {
			p.cb(nil, errOffline)
		}
	}
}

// ---- commands ----

func (c *Charger) handleCmd(cmd Cmd) {
	switch cmd.Kind {
	case CmdDrop:
		if c.online {
			c.m.InjectedDrops.Add(1)
			_ = c.conn.UnderlyingConn().Close()
		}
	case CmdFault:
		k := c.idleConnector()
		if k == nil || !c.booted {
			return
		}
		c.m.InjectedFaults.Add(1)
		codes := []string{"ConnectorLockFailure", "GroundFailure", "OverCurrentFailure", "PowerSwitchFailure", "OtherError"}
		c.status(k, "Faulted", codes[c.rng.Intn(len(codes))])
		c.after(c.real(cmd.Duration), func() {
			if k.sess == nil && k.status == "Faulted" {
				c.status(k, "Available", "NoError")
			}
		})
	case CmdStartSession:
		k := c.idleConnector()
		if k == nil || !c.booted {
			c.m.SessionsSkipped.Add(1)
			if cmd.Release != nil {
				cmd.Release()
			}
			return
		}
		c.beginSession(k, cmd)
	}
}

func (c *Charger) idleConnector() *connector {
	start := c.rng.Intn(len(c.conns))
	for i := range c.conns {
		k := c.conns[(start+i)%len(c.conns)]
		if k.status == "Available" && k.sess == nil {
			return k
		}
	}
	return nil
}

// ---- session lifecycle ----

func (c *Charger) beginSession(k *connector, cmd Cmd) {
	s := &session{
		idTag: cmd.IDTag, stage: stPreparing, plannedAt: cmd.Duration, release: cmd.Release,
		powerFrac: 0.55 + 0.45*c.rng.Float64(),
		capWh:     (35 + 55*c.rng.Float64()) * 1000,
		socPct:    8 + 35*c.rng.Float64(),
	}
	k.sess = s
	c.status(k, "Preparing", "NoError")
	// driver plugs in and presents the tag a moment later
	c.after(c.real(c.jitter(500*time.Millisecond, 4*time.Second)), func() { c.authorize(k, s) })
}

func (c *Charger) abort(k *connector, s *session, counted bool) {
	if k.sess != s {
		return
	}
	if counted {
		c.m.SessionsAborted.Add(1)
	}
	c.finish(k, s)
	c.status(k, "Available", "NoError")
}

func (c *Charger) finish(k *connector, s *session) {
	k.sess = nil
	if s.release != nil {
		s.release()
		s.release = nil
	}
	c.updateFree()
}

func (c *Charger) authorize(k *connector, s *session) {
	if k.sess != s {
		return
	}
	s.stage = stAuthorizing
	c.call("Authorize", ocpp.AuthorizeReq{IDTag: s.idTag}, func(f *ocpp.Frame, err error) {
		if k.sess != s {
			return
		}
		var res ocpp.AuthorizeRes
		if err != nil || f.Type != ocpp.CallResult || jsonUnmarshal(f.Payload, &res) != nil {
			c.abort(k, s, true)
			return
		}
		if res.IDTagInfo.Status != "Accepted" {
			c.m.AuthRejected.Add(1)
			c.abort(k, s, false)
			return
		}
		c.startTransaction(k, s)
	})
}

func (c *Charger) startTransaction(k *connector, s *session) {
	s.stage = stStarting
	c.call("StartTransaction", ocpp.StartTransactionReq{
		ConnectorID: k.id, IDTag: s.idTag, MeterStart: int(c.meterBase), Timestamp: ocpp.Now(),
	}, func(f *ocpp.Frame, err error) {
		if k.sess != s {
			return
		}
		var res ocpp.StartTransactionRes
		if err != nil || f.Type != ocpp.CallResult || jsonUnmarshal(f.Payload, &res) != nil || res.IDTagInfo.Status != "Accepted" {
			c.abort(k, s, true)
			return
		}
		s.txID = res.TransactionID
		s.stage = stCharging
		s.meterWh = c.meterBase
		c.m.SessionsStarted.Add(1)
		c.m.SessionsDelta(1)
		c.status(k, "Charging", "NoError")
		c.after(c.real(c.prof.MeterInterval), func() { c.meterTick(k, s) })
		c.after(c.real(s.plannedAt), func() { c.endSession(k, s, "") })
	})
}

// power returns the instantaneous charging power (W): flat for AC, tapering above 80% SoC for DC.
func (c *Charger) power(s *session) float64 {
	p := c.prof.MaxPowerW * s.powerFrac
	if s.socPct > 80 {
		p *= math.Max(0.1, 1-(s.socPct-80)/22)
	}
	return p
}

func (c *Charger) meterTick(k *connector, s *session) {
	if k.sess != s || s.stage != stCharging {
		return
	}
	step := c.prof.MeterInterval
	p := c.power(s)
	wh := p * step.Hours()
	// whole Wh only: the CSMS rounds readings but truncates the stop meter, and a reading above the stop meter trips its auto-close rule
	s.meterWh = math.Round(s.meterWh + wh)
	s.socPct = math.Min(100, s.socPct+wh/s.capWh*100)
	s.elapsed += step

	sv := func(meas, unit string, v float64) ocpp.SampledValue {
		return ocpp.SampledValue{Value: ocpp.Num(v), Context: "Sample.Periodic", Format: "Raw", Measurand: meas, Location: "Outlet", Unit: unit}
	}
	volts := c.prof.Voltage
	vals := []ocpp.SampledValue{
		sv("Energy.Active.Import.Register", "Wh", s.meterWh),
		sv("Power.Active.Import", "W", p),
		sv("Current.Import", "A", p/volts),
		sv("Voltage", "V", volts),
	}
	if c.prof.DC {
		vals = append(vals, sv("SoC", "Percent", s.socPct))
	}
	tx := s.txID
	c.callQueueable("MeterValues", ocpp.MeterValuesReq{
		ConnectorID: k.id, TransactionID: &tx,
		MeterValue: []ocpp.MeterValue{{Timestamp: ocpp.Now(), SampledValue: vals}},
	}, nil)

	if s.socPct >= 100 {
		c.endSession(k, s, "EVDisconnected")
		return
	}
	c.after(c.real(step), func() { c.meterTick(k, s) })
}

func (c *Charger) endSession(k *connector, s *session, reason string) {
	if k.sess != s || s.stage != stCharging {
		return
	}
	if reason == "" {
		reason = "Local"
		if c.rng.Intn(10) < 3 {
			reason = "EVDisconnected"
		}
	}
	s.stage = stPreparing // no further meter ticks
	c.status(k, "Finishing", "NoError")
	c.meterBase = s.meterWh
	c.m.SessionsDelta(-1)
	c.m.SessionsDone.Add(1)
	c.m.PendingStops.Add(1) // released when the stop is answered, fails, or is queued, so the run can drain cleanly
	c.after(c.real(c.jitter(time.Second, 4*time.Second)), func() {
		c.callQueueable("StopTransaction", ocpp.StopTransactionReq{
			IDTag: s.idTag, MeterStop: int(s.meterWh), Timestamp: ocpp.Now(), TransactionID: s.txID, Reason: reason,
		}, func(*ocpp.Frame, error) { c.m.PendingStops.Add(-1) })
		if k.sess == s {
			c.finish(k, s)
			c.status(k, "Available", "NoError")
		}
	})
}
