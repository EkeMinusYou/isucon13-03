package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestISUCON13ScoreSemantics(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb is not installed")
	}
	schema, err := os.ReadFile("../contest/analysis-schema/bench-errors-semantic.sql")
	if err != nil {
		t.Fatal(err)
	}
	fixture := `CREATE TABLE bench_error_logs(run_id varchar, content varchar, source_artifact varchar);
INSERT INTO bench_error_logs VALUES
('success', 'SCORE: 12345' || chr(10) || 'BENCHMARK_PASS', 'bench.log'),
('failure', 'BENCHMARK_PASS' || chr(10) || 'BENCHMARK_FAIL', 'bench.log'),
('unknown', '', 'bench.log'),
('official', '2026-09-29T21:26:33.717Z' || chr(9) || 'info' || chr(9) || '最終チェックが成功しました' || chr(10) || 'スコア: 3653', 'bench.log');`
	cmd := exec.Command("duckdb", "-json", "-c", fixture+string(schema)+"SELECT * FROM bench_results ORDER BY run_id")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	var rows []struct {
		RunID     string `json:"run_id"`
		Score     *int64 `json:"score"`
		Passed    *bool  `json:"passed"`
		Addition  *int64 `json:"addition"`
		Deduction *int64 `json:"deduction"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("results: %s", output)
	}
	for _, row := range rows {
		if row.Addition != nil || row.Deduction != nil {
			t.Fatal("unknown breakdown became zero")
		}
		switch row.RunID {
		case "success":
			if row.Score == nil || *row.Score != 12345 || row.Passed == nil || !*row.Passed {
				t.Fatal(string(output))
			}
		case "official":
			if row.Score == nil || *row.Score != 3653 || row.Passed == nil || !*row.Passed {
				t.Fatal(string(output))
			}
		case "failure":
			if row.Score != nil || row.Passed == nil || *row.Passed {
				t.Fatal(string(output))
			}
		case "unknown":
			if row.Score != nil || row.Passed != nil {
				t.Fatal(string(output))
			}
		}
	}
}

func TestISUCON13SetupSmokeImport(t *testing.T) {
	dir := os.Getenv("ISUCON_SETUP_SMOKE")
	if dir == "" {
		t.Skip("set ISUCON_SETUP_SMOKE to inspect actual setup artifacts")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	cfg, err := loadConfig("tools/analysis/sources.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := runner{options: options{db: filepath.Join(t.TempDir(), "smoke.duckdb"), duckdb: "duckdb", stdout: io.Discard, stderr: os.Stderr}, config: cfg}
	// The importer validates RUN-shaped paths. Alias the actual smoke data
	// inside the test directory only; never create a manifest or a real RUN.
	fixtureID := strings.TrimPrefix(filepath.Base(dir), "setup-smoke-")
	fixtureParent := filepath.Join(t.TempDir(), "runs")
	if err := os.Mkdir(fixtureParent, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureDir := filepath.Join(fixtureParent, fixtureID)
	if err := os.Symlink(dir, fixtureDir); err != nil {
		t.Fatal(err)
	}
	// Legacy MySQL readers resolve the key through scores.tsv. Supply an
	// empty-score fixture in the temporary directory, never the live history.
	fixtureScores := filepath.Join(fixtureParent, "scores.tsv")
	header := "run_id\tscore\tapp\tnginx\tmysql\tapp_traffic\n"
	row := fixtureID + "\t\tisucon-1,isucon-2,isucon-3\tisucon-1,isucon-2,isucon-3\tisucon-1\tisucon-1,isucon-2,isucon-3\n"
	if err := os.WriteFile(fixtureScores, []byte(header+row), 0600); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(cfg.path(cfg.BaseSchema))
	if err != nil {
		t.Fatal(err)
	}
	fixtureSchema := filepath.Join(fixtureParent, "base.sql")
	base = []byte(strings.ReplaceAll(string(base), "'runs/scores.tsv'", sqlString(fixtureScores)))
	if err := os.WriteFile(fixtureSchema, base, 0600); err != nil {
		t.Fatal(err)
	}
	r.config.BaseSchema = fixtureSchema
	if err := r.syncSelection(fixtureDir, []string{fixtureDir}, []string{fixtureID}); err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{
		"select count(distinct host) from metrics where source='proc'":           3,
		"select count(distinct host) from metrics where source='mysql'":          3,
		"select count(distinct entity) from metrics where source='service'":      4,
		"select count(distinct host) from metrics where source='sql_pool'":       3,
		"select count(distinct host || ':' || profile_type) from pprof_metadata": 15,
		"select count(distinct ingress_host) from upstreams_by_ingress":          3,
		"select count(distinct ingress_host) from endpoints_by_ingress":          3,
		"select count(distinct host) from queries where source='slp'":            3,
		"select count(distinct host) from runtime_logs":                          3,
		"select sum(transitions) from user_transition_edges":                     18,
	}
	for query, expected := range checks {
		actual, err := r.queryCount(query)
		if err != nil || actual != expected {
			t.Fatalf("%s: got %d, want %d: %v", query, actual, expected, err)
		}
		t.Logf("%s = %d", query, actual)
	}
}
