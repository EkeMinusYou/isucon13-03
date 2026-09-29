package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestISUCON13TimelineCountsConcurrentWarnings(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb is not installed")
	}
	dir := t.TempDir()
	runID := "20260930-000000"
	rawDir := filepath.Join(dir, runID, "raw")
	if err := os.MkdirAll(rawDir, 0700); err != nil {
		t.Fatal(err)
	}
	access := `{"msec":"1790720000.1","status":200,"response_time":0.01,"body_bytes":1}`
	if err := os.WriteFile(filepath.Join(rawDir, "access-host.log"), []byte(access+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("../../contest/analysis-schema/bench-errors-semantic.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Distinct requests, including identical messages, must each count once.
	log := "2026-09-29T22:13:20.125Z\twarn\tstaff-logger\trequest canceled\n" +
		"2026-09-29T22:13:20.125Z\twarn\tstaff-logger\trequest canceled\n" +
		"2026-09-29T22:13:20.125Z\terror\tstaff-logger\tHTTP 500\n"
	sql := "CREATE TABLE bench_error_logs(run_id varchar,content varchar,source_artifact varchar);" +
		"INSERT INTO bench_error_logs VALUES ('" + runID + "','" + strings.ReplaceAll(log, "'", "''") + "','bench.log');" + string(schema)
	app := &app{runsDir: dir, analysisDB: filepath.Join(t.TempDir(), "analysis.duckdb")}
	if out, err := exec.Command("duckdb", app.analysisDB, "-c", sql).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	r := httptest.NewRequest("GET", "/api/runs/"+runID+"/timeline", nil)
	r.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	app.handleTimeline(w, r)
	var got timelineResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(got.BenchErrors) != 3 || len(got.Buckets) != 1 || got.Buckets[0].ScenarioWarnings != 3 {
		t.Fatalf("concurrent warning counts: %+v", got)
	}
}

func TestISUCON13SetupSmokeReaders(t *testing.T) {
	dir := os.Getenv("ISUCON_SETUP_SMOKE")
	if dir == "" {
		t.Skip("set ISUCON_SETUP_SMOKE to inspect actual setup artifacts")
	}
	for _, host := range []string{"isucon-1", "isucon-2", "isucon-3"} {
		hosts, err := parseHostSeries(filepath.Join(dir, host+"-proc-metrics.tsv"), nil)
		if err != nil || len(hosts) < 2 {
			t.Fatalf("%s host points: %d: %v", host, len(hosts), err)
		}
		services, err := parseServiceSeries(filepath.Join(dir, host+"-service-metrics.tsv"), nil)
		if err != nil || len(services) != 4 {
			t.Fatalf("%s services: %d: %v", host, len(services), err)
		}
		mysql, err := parseMysqlSeries(filepath.Join(dir, host+"-mysql-status.tsv"), nil)
		if err != nil || len(mysql) < 2 {
			t.Fatalf("%s MySQL points: %d: %v", host, len(mysql), err)
		}
		for _, kind := range goPprofKinds {
			if _, err := parseGoPprofFile(filepath.Join(dir, host+kind.suffix), kind); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := parsePprofFile(filepath.Join(dir, host+"-fgprof.pprof")); err != nil {
			t.Fatal(err)
		}
	}
	alp, err := parseAlpJSON(filepath.Join(dir, "alp.json"))
	if err != nil || len(alp) == 0 {
		t.Fatalf("alp: %v", err)
	}
	upstreams, err := parseUpstreamRows(filepath.Join(dir, "upstream-breakdown.tsv"))
	if err != nil || len(upstreams) != 1 || upstreams[0].Requests != 21 {
		t.Fatalf("upstreams: %d: %v", len(upstreams), err)
	}
	transitions, available, err := parseUserTransitions(filepath.Join(dir, "user-transitions.json"))
	if err != nil || !available || transitions.Summary.Transitions != 18 || transitions.Summary.Sessions != 3 {
		t.Fatalf("transitions: %v", err)
	}
	t.Log("all three hosts, fifteen profiles, alp, upstreams, and session transitions parsed")
}
