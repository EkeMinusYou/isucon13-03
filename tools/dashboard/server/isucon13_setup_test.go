package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
