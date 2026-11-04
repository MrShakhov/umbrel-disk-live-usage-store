package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Telegram alerts. A thermostat that fails a few polls in a row gets one
// message, and another once it answers again, so a single lost poll stays
// quiet. The bot token stays in notify.json (0600) and never leaves the app
// through the API, like the local keys.

const (
	notifyFile   = "notify.json"
	defaultAfter = 5
	maxAfter     = 60
	telegramAPI  = "https://api.telegram.org"
)

var (
	tokenPattern = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{30,}$`)
	chatPattern  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
	errNoChat    = errors.New("no messages yet: send any message to your bot, then try again")
)

type notifyConfig struct {
	Token  string `json:"token"`
	ChatID string `json:"chat_id"`
	After  int    `json:"after"` // failed polls in a row before an alert
}

func (c notifyConfig) enabled() bool { return c.Token != "" && c.ChatID != "" }

// notifyStatus is what the page may see: everything but the token.
type notifyStatus struct {
	Enabled bool   `json:"enabled"`
	ChatID  string `json:"chat_id,omitempty"`
	After   int    `json:"after"`
	Error   string `json:"error,omitempty"` // why the last message didn't go out
}

// outage follows one device from its first failed poll.
type outage struct {
	since   time.Time
	fails   int
	alerted bool
}

type notifier struct {
	path   string
	api    string // the Bot API base URL; tests point it at a fake
	client *http.Client
	retry  []time.Duration // waits between delivery attempts
	queue  chan string

	mu      sync.Mutex
	cfg     notifyConfig
	lastErr string
	outages map[string]*outage // by device ID
}

func openNotifier(dir string) (*notifier, error) {
	n := &notifier{
		path:    filepath.Join(dir, notifyFile),
		api:     telegramAPI,
		client:  &http.Client{Timeout: 15 * time.Second},
		retry:   []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute},
		queue:   make(chan string, 16),
		cfg:     notifyConfig{After: defaultAfter},
		outages: map[string]*outage{},
	}
	b, err := os.ReadFile(n.path)
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &n.cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", notifyFile, err)
	}
	return n, nil
}

func (n *notifier) status() notifyStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	return notifyStatus{Enabled: n.cfg.enabled(), ChatID: n.cfg.ChatID, After: n.cfg.After, Error: n.lastErr}
}

// configure stores new settings. An empty token keeps the stored one, so the
// page can change the chat without ever seeing the token.
func (n *notifier) configure(in notifyConfig) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	in.Token = cmp.Or(in.Token, n.cfg.Token)
	in.ChatID = strings.TrimSpace(in.ChatID)
	in.After = cmp.Or(in.After, defaultAfter)
	switch {
	case !tokenPattern.MatchString(in.Token):
		return errors.New("a bot token looks like 123456789:AAE…, copy it from @BotFather")
	case !chatPattern.MatchString(in.ChatID):
		return errors.New("a chat ID is a number such as 123456789 or -1001234567890")
	case in.After < 1 || in.After > maxAfter:
		return fmt.Errorf("alert after 1 to %d failed polls", maxAfter)
	}
	if err := writeJSON(n.path, in, 0o600); err != nil {
		return err
	}
	n.cfg, n.lastErr = in, ""
	return nil
}

// disable forgets the token and the chat.
func (n *notifier) disable() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := os.Remove(n.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	n.cfg, n.lastErr = notifyConfig{After: defaultAfter}, ""
	return nil
}

// observe takes one round of polls and queues a message about the devices
// that crossed the threshold or answer again.
func (n *notifier) observe(devices []device, round []reading) {
	n.mu.Lock()
	text, on := n.changes(devices, round), n.cfg.enabled()
	n.mu.Unlock()
	if text == "" || !on {
		return
	}
	select {
	case n.queue <- text:
	default:
		log.Printf("telegram: too many alerts waiting, dropped one")
	}
}

// changes updates the outages from a round and describes what changed. The
// caller holds n.mu.
func (n *notifier) changes(devices []device, round []reading) string {
	names := map[string]string{}
	for _, d := range devices {
		names[d.ID] = cmp.Or(d.Name, d.ID)
	}
	var down, up []string
	polled := map[string]bool{}
	for _, r := range round {
		polled[r.ID] = true
		o := n.outages[r.ID]
		if r.Err == "" {
			if o != nil && o.alerted {
				up = append(up, fmt.Sprintf("🟢 %s answers again after %s.", names[r.ID], span(r.T.Sub(o.since))))
			}
			delete(n.outages, r.ID)
			continue
		}
		if o == nil {
			o = &outage{since: r.T}
			n.outages[r.ID] = o
		}
		o.fails++
		if o.alerted || o.fails < n.cfg.After {
			continue
		}
		o.alerted = true
		msg := fmt.Sprintf("🔴 %s stopped answering %s ago.\n%s", names[r.ID], span(r.T.Sub(o.since)), r.Err)
		if h := hint(r.Err); h != "" {
			msg += "\n" + h
		}
		down = append(down, msg)
	}
	for id := range n.outages {
		if !polled[id] { // removed from the device list
			delete(n.outages, id)
		}
	}
	return strings.Join(append(down, up...), "\n\n")
}

// hint suggests a fix for the errors these thermostats are known for.
func hint(err string) string {
	switch {
	case strings.HasPrefix(err, "dial tcp") && strings.HasSuffix(err, "i/o timeout"):
		return "If it's still in your router's list of clients, its local server may be stuck: disconnect it from Wi-Fi for a minute, or turn its power off and on."
	case strings.Contains(err, "no route to host"), strings.Contains(err, "host is unreachable"):
		return "It may have stopped answering ARP requests: check its MAC address under Devices."
	case strings.Contains(err, "connection refused"):
		return "Something else answers at this address: check its IP under Devices."
	case strings.Contains(err, "local key"):
		return "If it was paired again in the mobile app, enter its new local key under Devices."
	}
	return ""
}

// span is a rounded duration for people: 4 min, 17 h 41 min, 2 d 3 h.
func span(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "less than a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 24*60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d d %d h", m/(24*60), m%(24*60)/60)
}

// deliver sends queued messages in order, retrying a failed one a few times.
// It runs for the life of the app.
func (n *notifier) deliver() {
	for text := range n.queue {
		for attempt := 0; ; attempt++ {
			err := n.send(text)
			if err == nil || errors.Is(err, errNotSetUp) {
				break
			}
			if attempt == len(n.retry) {
				log.Printf("telegram: gave up on an alert: %v", err)
				break
			}
			log.Printf("telegram: %v, trying again in %s", err, n.retry[attempt])
			time.Sleep(n.retry[attempt])
		}
	}
}

var errNotSetUp = errors.New("Telegram alerts are not set up")

// send posts one message to the stored chat and keeps the outcome for the page.
func (n *notifier) send(text string) error {
	n.mu.Lock()
	cfg := n.cfg
	n.mu.Unlock()
	if !cfg.enabled() {
		return errNotSetUp
	}
	err := n.call(cfg.Token, "sendMessage", map[string]any{
		"chat_id": cfg.ChatID, "text": text, "disable_web_page_preview": true,
	}, nil)
	n.mu.Lock()
	n.lastErr = ""
	if err != nil {
		n.lastErr = err.Error()
	}
	n.mu.Unlock()
	return err
}

type tgChat struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// findChat returns the chat that last wrote to the bot, so nobody has to look
// up a chat ID by hand. An empty token means the stored one.
func (n *notifier) findChat(token string) (id, name string, err error) {
	if token == "" {
		n.mu.Lock()
		token = n.cfg.Token
		n.mu.Unlock()
	}
	if !tokenPattern.MatchString(token) {
		return "", "", errors.New("enter the bot token first")
	}
	type post struct {
		Chat tgChat `json:"chat"`
	}
	var updates []struct {
		Message      *post `json:"message"`
		ChannelPost  *post `json:"channel_post"`
		MyChatMember *post `json:"my_chat_member"`
	}
	if err := n.call(token, "getUpdates", map[string]any{}, &updates); err != nil {
		return "", "", err
	}
	for i := len(updates) - 1; i >= 0; i-- {
		u := updates[i]
		for _, p := range []*post{u.Message, u.ChannelPost, u.MyChatMember} {
			if p != nil && p.Chat.ID != 0 {
				c := p.Chat
				return strconv.FormatInt(c.ID, 10), cmp.Or(c.Title, c.FirstName, c.Username), nil
			}
		}
	}
	return "", "", errNoChat
}

// call runs one Bot API method. Its errors never carry the URL, which holds the token.
func (n *notifier) call(token, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	res, err := n.client.Post(n.api+"/bot"+token+"/"+method, "application/json", bytes.NewReader(body))
	if err != nil {
		var u *url.Error
		if errors.As(err, &u) {
			err = u.Err
		}
		return fmt.Errorf("cannot reach Telegram: %v", err)
	}
	defer res.Body.Close()
	var reply struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply); err != nil {
		return fmt.Errorf("unexpected reply from Telegram (HTTP %d)", res.StatusCode)
	}
	if !reply.OK {
		return fmt.Errorf("Telegram says: %s", cmp.Or(reply.Description, res.Status))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(reply.Result, out)
}
