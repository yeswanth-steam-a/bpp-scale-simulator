// Package fleet plans the day's charging sessions and drives the charger population through them.
package fleet

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/steam-a/bpp-load-simulator/internal/config"
)

type Planned struct {
	At       time.Duration // simulated offset from the start of the plan
	Profile  *config.Profile
	Duration time.Duration // simulated session length
}

// BuildPlan draws cfg.Sessions.Total session starts from the (piecewise-constant) demand curve.
// Every shard calls this with the same seed and then keeps its own slice, so shards need no coordination.
func BuildPlan(cfg *config.Config) []Planned {
	rng := rand.New(rand.NewSource(cfg.Seed))
	curve := cfg.Sessions.Curve
	var sum float64
	for _, w := range curve {
		sum += w
	}
	cum := make([]float64, len(curve))
	var acc float64
	for i, w := range curve {
		acc += w / sum
		cum[i] = acc
	}
	slot := cfg.Duration / time.Duration(len(curve))

	var wsum float64
	for _, p := range cfg.Profiles {
		wsum += p.SessionWeight
	}

	plan := make([]Planned, 0, cfg.Sessions.Total)
	for n := 0; n < cfg.Sessions.Total; n++ {
		u := rng.Float64()
		i := sort.SearchFloat64s(cum, u)
		if i >= len(curve) {
			i = len(curve) - 1
		}
		prev := 0.0
		if i > 0 {
			prev = cum[i-1]
		}
		frac := 0.0
		if cum[i] > prev {
			frac = (u - prev) / (cum[i] - prev)
		}
		at := time.Duration(i)*slot + time.Duration(frac*float64(slot))

		pick := rng.Float64() * wsum
		prof := &cfg.Profiles[len(cfg.Profiles)-1]
		var pa float64
		for k := range cfg.Profiles {
			pa += cfg.Profiles[k].SessionWeight
			if pick <= pa {
				prof = &cfg.Profiles[k]
				break
			}
		}
		plan = append(plan, Planned{At: at, Profile: prof, Duration: sessionLength(rng, prof)})
	}
	sort.Slice(plan, func(a, b int) bool { return plan[a].At < plan[b].At })
	return plan
}

func sessionLength(rng *rand.Rand, p *config.Profile) time.Duration {
	sigma := p.SessionSigma
	if sigma <= 0 {
		sigma = 0.4
	}
	d := time.Duration(float64(p.SessionMean) * math.Exp(rng.NormFloat64()*sigma))
	if p.SessionMin > 0 && d < p.SessionMin {
		d = p.SessionMin
	}
	if p.SessionMax > 0 && d > p.SessionMax {
		d = p.SessionMax
	}
	return d
}

type PlanStats struct {
	Total        int
	PeakParallel int
	PeakAt       time.Duration
	HourlyStarts []int
}

// Stats reports what the plan will demand of the CSMS, assuming every session starts on time.
func Stats(plan []Planned, total time.Duration) PlanStats {
	type ev struct {
		at time.Duration
		d  int
	}
	evs := make([]ev, 0, 2*len(plan))
	hours := make([]int, int(math.Ceil(total.Hours())))
	for _, s := range plan {
		evs = append(evs, ev{s.At, 1}, ev{s.At + s.Duration, -1})
		if h := int(s.At.Hours()); h < len(hours) {
			hours[h]++
		}
	}
	sort.Slice(evs, func(a, b int) bool {
		if evs[a].at == evs[b].at {
			return evs[a].d < evs[b].d
		}
		return evs[a].at < evs[b].at
	})
	st := PlanStats{Total: len(plan), HourlyStarts: hours}
	cur := 0
	for _, e := range evs {
		cur += e.d
		if cur > st.PeakParallel {
			st.PeakParallel, st.PeakAt = cur, e.at
		}
	}
	return st
}

// Shard keeps every count-th planned session starting at idx.
func Shard(plan []Planned, idx, count int) []Planned {
	if count <= 1 {
		return plan
	}
	out := make([]Planned, 0, len(plan)/count+1)
	for i := idx; i < len(plan); i += count {
		out = append(out, plan[i])
	}
	return out
}
