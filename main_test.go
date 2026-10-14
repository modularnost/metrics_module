package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
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

func TestManifestIsContractTwoWithTwoPagesOfBlocks(t *testing.T) {
	var m struct {
		Contract      int `json:"contract"`
		Contributions []struct {
			Slot, Path, Role, Render string
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
		if c.Render != "ui" {
			t.Errorf("%s: a page of blocks, not a frame: %q", c.Slot, c.Render)
		}
	}
	if m.Contract != 2 || slots["nav"] != "/" || slots["service.tab"] != "/service" || len(m.Permissions) != 1 {
		t.Fatalf("%+v", m)
	}
}

func seeded(t *testing.T) (*sql.DB, time.Time) {
	t.Helper()
	db, err := openDB(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now()
	for i := range 3 {
		s := []stat{{Node: "n1", Service: "api", Stack: "shop", Slot: 1, CPU: float64(i), Mem: 100, MemLimit: 1000, NetRX: uint64(100 * i)},
			{Node: "n2", Service: "api", Stack: "shop", Slot: 2, CPU: 1, Mem: 50, MemLimit: 1000}}
		if err := save(db, now.Add(-time.Duration(2-i)*time.Minute), s); err != nil {
			t.Fatal(err)
		}
	}
	return db, now
}

func TestOverviewIsATableOfServicesWithLinksIntoTheModule(t *testing.T) {
	db, now := seeded(t)
	rows, err := latest(db, now)
	if err != nil {
		t.Fatal(err)
	}
	d := overviewDoc(rows, "")
	var table block
	for _, b := range d.Blocks {
		if b["type"] == "table" {
			table = b
		}
	}
	r := table["rows"].([]block)
	if len(r) != 1 || r[0]["replicas"] != 2 || r[0]["memory"] != "150 B / 2 × 1000 B" {
		t.Fatalf("%+v", r)
	}
	if href := r[0]["service"].(block)["href"].(block); href["path"] != "/service?name=api" {
		t.Errorf("a link must open the service page: %+v", href)
	}
	if d.Blocks[0]["type"] != "stat" {
		t.Errorf("no banner while the token is known: %+v", d.Blocks[0])
	}
	if w := overviewDoc(nil, "No token yet"); w.Blocks[0]["type"] != "banner" {
		t.Errorf("a missing token is said first: %+v", w.Blocks[0])
	}
}

func TestServiceDocumentHasAChartPerNumberOnOneAxisAndAPageOfItsOwnWithAWayBack(t *testing.T) {
	db, now := seeded(t)
	standalone, err := serviceDoc(db, "api", false, "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	tab, err := serviceDoc(db, "api", true, "1h", now)
	if err != nil {
		t.Fatal(err)
	}

	titles := func(d document) (charts []string, types []any) {
		for _, b := range d.Blocks {
			types = append(types, b["type"])
			if b["type"] == "chart" {
				charts = append(charts, b["title"].(string))
				if b["sync"] != "time" {
					t.Errorf("charts share one time axis: %+v", b)
				}
			}
		}
		return
	}
	charts, tabTypes := titles(tab)
	if len(charts) != 6 || charts[0] != "CPU, % of one core" || charts[5] != "Block write" {
		t.Errorf("charts: %v", charts)
	}
	// A tab is inside the service's own page: no heading, no link out of it.
	if tabTypes[0] != "choice" {
		t.Errorf("a tab starts with the range: %v", tabTypes)
	}
	if _, types := titles(standalone); types[0] != "link" || types[1] != "heading" || types[2] != "choice" {
		t.Errorf("a page of its own has a way back and a heading: %v", types)
	}

	// Points are [unix seconds, value] pairs.
	cpu := tab.Blocks[1]["series"].([]block)[0]["points"].([][2]float64)
	if len(cpu) != 3 || cpu[0][1] != 1 || cpu[2][1] != 3 { // the replicas are summed: (0+1), (1+1), (2+1)
		t.Errorf("cpu points: %v", cpu)
	}
	if _, err := json.Marshal(tab); err != nil {
		t.Fatal(err)
	}
}

func TestServicePageAsksWhichServiceWhenNoneIsGiven(t *testing.T) {
	db, _ := seeded(t)
	page := func(query string) document {
		rec := httptest.NewRecorder()
		servicePage(rec, db, httptest.NewRequest("GET", "/service?"+query, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
		}
		var d document
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := page(""); d.Blocks[0]["type"] != "banner" {
		t.Errorf("no service: %+v", d.Blocks)
	}
	// The panel's tab says ?service=, our own links ?name=; the range is kept apart.
	if tab := page("service=api&id=abc&stack=shop&range=24h"); tab.Blocks[0]["value"] != "24h" {
		t.Errorf("tab: %+v", tab.Blocks[0])
	}
	if own := page("name=api"); own.Blocks[0]["type"] != "link" {
		t.Errorf("own page: %+v", own.Blocks[0])
	}
	if d := page("name=api&range=forever"); d.Blocks[2]["value"] != "6h" {
		t.Errorf("an unknown range is six hours: %+v", d.Blocks[2])
	}
}
