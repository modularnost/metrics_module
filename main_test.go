package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSaveAndSeries(t *testing.T) {
	db, err := openDB(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	stats := []stat{
		{Node: "n1", Service: "api", Stack: "shop", Slot: 1, CPU: 10, Mem: 100, MemLimit: 1000, NetRX: 100, BlockWrite: 30},
		{Node: "n2", Service: "api", Stack: "shop", Slot: 2, CPU: 5, Mem: 50, MemLimit: 1000, NetRX: 50, BlockWrite: 10},
		{Node: "n2", Service: "db", Stack: "shop", CPU: 1, Mem: 10},
		{Node: "n3", Err: "unreachable"},
	}
	if err := save(db, now, stats); err != nil {
		t.Fatal(err)
	}
	if err := save(db, now.Add(-2*time.Hour), stats); err != nil {
		t.Fatal(err)
	}
	// Older than the retention window: must not survive the next write.
	if err := save(db, now.Add(-retention-time.Hour), stats); err != nil {
		t.Fatal(err)
	}
	if err := save(db, now.Add(time.Second), stats); err != nil {
		t.Fatal(err)
	}

	rows, err := latest(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want api and db (the errored node dropped), got %+v", rows)
	}
	if rows[0].Service != "api" || rows[0].CPU != 15 || rows[0].Replicas != 2 || rows[0].Mem != 150 {
		t.Errorf("replicas not summed, hungriest not first: %+v", rows[0])
	}
	if rows[0].MemLimit != 1000 {
		t.Errorf("the limit is per replica, not a sum: %+v", rows[0])
	}
	var netRX uint64
	if err := db.QueryRow(`SELECT net_rx FROM samples WHERE service = 'api' AND ts = ?`, now.Unix()).Scan(&netRX); err != nil || netRX != 150 {
		t.Fatalf("network counters not summed: %d, %v", netRX, err)
	}

	cpu, mem, err := series(db, "api", now.Add(-3*time.Hour), now.Add(time.Minute), 180)
	if err != nil {
		t.Fatal(err)
	}
	if len(cpu) != 2 || len(mem) != 2 {
		t.Fatalf("want 2 buckets in range, retained row excluded: cpu=%v mem=%v", cpu, mem)
	}
	if cpu[1].V != 15 || mem[1].V != 150 {
		t.Errorf("bucket values wrong: %v %v", cpu[1], mem[1])
	}
	net, err := counterSeries(db, "api", "net_rx", now.Add(-3*time.Hour), now.Add(time.Minute), 180)
	if err != nil || len(net) != 1 || net[0].V != 0 {
		t.Errorf("counter series should handle equal samples: %v, %v", net, err)
	}
	if _, err := counterSeries(db, "api", "not-a-column", now, now, 1); err == nil {
		t.Error("unknown counter column accepted")
	}
}

func TestChart(t *testing.T) {
	to := time.Now()
	from := to.Add(-time.Hour)
	svg := string(chart([]point{{from.Unix(), 0}, {to.Unix(), 50}}, from, to, pct, "cpu"))
	if !strings.Contains(svg, `class="line"`) || !strings.Contains(svg, "57.5 %") {
		t.Errorf("no line or no axis label with headroom: %s", svg)
	}
	// The hover script snaps to these: one entry per point, x/y in viewBox
	// units, with the label already formatted.
	pts := svg[strings.Index(svg, `data-pts="`)+10:]
	pts = pts[:strings.Index(pts, `"`)]
	if !strings.Contains(pts, "50.0 %") || strings.Count(pts, "],[") != 1 {
		t.Errorf("hover data wrong: %s", pts)
	}
	// Times travel as unix seconds; the browser formats them in the reader's
	// timezone, which the module cannot know.
	if !strings.Contains(pts, fmt.Sprint(to.Unix())) {
		t.Errorf("hover data has no raw timestamp: %s", pts)
	}
	if !strings.Contains(svg, fmt.Sprintf(`data-ts="%d"`, from.Unix())) {
		t.Errorf("axis label has no raw timestamp: %s", svg)
	}
	if !strings.Contains(svg, `class="cursor"`) {
		t.Errorf("no crosshair: %s", svg)
	}
	if empty := string(chart(nil, from, to, pct, "cpu")); !strings.Contains(empty, "no samples") || !strings.Contains(empty, `data-pts="[]"`) {
		t.Errorf("empty chart should say so: %s", empty)
	}
}

func TestManifestIsContractOneWithAServiceTab(t *testing.T) {
	var m struct {
		Contract      int `json:"contract"`
		Contributions []struct {
			Slot, Path, Role string
		} `json:"contributions"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		t.Fatal(err)
	}
	slots := map[string]string{}
	for _, c := range m.Contributions {
		slots[c.Slot] = c.Path
		if c.Role != "viewer" {
			t.Errorf("%s: the numbers are what the panel shows a viewer, got role %q", c.Slot, c.Role)
		}
	}
	if m.Contract != 1 || slots["nav"] != "/" || slots["service.tab"] != "/service" || len(m.Permissions) != 1 {
		t.Fatalf("%+v", m)
	}
}

func TestServicePageAsATabHasNoWayOutOfTheService(t *testing.T) {
	db, err := openDB(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	page := func(query string) string {
		rec := httptest.NewRecorder()
		servicePage(rec, db, httptest.NewRequest("GET", "/service?"+query, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	// The panel's tab: told the service as ?service=, shown inside a service.
	tab := page("service=api&id=abc&stack=shop")
	if strings.Contains(tab, "all services") || strings.Contains(tab, "<h1>api</h1>") {
		t.Error("a tab must not repeat the service's heading or link out to the overview")
	}
	if !strings.Contains(tab, "service?service=api&amp;range=") {
		t.Error("the range links must keep telling the page which service it is")
	}

	// Our own overview link: a page of its own.
	own := page("name=api")
	if !strings.Contains(own, "all services") || !strings.Contains(own, "<h1>api</h1>") || !strings.Contains(own, "service?name=api&amp;range=") {
		t.Error("the standalone page lost its heading, its way back or its range links")
	}

	rec := httptest.NewRecorder()
	servicePage(rec, db, httptest.NewRequest("GET", "/service", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no service at all: %d", rec.Code)
	}
}
