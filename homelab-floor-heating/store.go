package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// device is one thermostat. Key is the Tuya local key: it stays in the data
// folder and never leaves the app through the API.
type device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`
	MAC  string `json:"mac,omitempty"`
	Key  string `json:"key"`
}

// reading is one poll result: a history line, and the latest state in the UI.
type reading struct {
	T   time.Time      `json:"t"`
	ID  string         `json:"id"`
	MS  int64          `json:"ms,omitempty"`
	DPS map[string]any `json:"dps,omitempty"`
	Err string         `json:"err,omitempty"`
}

type store struct {
	dir string

	mu      sync.Mutex
	devices []device
	last    map[string]reading

	polling  bool
	lastPoll time.Time // when the last round finished
	nextPoll time.Time // when the next round is due
	stallAt  time.Time // the collector is stuck if it hasn't moved on by then
}

func openStore(dir string) (*store, error) {
	s := &store{dir: dir, last: map[string]reading{}}
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.devicesPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.devices)
}

func (s *store) devicesPath() string { return filepath.Join(s.dir, "devices.json") }

func (s *store) list() []device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]device(nil), s.devices...)
}

// save replaces the device list. An entry without a key keeps the stored key,
// so the UI can edit a device without ever seeing its key.
func (s *store) save(in []device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := map[string]string{}
	for _, d := range s.devices {
		keys[d.ID] = d.Key
	}
	seen := map[string]bool{}
	for i := range in {
		d := &in[i]
		if d.Key == "" {
			d.Key = keys[d.ID]
		}
		if d.ID == "" || seen[d.ID] {
			return fmt.Errorf("missing or duplicate device id %q", d.ID)
		}
		seen[d.ID] = true
		if net.ParseIP(d.IP) == nil {
			return fmt.Errorf("device %s: bad IP %q", d.ID, d.IP)
		}
		if len(d.Key) != 16 {
			return fmt.Errorf("device %s: the local key must be 16 characters", d.ID)
		}
		if d.MAC != "" {
			mac, err := net.ParseMAC(d.MAC)
			if err != nil || len(mac) != 6 {
				return fmt.Errorf("device %s: bad MAC address %q", d.ID, d.MAC)
			}
			d.MAC = mac.String()
		}
	}
	if err := writeJSON(s.devicesPath(), in, 0o600); err != nil {
		return err
	}
	pins := []neighbor{}
	for _, d := range in {
		if d.MAC != "" {
			pins = append(pins, neighbor{IP: d.IP, MAC: d.MAC})
		}
	}
	// World-readable on purpose: the pinning container can't read anything else here.
	if err := writeJSON(filepath.Join(s.dir, neighborsFile), pins, 0o644); err != nil {
		return err
	}
	s.devices = in
	return nil
}

func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// record keeps a reading as the latest and appends it to the day's JSONL file.
// Failed polls are written too, so gaps in the history can be told from outages.
func (s *store) record(r reading) error {
	s.mu.Lock()
	s.last[r.ID] = r
	s.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.dayPath(r.T.UTC().Format(time.DateOnly)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *store) dayPath(day string) string {
	return filepath.Join(s.dir, "history", day+".jsonl")
}

type deviceStatus struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	IP     string   `json:"ip"`
	MAC    string   `json:"mac,omitempty"`
	Latest *reading `json:"latest,omitempty"`
	State  *state   `json:"state,omitempty"`
}

func (s *store) status() []deviceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]deviceStatus, 0, len(s.devices))
	for _, d := range s.devices {
		st := deviceStatus{ID: d.ID, Name: d.Name, IP: d.IP, MAC: d.MAC}
		if r, ok := s.last[d.ID]; ok {
			st.Latest = &r
			if r.Err == "" {
				decoded := decode(r.DPS)
				st.State = &decoded
			}
		}
		out = append(out, st)
	}
	return out
}

func (s *store) roundStarted(now time.Time, devices int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polling = true
	// A device can use the timeout twice: to connect, and then to answer.
	s.stallAt = now.Add(time.Duration(devices)*2*queryTimeout + stallMargin)
}

func (s *store) roundFinished(now, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polling = false
	s.lastPoll, s.nextPoll = now, next
	s.stallAt = next.Add(stallMargin)
}

// pollStatus says what the collector is doing. It holds durations, not times, so
// the page doesn't depend on the browser's clock or on a Pi setting its own late.
type pollStatus struct {
	State     string `json:"state"`       // polling, waiting or stalled
	NextInMS  int64  `json:"next_in_ms"`  // negative once overdue, 0 before the first round ends
	LastAgoMS int64  `json:"last_ago_ms"` // since the last round finished, 0 before the first
}

func (s *store) pollStatus(now time.Time) pollStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := pollStatus{State: "waiting"}
	if !s.nextPoll.IsZero() {
		st.NextInMS = s.nextPoll.Sub(now).Milliseconds()
		st.LastAgoMS = now.Sub(s.lastPoll).Milliseconds()
	}
	switch {
	case s.stallAt.IsZero(): // the first round is about to start
		st.State = "polling"
	case now.After(s.stallAt):
		st.State = "stalled"
	case s.polling:
		st.State = "polling"
	}
	return st
}
