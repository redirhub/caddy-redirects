package redirects

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const (
	hubToken = "SUBSCRIBERJWT"
	hubTopic = "integration/RULESTOKEN"
)

// fakeHub is a Mercure hub for one topic: it checks the bearer token and the
// topic, keeps each stream open with heartbeats, and sends what publish()
// is given to every open stream.
type fakeHub struct {
	*httptest.Server
	mu          sync.Mutex
	status      int // forced answer when non-zero
	silent      bool
	connects    int
	lastEventID []string
	streams     map[int]chan string
	nextID      int
	drop        chan struct{}
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{streams: map[int]chan string{}, drop: make(chan struct{})}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(func() {
		h.dropAll()
		h.Server.CloseClientConnections()
		h.Close()
	})
	return h
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	status, silent, drop := h.status, h.silent, h.drop
	h.mu.Unlock()
	if status != 0 {
		http.Error(w, "forced", status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+hubToken || r.URL.Query().Get("topic") != hubTopic {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Accept") != "text/event-stream" {
		http.Error(w, "not sse", http.StatusBadRequest)
		return
	}
	events := make(chan string, 16)
	h.mu.Lock()
	h.connects++
	h.lastEventID = append(h.lastEventID, r.Header.Get("Last-Event-ID"))
	id := h.nextID
	h.nextID++
	h.streams[id] = events
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.streams, id)
		h.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Millisecond)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-drop:
			return
		case event := <-events:
			io.WriteString(w, event)
			flusher.Flush()
		case <-heartbeat.C:
			if !silent {
				io.WriteString(w, ":\n")
				flusher.Flush()
			}
		}
	}
}

// publish sends one Mercure update to every open stream.
func (h *fakeHub) publish(id, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, events := range h.streams {
		events <- fmt.Sprintf("id: %s\ndata: %s\n\n", id, data)
	}
}

// dropAll ends every open stream; later ones stay open.
func (h *fakeHub) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.drop)
	h.drop = make(chan struct{})
}

func (h *fakeHub) set(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = status
}

func (h *fakeHub) open() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

func (h *fakeHub) subscribeURL() string {
	return h.URL + "/.well-known/mercure?topic=integration%2FRULESTOKEN"
}

func notifyRules(hub *fakeHub, extra string) string {
	return "@format 1\n@refresh 60\n@notify " + hub.subscribeURL() + " token=" + hubToken + "\n" +
		"@host promo.example.com check=EEEXQ4RS\n" + extra
}

// shortNotify shortens every push timing for one test; @refresh is
// replaced by pollOverride.
func shortNotify(t *testing.T, poll time.Duration) {
	t.Helper()
	old := []time.Duration{pollOverride, notifyIdleTimeout, notifyMinBackoff, notifyMaxBackoff, notifyStableAfter, notifiedPoll}
	pollOverride, notifyIdleTimeout, notifyMinBackoff, notifyMaxBackoff, notifyStableAfter, notifiedPoll =
		poll, 200*time.Millisecond, 10*time.Millisecond, 40*time.Millisecond, time.Minute, time.Hour
	t.Cleanup(func() {
		pollOverride, notifyIdleTimeout, notifyMinBackoff, notifyMaxBackoff, notifyStableAfter, notifiedPoll =
			old[0], old[1], old[2], old[3], old[4], old[5]
	})
}

func (rs *rulesServer) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.requests
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func startFetcher(t *testing.T, url string, logger *zap.Logger) *fetcher {
	t.Helper()
	f := newFetcher(url, logger, "")
	f.start()
	t.Cleanup(func() { f.Destruct() })
	return f
}

func TestParseNotifyAndPoweredBy(t *testing.T) {
	rs, err := ParseRules([]byte("@format 1\n@notify https://hub.example.test/.well-known/mercure?topic=integration%2FX token=JWT future=1\n@powered-by  RedirHub (integrated)  \n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if rs.NotifyURL != "https://hub.example.test/.well-known/mercure?topic=integration%2FX" || rs.NotifyToken != "JWT" || rs.PoweredBy != "RedirHub (integrated)" {
		t.Errorf("got %q %q %q", rs.NotifyURL, rs.NotifyToken, rs.PoweredBy)
	}
	for _, body := range []string{
		"@format 1\n",
		"@notify\n@powered-by\n",
		"@notify ftp://hub.example.test/x token=JWT\n@powered-by bad\x01name\n",
		"@notify /relative token=JWT\n@powered-by " + strings.Repeat("x", 129) + "\n",
	} {
		rs, err := ParseRules([]byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		if rs.NotifyURL != "" || rs.NotifyToken != "" || rs.PoweredBy != "" {
			t.Errorf("%q: got %q %q %q", body, rs.NotifyURL, rs.NotifyToken, rs.PoweredBy)
		}
	}
}

func TestNotifyMessageFetchesAtOnce(t *testing.T) {
	shortNotify(t, time.Hour) // @refresh would never come in this test
	hub := newFakeHub(t)
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	f := startFetcher(t, rules.URL, nil)

	// the first download names the hub; the connection then fetches once
	// more, for a change made before it was open
	waitFor(t, "the subscription", func() bool { return hub.open() == 1 && f.notify.Load().Connected() })
	waitFor(t, "the fetch on connect", func() bool { return rules.count() == 2 })

	rules.set(notifyRules(hub, "promo.example.com/new\thttps://example.net/\n"), `"v2"`, 0)
	start := time.Now()
	hub.publish("urn:uuid:1", `{"etag":"v2"}`)
	waitFor(t, "the new rules", func() bool { return f.Rules().ETag == "v2" })
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s", took)
	}
	if _, ok := f.Rules().Host("promo.example.com").Exact["/new"]; !ok {
		t.Error("new rule missing")
	}
	// the message is not trusted: a wrong or empty one still only causes
	// a conditional download
	hub.publish("urn:uuid:2", `garbage`)
	waitFor(t, "the download after any message", func() bool { return rules.count() == 4 })
	if f.Rules().ETag != "v2" {
		t.Error("a message changed the rules")
	}
}

func TestNotifyStretchesThePollWhileConnected(t *testing.T) {
	shortNotify(t, 20*time.Millisecond) // stands for @refresh
	hub := newFakeHub(t)
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	f := startFetcher(t, rules.URL, nil)
	waitFor(t, "the subscription", func() bool { return f.notify.Load().Connected() })
	waitFor(t, "the fetch on connect", func() bool { return rules.count() == 2 })

	// connected: no poll for the hour, so none in 200ms (10 @refresh)
	time.Sleep(200 * time.Millisecond)
	if n := rules.count(); n != 2 {
		t.Fatalf("polled %d times while connected", n)
	}

	// the hub goes away: back to @refresh at once
	hub.set(http.StatusServiceUnavailable)
	hub.dropAll()
	waitFor(t, "the disconnect", func() bool { return !f.notify.Load().Connected() })
	waitFor(t, "polling every @refresh", func() bool { return rules.count() >= 6 })

	// back: one fetch on reconnect, then the poll stretches again
	hub.set(0)
	waitFor(t, "the reconnect", func() bool { return f.notify.Load().Connected() })
	time.Sleep(50 * time.Millisecond)
	n := rules.count()
	time.Sleep(200 * time.Millisecond)
	if got := rules.count(); got != n {
		t.Errorf("polled %d times after reconnecting", got-n)
	}
}

func TestWithoutNotifyPollsAsBefore(t *testing.T) {
	shortNotify(t, 20*time.Millisecond)
	rules := newRulesServer(t, sampleRules, `"v1"`)
	f := startFetcher(t, rules.URL, nil)
	waitFor(t, "polls", func() bool { return rules.count() >= 5 })
	if f.notify.Load() != nil {
		t.Error("a subscription without @notify")
	}
}

func TestNotifyReconnectsWithLastEventIDAndBackoff(t *testing.T) {
	shortNotify(t, time.Hour)
	hub := newFakeHub(t)
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	f := startFetcher(t, rules.URL, nil)
	waitFor(t, "the subscription", func() bool { return hub.open() == 1 })
	hub.publish("urn:uuid:7", `{"etag":"v1"}`)
	waitFor(t, "the message's fetch", func() bool { return rules.count() == 3 })

	hub.dropAll()
	waitFor(t, "the reconnect", func() bool { return hub.open() == 1 && f.notify.Load().Connected() })
	// a message may have been missed in between: one download on reconnect
	waitFor(t, "the reconnect's fetch", func() bool { return rules.count() == 4 })
	hub.mu.Lock()
	ids := append([]string(nil), hub.lastEventID...)
	hub.mu.Unlock()
	if len(ids) != 2 || ids[0] != "" || ids[1] != "urn:uuid:7" {
		t.Errorf("Last-Event-ID = %q", ids)
	}

	// a refusing hub is retried with a growing delay, capped
	hub.set(http.StatusUnauthorized)
	hub.dropAll()
	n := 0
	time.Sleep(300 * time.Millisecond)
	hub.mu.Lock()
	n = hub.connects
	hub.mu.Unlock()
	if n != 2 {
		t.Errorf("connects = %d", n)
	}
}

func TestNotifyIdleTimeoutDisconnects(t *testing.T) {
	shortNotify(t, time.Hour)
	hub := newFakeHub(t)
	hub.mu.Lock()
	hub.silent = true // no heartbeats
	hub.mu.Unlock()
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	f := startFetcher(t, rules.URL, nil)
	waitFor(t, "the subscription", func() bool { return f.notify.Load().Connected() })
	waitFor(t, "the idle timeout", func() bool { return !f.notify.Load().Connected() })
}

func TestNotifyFollowsTheDirective(t *testing.T) {
	shortNotify(t, 20*time.Millisecond)
	hub := newFakeHub(t)
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	f := startFetcher(t, rules.URL, nil)
	waitFor(t, "the subscription", func() bool { return hub.open() == 1 })
	first := f.notify.Load()

	// a new token (a rotated key) replaces the subscription
	rules.set(strings.Replace(notifyRules(hub, ""), "token="+hubToken, "token=OTHER", 1), `"v2"`, 0)
	hub.publish("urn:uuid:1", `{}`)
	waitFor(t, "the new subscription", func() bool { n := f.notify.Load(); return n != first && n != nil && n.token == "OTHER" })
	waitFor(t, "the old stream closed", func() bool { return hub.open() == 0 })

	// the directive goes: no subscription, polling every @refresh
	rules.set(sampleRules, `"v3"`, 0)
	waitFor(t, "no subscription", func() bool { return f.notify.Load() == nil })
	n := rules.count()
	waitFor(t, "polls", func() bool { return rules.count() >= n+3 })
}

func TestNotifyNeverLogsTheTokenOrTopic(t *testing.T) {
	shortNotify(t, time.Hour)
	core, logs := observer.New(zap.DebugLevel)
	hub := newFakeHub(t)
	hub.set(http.StatusUnauthorized)
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	startFetcher(t, rules.URL, zap.New(core))
	waitFor(t, "logged failures", func() bool { return logs.FilterMessageSnippet("disconnected").Len() >= 2 })

	// and a hub that does not listen at all
	addr := hub.URL
	hub.CloseClientConnections()
	hub.Close()
	waitFor(t, "a logged connection error", func() bool {
		for _, e := range logs.All() {
			if strings.Contains(fmt.Sprint(e.ContextMap()["error"]), "connect") {
				return true
			}
		}
		return false
	})
	for _, e := range logs.All() {
		line := e.Message + fmt.Sprint(e.ContextMap())
		if strings.Contains(line, hubToken) || strings.Contains(line, "RULESTOKEN") {
			t.Errorf("logged %q", line)
		}
		if hubURL, ok := e.ContextMap()["hub"]; ok && hubURL != addr+"/.well-known/mercure" {
			t.Errorf("hub = %q", hubURL)
		}
	}
}

// An outage is logged once at Warn and its end once at Info, however many
// attempts it takes; the attempts in between log at Debug.
func TestNotifyLogsAnOutageOnce(t *testing.T) {
	shortNotify(t, time.Hour)
	core, logs := observer.New(zap.InfoLevel)
	hub := newFakeHub(t)
	rules := newRulesServer(t, notifyRules(hub, ""), `"v1"`)
	f := startFetcher(t, rules.URL, zap.New(core))
	waitFor(t, "the subscription", func() bool { return f.notify.Load().Connected() })

	hub.set(http.StatusServiceUnavailable)
	hub.dropAll()
	waitFor(t, "several attempts", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return hub.connects == 1 && rules.count() >= 1 && !f.notify.Load().Connected()
	})
	time.Sleep(300 * time.Millisecond) // ~10 attempts at a 40 ms backoff
	hub.set(0)
	waitFor(t, "the reconnect", func() bool { return f.notify.Load().Connected() })
	// a connection that drops before it held is the same outage
	hub.dropAll()
	waitFor(t, "the second reconnect", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return hub.connects >= 3 && f.notify.Load().Connected()
	})

	count := func(level zapcore.Level, snippet string) int {
		n := 0
		for _, e := range logs.FilterMessageSnippet(snippet).All() {
			if e.Level == level {
				n++
			}
		}
		return n
	}
	if n := count(zap.WarnLevel, "notify disconnected"); n != 1 {
		t.Errorf("disconnect warnings = %d, want 1", n)
	}
	if n := count(zap.InfoLevel, "notify reconnected"); n != 1 {
		t.Errorf("reconnect infos = %d, want 1", n)
	}
	if n := count(zap.InfoLevel, "notify connected"); n != 1 {
		t.Errorf("connect infos = %d, want 1", n)
	}
}
