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

const queryTimeout = 5 * time.Second

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
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
	srv := &http.Server{Addr: addr, Handler: newMux(s, kick), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func newMux(s *store, kick chan<- struct{}) *http.ServeMux {
	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.status())
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

// run polls every interval, and right after the device list changes.
func run(s *store, interval time.Duration, kick <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		pollAll(s)
		select {
		case <-t.C:
		case <-kick:
		}
	}
}

// pollAll goes one device at a time: each connection is short, and these
// devices accept only a few at once.
func pollAll(s *store) {
	for _, d := range s.list() {
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
