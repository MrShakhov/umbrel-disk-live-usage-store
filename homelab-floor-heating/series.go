package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// Data points of the SS210RH_WH_WF thermostat. dpWork isn't in the vendor's
// spec: it reports "heating" while the relay is on, "warming" while idle.
const (
	dpPower     = "1"
	dpWork      = "3"
	dpSet       = "16"
	dpTemp      = "24"
	workHeating = "heating"
	tempScale   = 10.0
)

// ranges maps a UI range to its span and chart resolution.
var ranges = map[string]struct{ span, step time.Duration }{
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour},
}

// state is what a reading means on this thermostat.
type state struct {
	Temp    *float64 `json:"temp,omitempty"`
	Set     *float64 `json:"set,omitempty"`
	Off     bool     `json:"off"`
	Heating bool     `json:"heating"`
}

func decode(dps map[string]any) state {
	s := state{Temp: degrees(dps[dpTemp]), Set: degrees(dps[dpSet]), Off: dps[dpPower] == false}
	s.Heating = !s.Off && dps[dpWork] == workHeating
	return s
}

func degrees(v any) *float64 {
	n, ok := v.(float64)
	if !ok {
		return nil
	}
	c := n / tempScale
	return &c
}

// point is one chart bucket: mean temperatures and the share of polls that
// found the heating on, from 0 to 1. T is the bucket start in Unix milliseconds.
type point struct {
	T    int64    `json:"t"`
	Temp float64  `json:"temp"`
	Set  *float64 `json:"set,omitempty"`
	Heat float64  `json:"heat"`
}

type series struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	HeatHours float64 `json:"heat_hours"`
	Points    []point `json:"points"`
}

type bucket struct {
	temp, set    float64
	n, nSet, hot int
}

// series reads the day files covering from..to and folds them into buckets.
// Failed polls hold no data, so an outage shows up as a gap in the line.
func (s *store) series(from, to time.Time, step, interval time.Duration) ([]series, error) {
	devices := s.list()
	buckets := make(map[string][]bucket, len(devices))
	count := int(to.Sub(from)/step) + 1
	for _, d := range devices {
		buckets[d.ID] = make([]bucket, count)
	}
	for day := from.UTC().Truncate(24 * time.Hour); !day.After(to); day = day.Add(24 * time.Hour) {
		err := s.eachReading(day.Format(time.DateOnly), func(r reading) {
			b, known := buckets[r.ID]
			if !known || r.Err != "" || r.T.Before(from) || r.T.After(to) {
				return
			}
			st := decode(r.DPS)
			if st.Temp == nil {
				return
			}
			c := &b[int(r.T.Sub(from)/step)]
			c.temp += *st.Temp
			c.n++
			if st.Set != nil {
				c.set += *st.Set
				c.nSet++
			}
			if st.Heating {
				c.hot++
			}
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]series, 0, len(devices))
	for _, d := range devices {
		line := series{ID: d.ID, Name: d.Name, Points: []point{}}
		hot := 0
		for i, c := range buckets[d.ID] {
			if c.n == 0 {
				continue
			}
			p := point{
				T:    from.Add(time.Duration(i) * step).UnixMilli(),
				Temp: c.temp / float64(c.n),
				Heat: float64(c.hot) / float64(c.n),
			}
			if c.nSet > 0 {
				v := c.set / float64(c.nSet)
				p.Set = &v
			}
			line.Points = append(line.Points, p)
			hot += c.hot
		}
		line.HeatHours = float64(hot) * interval.Hours()
		out = append(out, line)
	}
	return out, nil
}

// eachReading streams one day file. A broken line, such as one cut off by a
// power loss, is skipped so it can't hide the rest of the day.
func (s *store) eachReading(day string, fn func(reading)) error {
	f, err := os.Open(s.dayPath(day))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		var r reading
		if json.Unmarshal(lines.Bytes(), &r) == nil {
			fn(r)
		}
	}
	return lines.Err()
}
