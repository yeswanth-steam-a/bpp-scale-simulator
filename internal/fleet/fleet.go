package fleet

import (
	"context"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/steam-a/bpp-load-simulator/internal/charger"
	"github.com/steam-a/bpp-load-simulator/internal/config"
	"github.com/steam-a/bpp-load-simulator/internal/metrics"
)

type Fleet struct {
	cfg       *config.Config
	m         *metrics.Registry
	shard     int
	shards    int
	chargers  []*charger.Charger
	byProfile map[*config.Profile][]*charger.Charger
	rng       *rand.Rand

	tagMu   sync.Mutex
	freeTag []int
}

func New(cfg *config.Config, m *metrics.Registry, shard, shards int) *Fleet {
	f := &Fleet{
		cfg: cfg, m: m, shard: shard, shards: shards,
		byProfile: map[*config.Profile][]*charger.Charger{},
		rng:       rand.New(rand.NewSource(cfg.Seed ^ int64(shard+1)*7919)),
	}
	gate := rate.NewLimiter(rate.Limit(cfg.ConnectRate), 1)
	for i := shard; i < cfg.Chargers; i += shards {
		p := cfg.ProfileFor(i)
		c := charger.New(cfg.ChargePointID(i), p, cfg.ConnectorsFor(i), cfg, m, gate, cfg.Seed+int64(i)*104729)
		f.chargers = append(f.chargers, c)
		f.byProfile[p] = append(f.byProfile[p], c)
	}
	// each shard owns a disjoint slice of the tag pool so two shards never use one tag at the same time
	for i := shard; i < cfg.IDTags.Pool; i += shards {
		f.freeTag = append(f.freeTag, i)
	}
	return f
}

func (f *Fleet) Size() int { return len(f.chargers) }

func (f *Fleet) acquireTag() (string, func(), bool) {
	f.tagMu.Lock()
	defer f.tagMu.Unlock()
	if len(f.freeTag) == 0 {
		return "", nil, false
	}
	if f.cfg.IDTags.Shared {
		n := f.freeTag[f.rng.Intn(len(f.freeTag))]
		return f.cfg.IDTag(n), func() {}, true
	}
	i := f.rng.Intn(len(f.freeTag))
	n := f.freeTag[i]
	f.freeTag[i] = f.freeTag[len(f.freeTag)-1]
	f.freeTag = f.freeTag[:len(f.freeTag)-1]
	var once sync.Once
	release := func() {
		once.Do(func() {
			f.tagMu.Lock()
			f.freeTag = append(f.freeTag, n)
			f.tagMu.Unlock()
		})
	}
	return f.cfg.IDTag(n), release, true
}

// Run starts every charger, waits for the ramp-up, then plays the session plan and the background
// noise for cfg.Duration. It returns after the run has drained.
func (f *Fleet) Run(ctx context.Context) {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	var wg sync.WaitGroup
	for _, c := range f.chargers {
		wg.Add(1)
		go func(c *charger.Charger) { defer wg.Done(); c.Run(runCtx) }(c)
	}

	f.waitRamp(ctx)
	if ctx.Err() != nil {
		stop()
		wg.Wait()
		return
	}

	if w := time.Duration(float64(f.cfg.Warmup) / f.cfg.TimeScale); w > 0 {
		log.Printf("warm-up: %d chargers connected, idling (heartbeats and status only) for %s before the first session",
			f.m.Connected.Load(), f.cfg.Warmup)
		select {
		case <-time.After(w):
		case <-ctx.Done():
			stop()
			wg.Wait()
			return
		}
	}

	plan := Shard(BuildPlan(f.cfg), f.shard, f.shards)
	f.m.SessionsPlanned.Store(int64(len(plan)))
	log.Printf("ramp complete: %d/%d chargers connected; playing %d sessions over %s (time scale x%g)",
		f.m.Connected.Load(), len(f.chargers), len(plan), f.cfg.Duration, f.cfg.TimeScale)

	start := time.Now()
	go f.noise(runCtx)
	f.dispatch(runCtx, plan, start)

	// drain: let in-flight sessions finish (bounded) so the last StopTransactions are exercised
	deadline := time.Now().Add(f.drainBudget() + 2*time.Minute) // + room for slow StopTransaction replies
	for ctx.Err() == nil && (f.m.ActiveSessions.Load() > 0 || f.m.PendingStops.Load() > 0) && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	stop()
	wg.Wait()
}

func (f *Fleet) drainBudget() time.Duration {
	var max time.Duration
	for _, p := range f.cfg.Profiles {
		if p.SessionMax > max {
			max = p.SessionMax
		}
	}
	if max == 0 || max > 3*time.Hour {
		max = 3 * time.Hour // bound the post-run drain; stragglers are cut off
	}
	return time.Duration(float64(max)/f.cfg.TimeScale) + 30*time.Second
}

func (f *Fleet) waitRamp(ctx context.Context) {
	want := int64(float64(len(f.chargers)) * 0.99)
	timeout := time.Duration(float64(len(f.chargers))/f.cfg.ConnectRate*3)*time.Second + time.Minute
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for f.m.Ready.Load() < want && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	if f.m.Ready.Load() < want {
		log.Printf("WARNING: ramp timed out with %d/%d chargers booted; starting the plan anyway",
			f.m.Ready.Load(), len(f.chargers))
	}
}

func (f *Fleet) dispatch(ctx context.Context, plan []Planned, start time.Time) {
	for _, s := range plan {
		at := start.Add(time.Duration(float64(s.At) / f.cfg.TimeScale))
		if d := time.Until(at); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return
			}
		}
		f.startSession(s)
	}
	// plan done; wait out the remainder of the configured duration so noise keeps running to the end
	if rem := time.Until(start.Add(time.Duration(float64(f.cfg.Duration) / f.cfg.TimeScale))); rem > 0 {
		select {
		case <-time.After(rem):
		case <-ctx.Done():
		}
	}
}

func (f *Fleet) startSession(s Planned) {
	cands := f.byProfile[s.Profile]
	if len(cands) == 0 {
		f.m.SessionsSkipped.Add(1)
		return
	}
	tag, release, ok := f.acquireTag()
	if !ok {
		f.m.SessionsSkipped.Add(1)
		return
	}
	for try := 0; try < 200; try++ {
		c := cands[f.rng.Intn(len(cands))]
		if !c.Ready() || c.FreeSlots() == 0 {
			continue
		}
		select {
		case c.Cmds <- charger.Cmd{Kind: charger.CmdStartSession, Duration: s.Duration, IDTag: tag, Release: release}:
			return
		default:
		}
	}
	release()
	f.m.SessionsSkipped.Add(1)
}

// noise injects low-rate random disconnects and connector faults across the whole run.
func (f *Fleet) noise(ctx context.Context) {
	n := f.cfg.Noise
	perSec := func(perDay float64) float64 {
		return float64(len(f.chargers)) * perDay / 86400 * f.cfg.TimeScale
	}
	drop, fault := perSec(n.DisconnectsPerChargerPerDay), perSec(n.FaultsPerChargerPerDay)
	total := drop + fault
	if total <= 0 {
		return
	}
	rng := rand.New(rand.NewSource(f.cfg.Seed + int64(f.shard)*31))
	for {
		wait := time.Duration(-math.Log(1-rng.Float64()) / total * float64(time.Second))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		c := f.chargers[rng.Intn(len(f.chargers))]
		if !c.Ready() {
			continue
		}
		if rng.Float64() < drop/total {
			select {
			case c.Cmds <- charger.Cmd{Kind: charger.CmdDrop}:
			default:
			}
			continue
		}
		span := n.FaultMax - n.FaultMin
		d := n.FaultMin
		if span > 0 {
			d += time.Duration(rng.Int63n(int64(span)))
		}
		select {
		case c.Cmds <- charger.Cmd{Kind: charger.CmdFault, Duration: d}:
		default:
		}
	}
}
