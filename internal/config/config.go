// Package config loads the YAML run configuration shared by the simulator and the seed generator.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Target       string `yaml:"target"`    // e.g. ws://host:9898/csms/
	Chargers     int    `yaml:"chargers"`  // total fleet size across all shards
	IDPrefix     string `yaml:"id_prefix"` // charge point id = prefix + zero-padded index
	IDsFile      string `yaml:"ids_file"`  // optional: use these existing charge point ids (one per line) instead of generating them
	ids          []string
	idConns      map[string]int
	Duration     time.Duration `yaml:"duration"`      // simulated wall-clock length of the run
	TimeScale    float64       `yaml:"time_scale"`    // >1 compresses time (smoke tests); 1 = real time
	Seed         int64         `yaml:"seed"`          // RNG seed, makes the session plan reproducible
	ConnectRate  float64       `yaml:"connect_rate"`  // new WebSocket connections per second during ramp-up
	ReconnectMin time.Duration `yaml:"reconnect_min"` // backoff bounds for dropped chargers
	ReconnectMax time.Duration `yaml:"reconnect_max"`
	CallTimeout  time.Duration `yaml:"call_timeout"` // no CALLRESULT within this => counted as timeout
	Warmup       time.Duration `yaml:"warmup"`       // after the chargers are connected, idle (heartbeats only) this long before the first session
	MetricsAddr  string        `yaml:"metrics_addr"` // "" disables the HTTP endpoint
	SummaryFile  string        `yaml:"summary_file"` // JSON summary written at the end
	Sessions     Sessions      `yaml:"sessions"`
	IDTags       IDTags        `yaml:"idtags"`
	Noise        Noise         `yaml:"noise"`
	Profiles     []Profile     `yaml:"profiles"`
}

type Sessions struct {
	Total int       `yaml:"total"` // sessions over the whole run, all shards
	Curve []float64 `yaml:"curve"` // relative start-rate weights, evenly spread over the run (24 hourly values by default)
}

type IDTags struct {
	Prefix     string   `yaml:"prefix"`
	Pool       int      `yaml:"pool"`
	InvalidPct float64  `yaml:"invalid_pct"` // share of sessions that use a tag the CSMS will not accept
	List       []string `yaml:"list"`        // optional: use exactly these existing tags instead of generating a pool (each is held by one session at a time)
	File       string   `yaml:"file"`        // optional: file of existing tags, one per line (VID:* virtual ids are skipped; see loadTags)
	Shared     bool     `yaml:"shared"`      // allow several concurrent sessions to use the same tag (the CSMS allows it)
}

type Noise struct {
	DisconnectsPerChargerPerDay float64       `yaml:"disconnects_per_charger_per_day"`
	FaultsPerChargerPerDay      float64       `yaml:"faults_per_charger_per_day"`
	FaultMin                    time.Duration `yaml:"fault_min"`
	FaultMax                    time.Duration `yaml:"fault_max"`
}

// Profile describes one charger type. Share is the fraction of the fleet (shares are normalised).
type Profile struct {
	Name          string        `yaml:"name"`
	Share         float64       `yaml:"share"`
	Vendor        string        `yaml:"vendor"`
	Model         string        `yaml:"model"`
	Connectors    int           `yaml:"connectors"`
	DC            bool          `yaml:"dc"`
	MaxPowerW     float64       `yaml:"max_power_w"`
	Voltage       float64       `yaml:"voltage"`
	MeterInterval time.Duration `yaml:"meter_interval"`
	SessionMean   time.Duration `yaml:"session_mean"`  // log-normal median
	SessionSigma  float64       `yaml:"session_sigma"` // log-normal sigma
	SessionMin    time.Duration `yaml:"session_min"`
	SessionMax    time.Duration `yaml:"session_max"`
	SessionWeight float64       `yaml:"session_weight"` // relative chance a planned session lands on this profile
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.IDsFile != "" {
		if err := c.loadIDs(); err != nil {
			return err
		}
		if c.Chargers <= 0 || c.Chargers > len(c.ids) {
			c.Chargers = len(c.ids)
		}
	}
	if c.IDTags.File != "" {
		if err := c.loadTags(); err != nil {
			return err
		}
	}
	if len(c.IDTags.List) > 0 {
		c.IDTags.Pool = len(c.IDTags.List)
	}
	if c.Target == "" || c.Chargers <= 0 || len(c.Profiles) == 0 {
		return fmt.Errorf("config needs target, chargers > 0 and at least one profile")
	}
	if c.TimeScale <= 0 {
		c.TimeScale = 1
	}
	if c.ConnectRate <= 0 {
		c.ConnectRate = 100
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 30 * time.Second
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = time.Second
	}
	if c.ReconnectMax < c.ReconnectMin {
		c.ReconnectMax = 60 * time.Second
	}
	if len(c.Sessions.Curve) == 0 {
		return fmt.Errorf("sessions.curve must have at least one weight")
	}
	var shareSum, weightSum float64
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.Connectors <= 0 || p.MaxPowerW <= 0 || p.MeterInterval <= 0 || p.SessionMean <= 0 {
			return fmt.Errorf("profile %q: connectors, max_power_w, meter_interval and session_mean are required", p.Name)
		}
		if p.Voltage == 0 {
			p.Voltage = 230
		}
		shareSum += p.Share
		weightSum += p.SessionWeight
	}
	if shareSum <= 0 || weightSum <= 0 {
		return fmt.Errorf("profile shares and session_weights must be positive")
	}
	return nil
}

// ChargePointID returns the id of the i-th charger (0-based) in the whole fleet.
// ConnectorsFor returns the connector count of fleet charger i: the per-id value from ids_file
// ("id,connectors") when given, otherwise the profile's.
func (c *Config) ConnectorsFor(i int) int {
	if c.ids != nil {
		if n := c.idConns[c.ids[i]]; n > 0 {
			return n
		}
	}
	return c.ProfileFor(i).Connectors
}

func (c *Config) ChargePointID(i int) string {
	if c.ids != nil {
		return c.ids[i]
	}
	return fmt.Sprintf("%s%06d", c.IDPrefix, i+1)
}

// ProfileFor assigns fleet index i to a profile deterministically so the seed generator and the
// simulator agree on how many connectors each charger has.
func (c *Config) ProfileFor(i int) *Profile {
	var total float64
	for _, p := range c.Profiles {
		total += p.Share
	}
	pos := (float64(i) + 0.5) / float64(c.Chargers) * total
	var acc float64
	for k := range c.Profiles {
		acc += c.Profiles[k].Share
		if pos <= acc {
			return &c.Profiles[k]
		}
	}
	return &c.Profiles[len(c.Profiles)-1]
}

// IDTag returns pool tag n (0-based). Tags at the tail of the pool are the intentionally invalid ones.
func (c *Config) IDTag(n int) string {
	if len(c.IDTags.List) > 0 {
		return c.IDTags.List[n]
	}
	return fmt.Sprintf("%s%06d", c.IDTags.Prefix, n+1)
}

func (c *Config) ValidTagCount() int {
	if len(c.IDTags.List) > 0 {
		return len(c.IDTags.List)
	}
	invalid := int(float64(c.IDTags.Pool) * c.IDTags.InvalidPct / 100)
	return c.IDTags.Pool - invalid
}

// UsesIDList reports whether chargers come from ids_file (existing ids; nothing to seed).
func (c *Config) UsesIDList() bool { return c.ids != nil }

var urlSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// loadIDs reads ids_file, dropping blanks, duplicates and ids that are not URL-safe
// (spaces or punctuation would need escaping in the WebSocket path).
func (c *Config) loadIDs() error {
	b, err := os.ReadFile(c.IDsFile)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	c.ids = []string{}
	c.idConns = map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		id := strings.TrimSpace(line)
		conns := 0
		if k := strings.IndexAny(id, ", \t"); k > 0 { // optional "id,connectors" form (also tab/space separated)
			if n, err := strconv.Atoi(strings.TrimSpace(id[k+1:])); err == nil {
				conns = n
				id = strings.TrimSpace(id[:k])
			} // otherwise leave the line alone; the URL-safety check below drops ids with spaces
		}
		if id == "" || seen[id] || !urlSafe.MatchString(id) {
			continue
		}
		seen[id] = true
		c.ids = append(c.ids, id)
		if conns > 0 {
			c.idConns[id] = conns
		}
	}
	if len(c.ids) == 0 {
		return fmt.Errorf("%s contains no usable charge point ids", c.IDsFile)
	}
	return nil
}

// loadTags reads idtags.file into idtags.list. Tags starting with "VID:" are virtual ids used by the
// app/remote-start flow, not RFID cards a charger would present, so they are skipped.
func (c *Config) loadTags() error {
	b, err := os.ReadFile(c.IDTags.File)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || seen[t] || strings.HasPrefix(strings.ToLower(t), "vid:") {
			continue
		}
		seen[t] = true
		c.IDTags.List = append(c.IDTags.List, t)
	}
	if len(c.IDTags.List) == 0 {
		return fmt.Errorf("%s contains no usable idTags", c.IDTags.File)
	}
	return nil
}
