// Command simulator runs the OCPP 1.6J charger fleet against a CSMS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/steam-a/bpp-load-simulator/internal/config"
	"github.com/steam-a/bpp-load-simulator/internal/fleet"
	"github.com/steam-a/bpp-load-simulator/internal/metrics"
)

func main() {
	var (
		cfgPath   = flag.String("config", "configs/default.yaml", "path to the run configuration")
		shardIdx  = flag.Int("shard-index", 0, "this load generator's shard (0-based)")
		shardCnt  = flag.Int("shard-count", 1, "total number of load generator shards")
		planOnly  = flag.Bool("plan", false, "print the session plan statistics and exit without connecting")
		target    = flag.String("target", "", "override target (ws://host:port/csms/)")
		chargers  = flag.Int("chargers", 0, "override fleet size")
		duration  = flag.Duration("duration", 0, "override run duration")
		timeScale = flag.Float64("time-scale", 0, "override time scale (e.g. 144 plays 24h in 10 minutes)")
	)
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	if *target != "" {
		cfg.Target = *target
	}
	if *chargers > 0 {
		cfg.Chargers = *chargers
	}
	if *duration > 0 {
		cfg.Duration = *duration
	}
	if *timeScale > 0 {
		cfg.TimeScale = *timeScale
	}
	if *shardCnt < 1 || *shardIdx < 0 || *shardIdx >= *shardCnt {
		log.Fatalf("invalid shard %d/%d", *shardIdx, *shardCnt)
	}

	if *planOnly {
		printPlan(cfg)
		return
	}

	m := metrics.New()
	f := fleet.New(cfg, m, *shardIdx, *shardCnt)
	log.Printf("shard %d/%d: %d chargers -> %s", *shardIdx, *shardCnt, f.Size(), cfg.Target)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { m.WritePrometheus(w) })
		go func() { log.Println(http.ListenAndServe(cfg.MetricsAddr, mux)) }()
	}
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				log.Println(m.Line())
			case <-ctx.Done():
				return
			}
		}
	}()

	f.Run(ctx)

	log.Println("run finished:", m.Line())
	if cfg.SummaryFile != "" {
		out, err := os.Create(fmt.Sprintf("%s.shard%d", cfg.SummaryFile, *shardIdx))
		if err != nil {
			log.Fatal(err)
		}
		defer out.Close()
		if err := m.WriteJSON(out); err != nil {
			log.Fatal(err)
		}
		log.Println("summary written to", out.Name())
	}
}

func printPlan(cfg *config.Config) {
	plan := fleet.BuildPlan(cfg)
	st := fleet.Stats(plan, cfg.Duration)
	fmt.Printf("sessions planned: %d\npeak parallel sessions: %d (at %s)\nsession starts per hour:\n",
		st.Total, st.PeakParallel, st.PeakAt.Truncate(time.Minute))
	for h, n := range st.HourlyStarts {
		fmt.Printf("  %02d:00  %5d\n", h, n)
	}
}
