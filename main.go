// A metrics history module for the modularnost panel: it polls the core's
// /internal/stats, keeps the numbers in SQLite and sends them as pages of
// blocks (a table, charts) that the panel draws.
//
// The core deliberately keeps no history (architecture.md, ADR 5) вЂ” that is
// this module's whole reason to exist. It has no docker.sock of its own; every
// number here came from GET /internal/stats with the stats:read permission.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var (
	core      = or(os.Getenv("PANEL_CORE_URL"), "http://panel:8080")
	dbPath    = or(os.Getenv("DB_PATH"), "/data/metrics.db")
	interval  = dur(os.Getenv("SAMPLE_INTERVAL"), 30*time.Second)
	retention = dur(os.Getenv("RETENTION"), 7*24*time.Hour)

	// [module, token]. Learned from the core's own requests, the manifest fetch
	// included, so sampling starts within a discovery TTL of either side booting
	// and survives the core changing its secret.
	creds atomic.Pointer[[2]string]
)

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func dur(v string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Contract 2 of the panel (docs/modules.md there): the menu entry, and a tab on
// every service's page, both pages of blocks the panel draws. Viewers may see
// the numbers: the panel's own Resources page shows them to a viewer too.
const manifest = `{
  "contract": 2,
  "name": "metrics",
  "version": "0.3.0",
  "contributions": [
    {"slot": "nav", "title": "Metrics", "path": "/", "role": "viewer", "render": "ui"},
    {"slot": "service.tab", "title": "Metrics", "path": "/service", "role": "viewer", "render": "ui"}
  ],
  "permissions": ["stats:read"]
}`

type stat struct {
	Node       string  `json:"node"`
	Service    string  `json:"service"`
	Stack      string  `json:"stack"`
	Slot       int     `json:"slot"`
	CPU        float64 `json:"cpu"`
	Mem        uint64  `json:"mem"`
	MemLimit   uint64  `json:"mem_limit"`
	NetRX      uint64  `json:"net_rx"`
	NetTX      uint64  `json:"net_tx"`
	BlockRead  uint64  `json:"block_read"`
	BlockWrite uint64  `json:"block_write"`
	Err        string  `json:"err"`
}

// ponytail: one row per service per tick, replicas summed at write time. Raw
// per-replica history would multiply the rows by the replica count to answer
// "which replica is hot" вЂ” which the panel's own Resources page already does.
// Add downsampling into hourly rows when the retention window outgrows a week.
const schema = `
CREATE TABLE IF NOT EXISTS samples (
  ts        INTEGER NOT NULL,
  service   TEXT    NOT NULL,
  stack     TEXT    NOT NULL DEFAULT '',
  cpu       REAL    NOT NULL,
  mem       INTEGER NOT NULL,
  mem_limit INTEGER NOT NULL,
  net_rx    INTEGER NOT NULL DEFAULT 0,
  net_tx    INTEGER NOT NULL DEFAULT 0,
  block_read  INTEGER NOT NULL DEFAULT 0,
  block_write INTEGER NOT NULL DEFAULT 0,
  replicas  INTEGER NOT NULL,
  PRIMARY KEY (service, ts)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS samples_ts ON samples(ts);`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for _, column := range []string{
		"net_rx INTEGER NOT NULL DEFAULT 0", "net_tx INTEGER NOT NULL DEFAULT 0",
		"block_read INTEGER NOT NULL DEFAULT 0", "block_write INTEGER NOT NULL DEFAULT 0",
	} {
		if _, err := db.Exec("ALTER TABLE samples ADD COLUMN " + column); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return nil, err
		}
	}
	return db, nil
}

func main() {
	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("cannot open %s: %v", dbPath, err)
	}
	defer db.Close()

	go sampler(db)

	http.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		remember(r) // discovery carries the token: this is where it arrives first
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, manifest)
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	http.HandleFunc("/service", func(w http.ResponseWriter, r *http.Request) {
		remember(r)
		servicePage(w, db, r)
	})
	http.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		remember(r)
		overviewPage(w, db, r)
	})

	addr := or(os.Getenv("ADDR"), ":8080")
	log.Printf("metrics-module on %s, core %s, db %s, every %s, keeping %s", addr, core, dbPath, interval, retention)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func remember(r *http.Request) {
	if m := r.Header.Get("X-Panel-Module"); m != "" {
		creds.Store(&[2]string{m, r.Header.Get("X-Panel-Token")})
	}
}

func sampler(db *sql.DB) {
	for ; ; time.Sleep(interval) {
		stats, err := fetchStats()
		if err != nil {
			log.Printf("sample skipped: %v", err)
			continue
		}
		if err := save(db, time.Now(), stats); err != nil {
			log.Printf("sample not stored: %v", err)
			continue
		}
		// Nobody polls: the browsers that have a page open ask again when told.
		for _, page := range []string{"/", "/service"} {
			if err := changed(page); err != nil {
				log.Printf("telling the panel about %s: %v", page, err)
			}
		}
	}
}

func fetchStats() ([]stat, error) {
	c := creds.Load()
	if c == nil {
		return nil, fmt.Errorf("no token yet: the core has not fetched /manifest")
	}
	req, err := http.NewRequest(http.MethodGet, core+"/internal/stats", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Panel-Module", c[0])
	req.Header.Set("X-Panel-Token", c[1])
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var stats []stat
	return stats, json.NewDecoder(resp.Body).Decode(&stats)
}

// save folds the per-replica rows into one row per service and drops whatever
// fell out of the retention window.
func save(db *sql.DB, now time.Time, stats []stat) error {
	type agg struct {
		stack                 string
		cpu                   float64
		mem, limit            uint64
		netRX, netTX          uint64
		blockRead, blockWrite uint64
		count                 uint64
	}
	byService := map[string]*agg{}
	for _, s := range stats {
		if s.Err != "" || s.Service == "" {
			continue // an unreachable node is not idle; storing it as zero would lie
		}
		a := byService[s.Service]
		if a == nil {
			a = &agg{stack: s.Stack}
			byService[s.Service] = a
		}
		a.cpu += s.CPU
		a.mem += s.Mem
		a.netRX += s.NetRX
		a.netTX += s.NetTX
		a.blockRead += s.BlockRead
		a.blockWrite += s.BlockWrite
		// The limit is per replica вЂ” summing it would claim a service with two
		// unlimited replicas on one node may use that node's memory twice.
		// ponytail: max, so replicas on unequal nodes read as the largest one.
		if s.MemLimit > a.limit {
			a.limit = s.MemLimit
		}
		a.count++
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR REPLACE INTO samples VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	ts := now.Unix()
	for name, a := range byService {
		if _, err := ins.Exec(ts, name, a.stack, a.cpu, a.mem, a.limit, a.netRX, a.netTX, a.blockRead, a.blockWrite, a.count); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM samples WHERE ts < ?`, now.Add(-retention).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

type point struct {
	T int64
	V float64
}

type row struct {
	Service  string
	Stack    string
	CPU      float64
	Mem      uint64
	MemLimit uint64
	Replicas int
	Age      string
}

// latest returns the last stored sample of every service, hungriest first.
func latest(db *sql.DB, now time.Time) ([]row, error) {
	rs, err := db.Query(`
		SELECT s.service, s.stack, s.cpu, s.mem, s.mem_limit, s.replicas, s.ts
		FROM samples s
		JOIN (SELECT service, MAX(ts) ts FROM samples GROUP BY service) m
		  ON m.service = s.service AND m.ts = s.ts`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		var ts int64
		if err := rs.Scan(&r.Service, &r.Stack, &r.CPU, &r.Mem, &r.MemLimit, &r.Replicas, &ts); err != nil {
			return nil, err
		}
		r.Age = since(now.Unix() - ts)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CPU > out[j].CPU })
	return out, rs.Err()
}

// series buckets a service's history so a chart never draws more points than a
// screen can show, whatever the range.
func series(db *sql.DB, service string, from, to time.Time, buckets int) (cpu, mem []point, err error) {
	width := (to.Unix() - from.Unix()) / int64(buckets)
	if width < 1 {
		width = 1
	}
	rs, err := db.Query(`
		SELECT (ts/?)*? AS b, AVG(cpu), AVG(mem)
		FROM samples WHERE service = ? AND ts >= ? AND ts <= ?
		GROUP BY b ORDER BY b`, width, width, service, from.Unix(), to.Unix())
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var t int64
		var c, m float64
		if err := rs.Scan(&t, &c, &m); err != nil {
			return nil, nil, err
		}
		cpu = append(cpu, point{t, c})
		mem = append(mem, point{t, m})
	}
	return cpu, mem, rs.Err()
}

// counterSeries turns cumulative Docker counters into bytes per second. A
// restarted container resets its counter; that interval is shown as zero.
func counterSeries(db *sql.DB, service, column string, from, to time.Time, buckets int) ([]point, error) {
	if column != "net_rx" && column != "net_tx" && column != "block_read" && column != "block_write" {
		return nil, fmt.Errorf("unknown counter %q", column)
	}
	width := (to.Unix() - from.Unix()) / int64(buckets)
	if width < 1 {
		width = 1
	}
	rs, err := db.Query(`SELECT (ts/?)*? AS b, AVG(`+column+`)
		FROM samples WHERE service = ? AND ts >= ? AND ts <= ?
		GROUP BY b ORDER BY b`, width, width, service, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []point
	var prev point
	for rs.Next() {
		var current point
		if err := rs.Scan(&current.T, &current.V); err != nil {
			return nil, err
		}
		if prev.T != 0 && current.T > prev.T {
			v := (current.V - prev.V) / float64(current.T-prev.T)
			if v < 0 {
				v = 0
			}
			out = append(out, point{current.T, v})
		}
		prev = current
	}
	return out, rs.Err()
}

func since(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds ago", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm ago", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh ago", sec/3600)
	default:
		return fmt.Sprintf("%dd ago", sec/86400)
	}
}

func human(b float64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	div, exp := unit, 0
	for n := b / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", b/div, "KMGTPE"[exp])
}

func humanU(b uint64) string { return human(float64(b)) }

var ranges = []struct {
	Label string
	D     time.Duration
}{
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

func rangeOf(s string) (string, time.Duration) {
	for _, r := range ranges {
		if r.Label == s {
			return r.Label, r.D
		}
	}
	return "6h", 6 * time.Hour
}

// A page is a document of blocks the panel draws (docs/modules.md there); this
// module sends numbers and units, and the panel formats them.
type block map[string]any

type document struct {
	UI     int     `json:"ui"`
	Blocks []block `json:"blocks"`
}

func write(w http.ResponseWriter, d document) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// changed tells the panel that a page of ours is different now.
func changed(page string) error {
	c := creds.Load()
	if c == nil {
		return fmt.Errorf("no token yet")
	}
	req, err := http.NewRequest(http.MethodPost, core+"/internal/ui/changed", bytes.NewBufferString(fmt.Sprintf(`{"path":%q}`, page)))
	if err != nil {
		return err
	}
	req.Header.Set("X-Panel-Module", c[0])
	req.Header.Set("X-Panel-Token", c[1])
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

func overviewDoc(rows []row, warn string) document {
	var cpu float64
	var mem uint64
	table := make([]block, 0, len(rows))
	for _, r := range rows {
		cpu += r.CPU
		mem += r.Mem
		memory := humanU(r.Mem)
		if r.MemLimit > 0 {
			memory += " / "
			if r.Replicas > 1 {
				memory += fmt.Sprintf("%d × ", r.Replicas)
			}
			memory += humanU(r.MemLimit)
		}
		table = append(table, block{
			"service":  block{"text": r.Service, "href": block{"path": "/service?name=" + url.QueryEscape(r.Service)}},
			"stack":    r.Stack,
			"replicas": r.Replicas,
			"cpu":      r.CPU,
			"memory":   memory,
			"sampled":  r.Age,
		})
	}
	blocks := []block{}
	if warn != "" {
		blocks = append(blocks, block{"type": "banner", "tone": "warning", "text": warn})
	}
	return document{UI: 1, Blocks: append(blocks,
		block{"type": "stat", "label": "Services", "value": len(rows)},
		block{"type": "stat", "label": "CPU", "value": cpu, "unit": "percent"},
		block{"type": "stat", "label": "Memory", "value": mem, "unit": "bytes"},
		block{"type": "table", "empty": "Nothing sampled yet.", "rows": table, "columns": []block{
			{"key": "service", "title": "Service"},
			{"key": "stack", "title": "Stack"},
			{"key": "replicas", "title": "Replicas", "type": "number"},
			{"key": "cpu", "title": "CPU", "type": "percent"},
			{"key": "memory", "title": "Memory", "align": "right"},
			{"key": "sampled", "title": "Sampled"},
		}},
	)}
}

func overviewPage(w http.ResponseWriter, db *sql.DB, _ *http.Request) {
	rows, err := latest(db, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	warn := ""
	if creds.Load() == nil {
		warn = "No token yet: waiting for the core to fetch /manifest."
	}
	write(w, overviewDoc(rows, warn))
}

// serviceOf is the service a request asks about, and whether the page is a tab
// on that service: the panel passes ?service= to a tab, our own links use ?name=.
func serviceOf(r *http.Request) (name string, tab bool) {
	if name = r.URL.Query().Get("service"); name != "" {
		return name, true
	}
	return r.URL.Query().Get("name"), false
}

func points(in []point) [][2]float64 {
	out := make([][2]float64, len(in))
	for i, p := range in {
		out[i] = [2]float64{float64(p.T), p.V}
	}
	return out
}

// serviceDoc is one service's history: a range to pick, and a chart per number.
// They share one time axis, so a hover compares every metric at once. A tab
// does not repeat the service's name or link out of it, the panel already does.
func serviceDoc(db *sql.DB, name string, tab bool, rangeLabel string, now time.Time) (document, error) {
	label, d := rangeOf(rangeLabel)
	from := now.Add(-d)
	const buckets = 180

	cpu, mem, err := series(db, name, from, now, buckets)
	if err != nil {
		return document{}, err
	}
	chart := func(title, unit string, pts []point) block {
		return block{"type": "chart", "title": title, "unit": unit, "sync": "time", "from": from.Unix(), "to": now.Unix(),
			"empty": "No samples in this range.", "series": []block{{"name": title, "points": points(pts)}}}
	}
	charts := []block{chart("CPU, % of one core", "percent", cpu), chart("Memory", "bytes", mem)}
	for _, c := range []struct{ title, column string }{
		{"Network receive", "net_rx"}, {"Network transmit", "net_tx"}, {"Block read", "block_read"}, {"Block write", "block_write"},
	} {
		pts, err := counterSeries(db, name, c.column, from, now, buckets)
		if err != nil {
			return document{}, err
		}
		charts = append(charts, chart(c.title, "bytes/s", pts))
	}

	options := make([]block, len(ranges))
	for i, r := range ranges {
		options[i] = block{"value": r.Label, "label": r.Label}
	}
	blocks := []block{}
	if !tab {
		blocks = append(blocks, block{"type": "link", "text": "All services", "href": block{"path": "/"}}, block{"type": "heading", "level": 1, "text": name})
	}
	blocks = append(blocks, block{"type": "choice", "param": "range", "value": label, "options": options})
	return document{UI: 1, Blocks: append(blocks, charts...)}, nil
}

func servicePage(w http.ResponseWriter, db *sql.DB, r *http.Request) {
	name, tab := serviceOf(r)
	if name == "" {
		write(w, document{UI: 1, Blocks: []block{{"type": "banner", "tone": "warning", "text": "Open a service from the list to see its history."}}})
		return
	}
	doc, err := serviceDoc(db, name, tab, r.URL.Query().Get("range"), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	write(w, doc)
}
