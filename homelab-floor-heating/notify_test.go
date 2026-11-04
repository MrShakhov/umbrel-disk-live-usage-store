package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "123456789:AAEabcdefghijklmnopqrstuvwxyz012345"

// testNotifier has no Telegram behind it: nothing in these tests may reach the internet.
func testNotifier(t *testing.T) *notifier {
	t.Helper()
	n, err := openNotifier(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n.api = "http://127.0.0.1:1"
	n.retry = nil
	return n
}

// fakeTelegram answers Bot API calls with the replies given per method and
// records every call.
type fakeTelegram struct {
	mu      sync.Mutex
	calls   []string // method names
	bodies  []map[string]any
	replies map[string][]string // queued replies per method; the last one repeats
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if !strings.HasPrefix(r.URL.Path, "/bot"+testToken+"/") {
		http.Error(w, `{"ok":false,"description":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &body)
	f.calls = append(f.calls, method)
	f.bodies = append(f.bodies, body)
	queue := f.replies[method]
	reply := `{"ok":true,"result":true}`
	if len(queue) > 0 {
		reply = queue[0]
		if len(queue) > 1 {
			f.replies[method] = queue[1:]
		}
	}
	w.Write([]byte(reply))
}

func withTelegram(t *testing.T, n *notifier, replies map[string][]string) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	n.api = srv.URL
	return f
}

func TestAlertsAfterFailsInARowAndOnRecovery(t *testing.T) {
	n := testNotifier(t)
	n.cfg.After = 3
	devices := []device{{ID: "a", Name: "Bath"}, {ID: "b"}}
	start := time.Date(2026, 11, 3, 19, 50, 0, 0, time.UTC)
	timeout := "dial tcp 192.0.2.10:6668: i/o timeout"
	round := func(minute int, errA, errB string) string {
		at := start.Add(time.Duration(minute) * time.Minute)
		return n.changes(devices, []reading{{T: at, ID: "a", Err: errA}, {T: at, ID: "b", Err: errB}})
	}

	// A single lost poll stays quiet.
	if got := round(0, timeout, "") + round(1, "", ""); got != "" {
		t.Fatalf("blip: %q", got)
	}
	if got := round(2, timeout, "") + round(3, timeout, ""); got != "" {
		t.Fatalf("two fails: %q", got)
	}
	got := round(4, timeout, "")
	if !strings.HasPrefix(got, "🔴 Bath stopped answering 2 min ago.\n"+timeout+"\n") || !strings.Contains(got, "router") {
		t.Fatalf("alert: %q", got)
	}
	// One alert per outage, and a device without a name goes by its ID.
	if got := round(5, timeout, "refused") + round(6, timeout, "refused"); got != "" {
		t.Fatalf("repeated: %q", got)
	}
	if got := round(7, timeout, "refused"); !strings.HasPrefix(got, "🔴 b stopped answering 2 min ago.") {
		t.Fatalf("second device: %q", got)
	}
	got = round(17*60+43, "", "")
	if got != "🟢 Bath answers again after 17 h 41 min.\n\n🟢 b answers again after 17 h 38 min." {
		t.Fatalf("recovery: %q", got)
	}
	if got := round(17*60+44, "", ""); got != "" || len(n.outages) != 0 {
		t.Fatalf("after recovery: %q, %v", got, n.outages)
	}

	// A device removed mid-outage is forgotten.
	round(18*60, timeout, "")
	n.changes(devices[1:], []reading{{T: start, ID: "b"}})
	if len(n.outages) != 0 {
		t.Fatalf("removed device kept: %v", n.outages)
	}
}

func TestObserveQueuesOnlyWhenSetUp(t *testing.T) {
	n := testNotifier(t)
	n.cfg.After = 1
	devices := []device{{ID: "a", Name: "Bath"}}
	n.observe(devices, []reading{{ID: "a", Err: "no route to host"}})
	if len(n.queue) != 0 {
		t.Fatal("queued without a token")
	}
	n.observe(devices, []reading{{ID: "a"}})
	n.cfg.Token, n.cfg.ChatID = testToken, "42"
	n.observe(devices, []reading{{ID: "a", Err: "no route to host"}})
	if text := <-n.queue; !strings.Contains(text, "Bath") || !strings.Contains(text, "MAC address") {
		t.Fatalf("queued %q", text)
	}
}

func TestNotifyAPINeverReturnsToken(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := testNotifier(t)
	mux := newMux(s, n, make(chan struct{}, 1), time.Minute)
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/notify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	for _, bad := range []string{
		`{"token":"nope","chat_id":"42"}`,
		`{"token":"` + testToken + `","chat_id":"forty-two"}`,
		`{"token":"` + testToken + `","chat_id":"42","after":61}`,
	} {
		if rec := do("PUT", bad); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), testToken) {
			t.Fatalf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if rec := do("PUT", `{"token":"`+testToken+`","chat_id":"42"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	// Changing the chat without a token keeps the stored one.
	if rec := do("PUT", `{"chat_id":" -1001234567890 ","after":3}`); rec.Code != http.StatusNoContent {
		t.Fatalf("change chat: %d %s", rec.Code, rec.Body)
	}
	rec := do("GET", "")
	if rec.Body.String() != `{"enabled":true,"chat_id":"-1001234567890","after":3}`+"\n" {
		t.Fatalf("status = %s", rec.Body)
	}
	info, err := os.Stat(n.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("notify.json: %v %v", info, err)
	}
	reopened, err := openNotifier(strings.TrimSuffix(n.path, notifyFile))
	if err != nil || reopened.cfg != (notifyConfig{testToken, "-1001234567890", 3}) {
		t.Fatalf("reopened = %+v, %v", reopened.cfg, err)
	}

	if rec := do("DELETE", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable: %d", rec.Code)
	}
	if st := n.status(); st.Enabled || st.ChatID != "" || st.After != defaultAfter {
		t.Fatalf("after disable: %+v", st)
	}
	if _, err := os.Stat(n.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("notify.json left: %v", err)
	}
}

func TestSendReportsErrorsWithoutToken(t *testing.T) {
	n := testNotifier(t)
	if err := n.send("hi"); !errors.Is(err, errNotSetUp) {
		t.Fatalf("not set up: %v", err)
	}
	n.cfg = notifyConfig{Token: testToken, ChatID: "42", After: 1}
	err := n.send("hi")
	if err == nil || strings.Contains(err.Error(), testToken) || n.status().Error != err.Error() {
		t.Fatalf("unreachable: %v, status %+v", err, n.status())
	}

	f := withTelegram(t, n, map[string][]string{
		"sendMessage": {`{"ok":false,"description":"Bad Request: chat not found"}`, `{"ok":true,"result":{}}`},
	})
	if err := n.send("hi"); err == nil || err.Error() != "Telegram says: Bad Request: chat not found" {
		t.Fatalf("telegram error: %v", err)
	}
	if err := n.send("hi"); err != nil || n.status().Error != "" {
		t.Fatalf("sent: %v, status %+v", err, n.status())
	}
	if f.bodies[1]["chat_id"] != "42" || f.bodies[1]["text"] != "hi" {
		t.Fatalf("request = %v", f.bodies[1])
	}
}

func TestDeliverRetriesAFailedMessage(t *testing.T) {
	n := testNotifier(t)
	n.cfg = notifyConfig{Token: testToken, ChatID: "42", After: 1}
	n.retry = []time.Duration{0, 0}
	f := withTelegram(t, n, map[string][]string{
		"sendMessage": {`{"ok":false,"description":"Too Many Requests"}`, `{"ok":true,"result":{}}`},
	})
	n.queue <- "first"
	n.queue <- "second"
	close(n.queue)
	n.deliver()
	if len(f.bodies) != 3 || f.bodies[1]["text"] != "first" || f.bodies[2]["text"] != "second" {
		t.Fatalf("calls = %v", f.bodies)
	}
}

func TestFindChatTakesTheLatestMessage(t *testing.T) {
	n := testNotifier(t)
	f := withTelegram(t, n, map[string][]string{"getUpdates": {
		`{"ok":true,"result":[]}`,
		`{"ok":true,"result":[{"message":{"chat":{"id":1,"first_name":"Old"}}},` +
			`{"my_chat_member":{"chat":{"id":-1001234567890,"title":"Home"}}}]}`,
	}})
	if _, _, err := n.findChat(testToken); !errors.Is(err, errNoChat) {
		t.Fatalf("no messages: %v", err)
	}
	id, name, err := n.findChat(testToken)
	if err != nil || id != "-1001234567890" || name != "Home" {
		t.Fatalf("chat = %q %q %v", id, name, err)
	}
	if _, _, err := n.findChat(""); err == nil || len(f.calls) != 2 {
		t.Fatalf("without a stored token: %v, %d calls", err, len(f.calls))
	}
}

func TestSpanAndHint(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:               "less than a minute",
		4*time.Minute + 20*time.Second: "4 min",
		17*time.Hour + 41*time.Minute:  "17 h 41 min",
		51*time.Hour + 10*time.Minute:  "2 d 3 h",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
	if hint("read tcp 192.0.2.10:6668: i/o timeout") != "" {
		t.Error("a read timeout is not a stuck server")
	}
	if !strings.Contains(hint("device closed the connection: wrong local key, or busy with another client"), "local key") {
		t.Error("no hint for a changed key")
	}
}
