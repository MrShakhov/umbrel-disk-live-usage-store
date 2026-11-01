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
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.devicesPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.devicesPath()); err != nil {
		return err
	}
	s.devices = in
	return nil
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
	Latest *reading `json:"latest,omitempty"`
}

func (s *store) status() []deviceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]deviceStatus, 0, len(s.devices))
	for _, d := range s.devices {
		st := deviceStatus{ID: d.ID, Name: d.Name, IP: d.IP}
		if r, ok := s.last[d.ID]; ok {
			st.Latest = &r
		}
		out = append(out, st)
	}
	return out
}
