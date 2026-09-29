package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	runtimepprof "runtime/pprof"
	"strconv"
	"sync"
	"time"

	"github.com/felixge/fgprof"
)

func profileHandler(maxSeconds int) http.Handler {
	return profileHandlerWithStarters(maxSeconds, func(w io.Writer) (func() error, error) {
		if err := runtimepprof.StartCPUProfile(w); err != nil {
			return nil, err
		}
		return func() error { runtimepprof.StopCPUProfile(); return nil }, nil
	}, func(w io.Writer) (func() error, error) {
		return fgprof.Start(w, fgprof.FormatPprof), nil
	})
}

type samplingStarter func(io.Writer) (stop func() error, err error)

func profileHandlerWithStarters(maxSeconds int, startCPU, startFG samplingStarter) http.Handler {
	mux := http.NewServeMux()
	var stateMu sync.Mutex
	active := map[string]string{"cpu": "", "fgprof": ""}
	mux.HandleFunc("/debug/profiles/status", func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(active)
	})
	// CPU and goroutine wall-clock sampling can run together. Reject duplicate
	// captures of the same kind and expose RUN ownership to the collector gate.
	bounded := func(kind string, start samplingStarter) http.Handler {
		var sampling sync.Mutex
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seconds := maxSeconds
			if value := r.URL.Query().Get("seconds"); value != "" {
				var err error
				seconds, err = strconv.Atoi(value)
				if err != nil || seconds < 1 || seconds > maxSeconds {
					http.Error(w, fmt.Sprintf("seconds must be between 1 and %d", maxSeconds), http.StatusBadRequest)
					return
				}
			}
			if !sampling.TryLock() {
				http.Error(w, "a sampling profile is already running", http.StatusConflict)
				return
			}
			defer sampling.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			stop, err := start(w)
			if err != nil {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				http.Error(w, "could not start "+kind+" profile: "+err.Error(), http.StatusInternalServerError)
				return
			}
			// Publish RUN ownership only after the profiler has started successfully.
			stateMu.Lock()
			active[kind] = r.URL.Query().Get("run_id")
			if active[kind] == "" {
				active[kind] = "manual"
			}
			stateMu.Unlock()
			defer func() {
				stateMu.Lock()
				active[kind] = ""
				stateMu.Unlock()
			}()
			defer func() {
				if err := stop(); err != nil {
					log.Printf("%s profile export failed: %v", kind, err)
				}
			}()
			timer := time.NewTimer(time.Duration(seconds) * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
			}
		})
	}
	mux.Handle("/debug/pprof/profile", bounded("cpu", startCPU))
	mux.Handle("/debug/fgprof", bounded("fgprof", startFG))
	for _, name := range []string{"heap", "allocs", "goroutine"} {
		mux.Handle("/debug/pprof/"+name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// These endpoints are snapshots; delta profiles need a separate policy.
			if r.URL.Query().Has("seconds") {
				http.Error(w, "snapshot profiles do not accept seconds", http.StatusBadRequest)
				return
			}
			pprof.Handler(r.URL.Path[len("/debug/pprof/"):]).ServeHTTP(w, r)
		}))
	}
	return mux
}

func startProfileServer(maxSeconds int) error {
	if maxSeconds < 1 || maxSeconds > 3600 {
		return fmt.Errorf("profile duration limit must be between 1 and 3600 seconds")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:6060")
	if err != nil {
		return fmt.Errorf("profile listener unavailable: %w", err)
	}
	server := &http.Server{
		Handler:           measurementHandler(maxSeconds),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      time.Duration(maxSeconds+15) * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("profile server stopped: %v", err)
		}
	}()
	return nil
}
