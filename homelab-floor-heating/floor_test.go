package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	testID  = "bf0000000000000000test"
	testKey = "0123456789abcdef"
)

// fakeDevice answers one DP_QUERY the way a thermostat does and checks that the
// request is a well-formed query for this device.
func fakeDevice(t *testing.T, ln net.Listener, key string, replies ...[]byte) {
	t.Helper()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		f, err := readFrame(conn)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if f.cmd != cmdDPQuery {
			t.Errorf("cmd = %#x, want DP_QUERY", f.cmd)
		}
		plain, err := decrypt(f.payload, key)
		if err != nil {
			t.Errorf("decrypt request: %v", err)
			return
		}
		var req map[string]string
		if err := json.Unmarshal(plain, &req); err != nil || req["devId"] != testID {
			t.Errorf("bad request %q", plain)
		}
		for _, r := range replies {
			conn.Write(r)
		}
	}()
}

func replyFrame(t *testing.T, key, body string, prefix ...byte) []byte {
	t.Helper()
	enc, err := encrypt([]byte(body), key)
	if err != nil {
		t.Fatal(err)
	}
	payload := append(append([]byte{0, 0, 0, 0}, prefix...), enc...)
	return encodeFrame(frame{seq: 1, cmd: cmdDPQuery, payload: payload})
}

func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ln, port
}

func exchange(t *testing.T, key string, replies ...[]byte) (map[string]any, error) {
	t.Helper()
	ln, port := listen(t)
	fakeDevice(t, ln, testKey, replies...)
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	return roundTrip(conn, testID, key)
}

func TestQueryReturnsDataPoints(t *testing.T) {
	dps, err := exchange(t, testKey, replyFrame(t, testKey, `{"devId":"x","dps":{"1":true,"24":215}}`))
	if err != nil {
		t.Fatal(err)
	}
	if dps["1"] != true || dps["24"] != float64(215) {
		t.Fatalf("dps = %v", dps)
	}
}

func TestQuerySkipsAckAndPushes(t *testing.T) {
	ack := encodeFrame(frame{seq: 1, cmd: cmdDPQuery, payload: []byte{0, 0, 0, 0}})
	push := encodeFrame(frame{seq: 0, cmd: 0x08, payload: []byte("3.3junk")})
	data := replyFrame(t, testKey, `{"dps":{"16":22}}`)
	dps, err := exchange(t, testKey, ack, push, data)
	if err != nil {
		t.Fatal(err)
	}
	if dps["16"] != float64(22) {
		t.Fatalf("dps = %v", dps)
	}
}

func TestQueryStripsVersionHeader(t *testing.T) {
	header := append([]byte("3.3"), make([]byte, 12)...)
	dps, err := exchange(t, testKey, replyFrame(t, testKey, `{"dps":{"1":false}}`, header...))
	if err != nil {
		t.Fatal(err)
	}
	if dps["1"] != false {
		t.Fatalf("dps = %v", dps)
	}
}

func TestWrongKeyIsReported(t *testing.T) {
	_, err := decodeReply(replyFrame(t, "fedcba9876543210", `{"dps":{"1":true}}`)[16:], testKey)
	if err == nil {
		t.Fatal("want an error for a reply under another key")
	}
}

func TestDeviceErrorTextIsReported(t *testing.T) {
	_, err := exchange(t, testKey, replyFrame(t, testKey, "json obj data unvalid"))
	if err == nil || !strings.Contains(err.Error(), "unvalid") {
		t.Fatalf("err = %v", err)
	}
}

func TestCorruptFrameIsRejected(t *testing.T) {
	b := replyFrame(t, testKey, `{"dps":{"1":true}}`)
	b[20] ^= 0xFF
	if _, err := readFrame(strings.NewReader(string(b))); err == nil {
		t.Fatal("want a CRC error")
	}
}

func put(mux http.Handler, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/devices", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDevicesAPINeverReturnsKeys(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(s, make(chan struct{}, 1), time.Minute)

	body := `[{"id":"` + testID + `","name":"Bath","ip":"192.0.2.10","key":"` + testKey + `"}]`
	if rec := put(mux, body, "text/plain"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", rec.Code)
	}
	if rec := put(mux, body, "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	// Renaming without a key keeps the stored one.
	if rec := put(mux, `[{"id":"`+testID+`","name":"Hall","ip":"192.0.2.10"}]`, "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := put(mux, `[{"id":"new","ip":"192.0.2.10"}]`, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("new device without key: %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if strings.Contains(rec.Body.String(), testKey) || !strings.Contains(rec.Body.String(), "Hall") {
		t.Fatalf("status = %s", rec.Body)
	}

	reopened, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.list(); len(got) != 1 || got[0].Key != testKey {
		t.Fatalf("reopened = %+v", got)
	}
	info, err := os.Stat(s.devicesPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("devices.json mode = %v", info.Mode().Perm())
	}
}

func TestRecordKeepsFailedPollsInHistory(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := s.record(reading{T: now, ID: testID, DPS: map[string]any{"1": true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.record(reading{T: now.Add(time.Minute), ID: testID, Err: "timeout"}); err != nil {
		t.Fatal(err)
	}
	mux := newMux(s, make(chan struct{}, 1), time.Minute)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/history/2026-11-01", nil))
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if rec.Code != http.StatusOK || len(lines) != 2 || !strings.Contains(lines[1], "timeout") {
		t.Fatalf("history %d: %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/history/..%2Fdevices", nil))
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("path traversal: %d", rec.Code)
	}
}

func TestPollStatusFollowsTheCollector(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if st := s.pollStatus(now); st.State != "polling" {
		t.Fatalf("before the first round: %+v", st)
	}
	s.roundStarted(now, 3)
	if st := s.pollStatus(now.Add(time.Second)); st.State != "polling" || st.NextInMS != 0 || st.LastAgoMS != 0 {
		t.Fatalf("first round: %+v", st)
	}
	// Three devices that all time out twice are slow, not stuck.
	worst := 6*queryTimeout + stallMargin
	if st := s.pollStatus(now.Add(worst)); st.State != "polling" {
		t.Fatalf("slow round: %+v", st)
	}
	if st := s.pollStatus(now.Add(worst + time.Second)); st.State != "stalled" {
		t.Fatalf("stuck round: %+v", st)
	}

	done, next := now.Add(2*time.Second), now.Add(time.Minute)
	s.roundFinished(done, next)
	if st := s.pollStatus(done.Add(18 * time.Second)); st != (pollStatus{"waiting", 40_000, 18_000}) {
		t.Fatalf("waiting: %+v", st)
	}
	if st := s.pollStatus(next.Add(stallMargin)); st.State != "waiting" || st.NextInMS != -stallMargin.Milliseconds() {
		t.Fatalf("late, within the margin: %+v", st)
	}
	if st := s.pollStatus(next.Add(stallMargin + time.Second)); st.State != "stalled" || st.NextInMS != -16_000 {
		t.Fatalf("missed the schedule: %+v", st)
	}
}

func TestStatusAPIReportsPollAndDevices(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save([]device{{ID: testID, Name: "Bath", IP: "192.0.2.10", Key: testKey}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.roundFinished(now, now.Add(time.Minute))
	rec := httptest.NewRecorder()
	newMux(s, make(chan struct{}, 1), time.Minute).ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	var got struct {
		Poll    pollStatus     `json:"poll"`
		Devices []deviceStatus `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("status %s: %v", rec.Body, err)
	}
	if got.Poll.State != "waiting" || got.Poll.NextInMS <= 0 || got.Poll.NextInMS > 60_000 {
		t.Fatalf("poll = %+v", got.Poll)
	}
	if len(got.Devices) != 1 || got.Devices[0].Name != "Bath" {
		t.Fatalf("devices = %+v", got.Devices)
	}
	if strings.Contains(rec.Body.String(), testKey) {
		t.Fatalf("key in %s", rec.Body)
	}
}

func TestNextRoundKeepsTheSchedule(t *testing.T) {
	due := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		after time.Duration
		want  time.Duration
	}{
		{-20 * time.Second, 0},               // a kicked round ending before the due time
		{time.Second, time.Minute},           // a normal round
		{time.Minute, 2 * time.Minute},       // ending right on the next due time
		{150 * time.Second, 3 * time.Minute}, // a round that ran past a due time
	} {
		if got := nextRound(due, due.Add(c.after), time.Minute); !got.Equal(due.Add(c.want)) {
			t.Errorf("%v after the due time: next %v", c.after, got.Sub(due))
		}
	}
}

// roundAfter waits until the collector has finished a round later than last.
func roundAfter(t *testing.T, s *store, last time.Time) (finished, next time.Time) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		s.mu.Lock()
		finished, next = s.lastPoll, s.nextPoll
		s.mu.Unlock()
		if finished.After(last) {
			return finished, next
		}
	}
	t.Fatal("no round finished")
	return
}

func TestKickPollsWithoutMovingTheSchedule(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kick := make(chan struct{}, 1)
	go run(s, time.Hour, kick)
	first, due := roundAfter(t, s, time.Time{})
	if got := due.Sub(first); got <= 59*time.Minute || got > time.Hour {
		t.Fatalf("next poll in %v", got)
	}
	kick <- struct{}{}
	second, after := roundAfter(t, s, first)
	if !after.Equal(due) {
		t.Fatalf("kick moved the next poll by %v", after.Sub(due))
	}
	if st := s.pollStatus(second); st.State != "waiting" || st.NextInMS < 59*60_000 {
		t.Fatalf("after a kick: %+v", st)
	}
}

func TestRunPublishesEachDueTime(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const interval = 20 * time.Millisecond
	go run(s, interval, make(chan struct{}))
	finished, due := roundAfter(t, s, time.Time{})
	_, next := roundAfter(t, s, finished)
	if gap := next.Sub(due); gap <= 0 || gap%interval != 0 {
		t.Fatalf("schedule moved by %v", gap)
	}
}

func TestDecodeReadsThermostatState(t *testing.T) {
	st := decode(map[string]any{"1": true, "3": "heating", "16": float64(235), "24": float64(222)})
	if !st.Heating || st.Off || *st.Temp != 22.2 || *st.Set != 23.5 {
		t.Fatalf("heating: %+v", st)
	}
	if st := decode(map[string]any{"1": true, "3": "warming"}); st.Heating {
		t.Fatal("warming must not count as heating")
	}
	if st := decode(map[string]any{"1": false, "3": "heating"}); st.Heating || !st.Off {
		t.Fatalf("off: %+v", st)
	}
}

func TestSeriesFoldsPollsAcrossDays(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save([]device{{ID: testID, Name: "Bath", IP: "192.0.2.10", Key: testKey}}); err != nil {
		t.Fatal(err)
	}
	poll := func(at time.Time, temp float64, work string) {
		t.Helper()
		r := reading{T: at, ID: testID, DPS: map[string]any{"1": true, "3": work, "16": 240.0, "24": temp}}
		if err := s.record(r); err != nil {
			t.Fatal(err)
		}
	}
	from := time.Date(2026, 11, 1, 23, 50, 0, 0, time.UTC)
	poll(from.Add(-time.Minute), 100, "heating") // before the range
	poll(from.Add(time.Minute), 200, "heating")
	poll(from.Add(2*time.Minute), 210, "warming")
	if err := s.record(reading{T: from.Add(12 * time.Minute), ID: testID, Err: "timeout"}); err != nil {
		t.Fatal(err)
	}
	poll(from.Add(21*time.Minute), 220, "heating") // next UTC day, after a gap
	to := from.Add(30 * time.Minute)

	lines, err := s.series(from, to, 5*time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || len(lines[0].Points) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
	first, second := lines[0].Points[0], lines[0].Points[1]
	if first.Temp != 20.5 || first.Heat != 0.5 || *first.Set != 24 || first.T != from.UnixMilli() {
		t.Fatalf("first = %+v", first)
	}
	if second.Temp != 22 || second.Heat != 1 {
		t.Fatalf("second = %+v", second)
	}
	if want := 2.0 / 60; lines[0].HeatHours != want {
		t.Fatalf("heat hours = %v, want %v", lines[0].HeatHours, want)
	}
}

func TestSeriesSkipsBrokenLinesAndAPIChecksRange(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := s.save([]device{{ID: testID, IP: "192.0.2.10", Key: testKey}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.dayPath(now.Format(time.DateOnly)), []byte("{\"t\":\"broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.record(reading{T: now, ID: testID, DPS: map[string]any{"24": 215.0}}); err != nil {
		t.Fatal(err)
	}
	mux := newMux(s, make(chan struct{}, 1), time.Minute)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?range=24h", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"temp":21.5`) {
		t.Fatalf("series %d: %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?range=1y", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range: %d", rec.Code)
	}
}

func TestSavePublishesAddressesWithoutKeys(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := []device{
		{ID: "a", IP: "192.0.2.10", MAC: "D8-D6-68-00-00-01", Key: testKey},
		{ID: "b", IP: "192.0.2.11", Key: testKey},
	}
	if err := s.save(in); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, neighborsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testKey) {
		t.Fatalf("key in %s", raw)
	}
	got, err := readNeighbors(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (neighbor{IP: "192.0.2.10", MAC: "d8:d6:68:00:00:01"}) {
		t.Fatalf("neighbors = %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if err := s.save([]device{{ID: "a", IP: "192.0.2.10", MAC: "nonsense", Key: testKey}}); err == nil {
		t.Fatal("want an error for a bad MAC")
	}
	if got, err := readNeighbors(filepath.Join(t.TempDir(), "missing")); err != nil || got != nil {
		t.Fatalf("missing file: %v, %v", got, err)
	}
}

func TestSyncNeighbors(t *testing.T) {
	var set []string
	var del []string
	apply := func(n neighbor) error {
		if n.IP == "192.0.2.99" {
			return errors.New("no interface")
		}
		set = append(set, n.IP)
		return nil
	}
	remove := func(ip string) error { del = append(del, ip); return nil }
	pinned := map[string]string{}

	syncNeighbors([]neighbor{{"192.0.2.1", "aa"}, {"192.0.2.99", "bb"}}, pinned, apply, remove)
	if len(pinned) != 1 || pinned["192.0.2.1"] != "aa" {
		t.Fatalf("pinned = %v", pinned)
	}
	// Set again every round, in case the kernel dropped the entry.
	syncNeighbors([]neighbor{{"192.0.2.1", "aa"}}, pinned, apply, remove)
	if len(set) != 2 || len(del) != 0 {
		t.Fatalf("set = %v, del = %v", set, del)
	}
	syncNeighbors(nil, pinned, apply, remove)
	if len(del) != 1 || del[0] != "192.0.2.1" || len(pinned) != 0 {
		t.Fatalf("del = %v, pinned = %v", del, pinned)
	}
}

func TestEncodeNeighbor(t *testing.T) {
	ip := net.ParseIP("192.0.2.10").To4()
	mac, _ := net.ParseMAC("d8:d6:68:00:00:01")
	b := encodeNeighbor(rtmNewNeigh, 0, 7, ip, mac)
	if int(binary.LittleEndian.Uint32(b)) != len(b) || len(b)%4 != 0 {
		t.Fatalf("length %d in a %d byte message", binary.LittleEndian.Uint32(b), len(b))
	}
	if binary.LittleEndian.Uint32(b[20:]) != 7 || binary.LittleEndian.Uint16(b[24:]) != nudPermanent {
		t.Fatalf("ndmsg = % x", b[16:28])
	}
	if !strings.Contains(string(b), string(ip)) || !strings.Contains(string(b), string(mac)) {
		t.Fatalf("addresses missing: % x", b)
	}
}

func TestNetlinkAck(t *testing.T) {
	reply := func(kind uint16, code int32) []byte {
		b := make([]byte, 20)
		binary.LittleEndian.PutUint16(b[4:], kind)
		binary.LittleEndian.PutUint32(b[16:], uint32(code))
		return b
	}
	if err := netlinkAck(reply(syscall.NLMSG_ERROR, 0)); err != nil {
		t.Fatal(err)
	}
	if err := netlinkAck(reply(syscall.NLMSG_ERROR, -1)); err != syscall.EPERM {
		t.Fatalf("err = %v", err)
	}
	if err := netlinkAck(reply(16, 0)); err == nil {
		t.Fatal("want an error for another message type")
	}
}
