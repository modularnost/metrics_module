// A metrics history module for the modularnost panel: it polls the core's
// /internal/stats, keeps the numbers in SQLite and draws them.
//
// The core deliberately keeps no history (architecture.md, ADR 5) — that is
// this module's whole reason to exist. It has no docker.sock of its own; every
// number here came from GET /internal/stats with the stats:read permission.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
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

const manifest = `{
  "name": "metrics",
  "version": "0.1.0",
  "nav_entries": [{"title": "Metrics", "path": "/"}],
  "permissions": ["stats:read"]
}`

type stat struct {
	Node     string  `json:"node"`
	Service  string  `json:"service"`
	Stack    string  `json:"stack"`
	Slot     int     `json:"slot"`
	CPU      float64 `json:"cpu"`
	Mem      uint64  `json:"mem"`
	MemLimit uint64  `json:"mem_limit"`
	Err      string  `json:"err"`
}

// ponytail: one row per service per tick, replicas summed at write time. Raw
// per-replica history would multiply the rows by the replica count to answer
// "which replica is hot" — which the panel's own Resources page already does.
// Add downsampling into hourly rows when the retention window outgrows a week.
const schema = `
CREATE TABLE IF NOT EXISTS samples (
  ts        INTEGER NOT NULL,
  service   TEXT    NOT NULL,
  stack     TEXT    NOT NULL DEFAULT '',
  cpu       REAL    NOT NULL,
  mem       INTEGER NOT NULL,
  mem_limit INTEGER NOT NULL,
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
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
		stack             string
		cpu               float64
		mem, limit, count uint64
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
		// The limit is per replica — summing it would claim a service with two
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
	ins, err := tx.Prepare(`INSERT OR REPLACE INTO samples VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	ts := now.Unix()
	for name, a := range byService {
		if _, err := ins.Exec(ts, name, a.stack, a.cpu, a.mem, a.limit, a.count); err != nil {
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

func pct(v float64) string { return fmt.Sprintf("%.1f %%", v) }

func round(v float64) float64 { return math.Round(v*10) / 10 }

// figure wraps a chart in the element the hover script drives: the points it
// snaps to travel in the markup, so the page needs no request to answer a hover.
func figure(svg, marks string) template.HTML {
	return template.HTML(fmt.Sprintf(`<figure class="chartwrap" data-pts="%s">%s<div class="tip" hidden></div></figure>`,
		template.HTMLEscapeString(marks), svg))
}

// chart draws one line over a fixed time window. Server-rendered SVG: a chart
// library would be a dependency and a build step for what is one polyline.
func chart(pts []point, from, to time.Time, label func(float64) string, class string) template.HTML {
	const w, h = 760.0, 170.0
	const padL, padR, padT, padB = 62.0, 8.0, 10.0, 20.0
	plotW, plotH := w-padL-padR, h-padT-padB

	max := 0.0
	for _, p := range pts {
		if p.V > max {
			max = p.V
		}
	}
	if max <= 0 {
		max = 1
	}
	max *= 1.15 // headroom, so the peak is not glued to the frame

	span := float64(to.Unix() - from.Unix())
	x := func(t int64) float64 {
		if span <= 0 {
			return padL
		}
		return padL + plotW*float64(t-from.Unix())/span
	}
	y := func(v float64) float64 { return padT + plotH*(1-v/max) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart %s" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img">`, class, w, h)
	for i := 0; i <= 2; i++ {
		v := max * float64(i) / 2
		gy := y(v)
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, padL, gy, w-padR, gy)
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, padL-6, gy+4, template.HTMLEscapeString(label(v)))
	}
	// Times carry as unix seconds and are formatted by the browser: the module
	// runs in whatever timezone its container has, the reader does not.
	for i, t := range []time.Time{from, from.Add(to.Sub(from) / 2), to} {
		anchor := [...]string{"start", "middle", "end"}[i]
		fmt.Fprintf(&b, `<text class="tick when" data-ts="%d" x="%.1f" y="%.1f" text-anchor="%s">%s</text>`,
			t.Unix(), x(t.Unix()), h-6, anchor, t.UTC().Format("02.01 15:04 UTC"))
	}
	if len(pts) == 0 {
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.1f" text-anchor="middle">no samples in this range</text>`, w/2, h/2)
		b.WriteString(`</svg>`)
		return figure(b.String(), "[]")
	}
	var line, area strings.Builder
	for i, p := range pts {
		cmd := "L"
		if i == 0 {
			cmd = "M"
			fmt.Fprintf(&area, "M%.1f,%.1f", x(p.T), y(0))
		}
		fmt.Fprintf(&line, "%s%.1f,%.1f", cmd, x(p.T), y(p.V))
		fmt.Fprintf(&area, "L%.1f,%.1f", x(p.T), y(p.V))
	}
	fmt.Fprintf(&area, "L%.1f,%.1fZ", x(pts[len(pts)-1].T), y(0))
	fmt.Fprintf(&b, `<path class="area" d="%s"/><path class="line" d="%s"/>`, area.String(), line.String())
	fmt.Fprintf(&b, `<g class="cursor" style="display:none"><line y1="%.1f" y2="%.1f"/><circle r="3.5"/></g>`, padT, padT+plotH)
	b.WriteString(`</svg>`)

	// The hover script needs the points where they are drawn, the value already
	// formatted (no second implementation of human() in JS) and the raw time.
	marks := make([][4]any, len(pts))
	for i, p := range pts {
		marks[i] = [4]any{round(x(p.T)), round(y(p.V)), p.T, label(p.V)}
	}
	js, _ := json.Marshal(marks)
	return figure(b.String(), string(js))
}

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

const head = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Metrics</title>
<link rel="stylesheet" href="/static/panel.css">
<style>
body { margin: 1rem; }
.chart { width: 100%; height: 170px; display: block; }
.chart .grid { stroke: currentColor; opacity: .18; }
.chart .tick { fill: currentColor; opacity: .6; font-size: 11px; }
.chart .line { fill: none; stroke: var(--accent, #4a90d9); stroke-width: 2; vector-effect: non-scaling-stroke; }
.chart .area { fill: var(--accent, #4a90d9); opacity: .12; }
.chart .cursor line { stroke: currentColor; opacity: .45; stroke-width: 1; vector-effect: non-scaling-stroke; }
.chart .cursor circle { fill: var(--accent, #4a90d9); }
.chart.mem .cursor circle { fill: #6ab04c; }
.chartwrap { position: relative; margin: 0 0 1rem; }
.tip { position: absolute; z-index: 2; pointer-events: none; white-space: nowrap;
  padding: .25rem .45rem; border-radius: 4px; font-size: 12px; line-height: 1.3;
  background: #000c; color: #fff; transform: translate(10px, -50%); }
.tip.flip { transform: translate(calc(-100% - 10px), -50%); }
.tip small { opacity: .7; }
.chart.mem .line { stroke: #6ab04c; }
.chart.mem .area { fill: #6ab04c; }
.num { text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
.tabs a { margin-right: .5rem; }
.tabs a.on { font-weight: 600; text-decoration: underline; }
</style></head><body>`

var overview = template.Must(template.New("o").Funcs(template.FuncMap{"human": humanU, "pct": pct}).Parse(head + `
<h1>Metrics</h1>
{{if .Warn}}<p class="bad">{{.Warn}}</p>{{end}}
<table>
<tr><th>Service</th><th>Stack</th><th class=num>Replicas</th><th class=num>CPU</th><th class=num>Memory</th><th>Sampled</th></tr>
{{range .Rows}}
<tr>
  <td><a href="/modules/metrics/service?name={{.Service}}">{{.Service}}</a></td>
  <td class=muted>{{.Stack}}</td>
  <td class=num>{{.Replicas}}</td>
  <td class=num>{{pct .CPU}}</td>
  <td class=num>{{human .Mem}}{{if .MemLimit}} / {{if gt .Replicas 1}}{{.Replicas}} &times; {{end}}{{human .MemLimit}}{{end}}</td>
  <td class=muted>{{.Age}}</td>
</tr>
{{else}}
<tr><td colspan=6>Nothing sampled yet.</td></tr>
{{end}}
</table>
<script>setTimeout(() => location.reload(), 30000);</script>
</body></html>`))

var detail = template.Must(template.New("d").Parse(head + `
<h1>{{.Service}}</h1>
<p class=tabs>
<a href="/modules/metrics/">&larr; all services</a>
{{range .Ranges}}<a class="{{if eq .Label $.Range}}on{{end}}" href="/modules/metrics/service?name={{$.Service}}&amp;range={{.Label}}">{{.Label}}</a>{{end}}
</p>
<h2>CPU, % of one core</h2>
{{.CPUChart}}
<h2>Memory</h2>
{{.MemChart}}
<script>
// Both charts share one time axis, so a hover on either moves the cursor on
// both — reading CPU against memory at the same second is the whole point.
const wraps = [...document.querySelectorAll('.chartwrap')];
const data = wraps.map(w => JSON.parse(w.dataset.pts));
const VBW = 760, VBH = 170;

// The server has no idea which timezone the reader is in, so it sends seconds.
const when = ts => new Date(ts * 1000).toLocaleString([], {
  day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit',
});
for (const label of document.querySelectorAll('.when')) {
  label.textContent = when(+label.dataset.ts);
}

function hide() {
  for (const w of wraps) {
    w.querySelector('.cursor').style.display = 'none';
    w.querySelector('.tip').hidden = true;
  }
}

function show(i) {
  wraps.forEach((w, k) => {
    const p = data[k][i], cursor = w.querySelector('.cursor'), tip = w.querySelector('.tip');
    if (!p) { cursor.style.display = 'none'; tip.hidden = true; return; }
    const [px, py, ts, value] = p;
    cursor.style.display = '';
    const line = cursor.querySelector('line');
    line.setAttribute('x1', px);
    line.setAttribute('x2', px);
    const dot = cursor.querySelector('circle');
    dot.setAttribute('cx', px);
    dot.setAttribute('cy', py);

    const box = w.getBoundingClientRect();
    tip.hidden = false;
    tip.innerHTML = '<b></b><br><small></small>';
    tip.querySelector('b').textContent = value;
    tip.querySelector('small').textContent = when(ts);
    tip.style.left = (px / VBW * box.width) + 'px';
    tip.style.top = (py / VBH * box.height) + 'px';
    tip.classList.toggle('flip', px > VBW * 0.7);
  });
}

for (const [k, w] of wraps.entries()) {
  w.addEventListener('mousemove', e => {
    const pts = data[k];
    if (!pts.length) return;
    const box = w.getBoundingClientRect();
    const vx = (e.clientX - box.left) / box.width * VBW;
    let best = 0;
    for (let i = 1; i < pts.length; i++) {
      if (Math.abs(pts[i][0] - vx) < Math.abs(pts[best][0] - vx)) best = i;
    }
    show(best);
  });
  w.addEventListener('mouseleave', hide);
}
</script>
</body></html>`))

func overviewPage(w http.ResponseWriter, db *sql.DB, r *http.Request) {
	rows, err := latest(db, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	warn := ""
	if creds.Load() == nil {
		warn = "No token yet — sampling starts with the first request the panel proxies here."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	overview.Execute(w, struct {
		Rows []row
		Warn string
	}{rows, warn})
}

func servicePage(w http.ResponseWriter, db *sql.DB, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	label, d := rangeOf(r.URL.Query().Get("range"))
	to := time.Now()
	from := to.Add(-d)
	cpu, mem, err := series(db, name, from, to, 180)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	detail.Execute(w, struct {
		Service            string
		Range              string
		Ranges             any
		CPUChart, MemChart template.HTML
	}{name, label, ranges,
		chart(cpu, from, to, pct, "cpu"),
		chart(mem, from, to, human, "mem")})
}
