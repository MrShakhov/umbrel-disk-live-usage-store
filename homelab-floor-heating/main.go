// Floor Heating: polls Tuya Wi-Fi thermostats over the LAN and keeps the
// history that the vendor's mobile app does not. Read-only towards the devices.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	queryTimeout = 5 * time.Second
	// How far past its schedule the collector can run before the page calls it stalled.
	stallMargin = 15 * time.Second
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "pin-neighbors" {
		pinNeighbors(env("DATA_DIR", "/data"))
		return
	}
	interval, err := time.ParseDuration(env("POLL_INTERVAL", "60s"))
	if err != nil || interval < 30*time.Second {
		log.Fatalf("bad POLL_INTERVAL %q (want e.g. 60s, minimum 30s)", os.Getenv("POLL_INTERVAL"))
	}
	s, err := openStore(env("DATA_DIR", "/data"))
	if err != nil {
		log.Fatalf("open data folder: %v", err)
	}
	kick := make(chan struct{}, 1)
	go run(s, interval, kick)

	addr := env("LISTEN", ":8080")
	log.Printf("floor-heating listening on %s, polling every %s", addr, interval)
	srv := &http.Server{Addr: addr, Handler: newMux(s, kick, interval), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func newMux(s *store, kick chan<- struct{}, interval time.Duration) *http.ServeMux {
	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"poll": s.pollStatus(time.Now()), "devices": s.status()})
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		rng, ok := ranges[r.URL.Query().Get("range")]
		if !ok {
			http.Error(w, "want range=24h, 7d or 30d", http.StatusBadRequest)
			return
		}
		to := time.Now().UTC()
		lines, err := s.series(to.Add(-rng.span), to, rng.step, interval)
		if err != nil {
			http.Error(w, "cannot read history", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"from": to.Add(-rng.span).UnixMilli(), "to": to.UnixMilli(),
			"step": rng.step.Milliseconds(), "series": lines,
		})
	})
	mux.HandleFunc("PUT /api/devices", func(w http.ResponseWriter, r *http.Request) {
		// JSON only: a cross-site form cannot send this content type without a preflight.
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
			return
		}
		var in []device
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		if err := s.save(in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case kick <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/history/{day}", func(w http.ResponseWriter, r *http.Request) {
		day := r.PathValue("day")
		if _, err := time.Parse(time.DateOnly, day); err != nil {
			http.Error(w, "want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		f, err := os.Open(s.dayPath(day))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "cannot read history", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/x-ndjson")
		http.ServeContent(w, r, "", time.Time{}, f)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// run polls every interval, and right after the device list changes. That extra
// round leaves the schedule alone. The loop waits for the time it publishes, so
// the page's countdown can't drift from what happens here.
func run(s *store, interval time.Duration, kick <-chan struct{}) {
	next := time.Now()
	for {
		pollAll(s)
		now := time.Now()
		next = nextRound(next, now, interval)
		s.roundFinished(now, next)
		t := time.NewTimer(next.Sub(now))
		select {
		case <-t.C:
		case <-kick:
			t.Stop()
		}
	}
}

// nextRound is the first time on the schedule after now. A round that runs past
// a due time skips it, so slow devices don't get polled back to back.
func nextRound(due, now time.Time, interval time.Duration) time.Time {
	for !due.After(now) {
		due = due.Add(interval)
	}
	return due
}

// pollAll goes one device at a time: each connection is short, and these
// devices accept only a few at once.
func pollAll(s *store) {
	devices := s.list()
	s.roundStarted(time.Now(), len(devices))
	for _, d := range devices {
		start := time.Now()
		r := reading{T: start, ID: d.ID}
		dps, err := query(d.IP, d.ID, d.Key, queryTimeout)
		if err != nil {
			r.Err = err.Error()
		} else {
			r.DPS, r.MS = dps, time.Since(start).Milliseconds()
		}
		if err := s.record(r); err != nil {
			log.Printf("write history: %v", err)
		}
	}
}
