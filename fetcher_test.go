package redirects

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestFetch304KeepsCopy(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"v1"`)
	f := newFetcher(rules.URL, nil, "")
	f.fetch(context.Background())
	first := f.Rules()
	if first == nil || first.ETag != "v1" {
		t.Fatalf("first = %+v", first)
	}
	f.fetch(context.Background())
	if f.Rules() != first {
		t.Error("304 must keep the same copy")
	}
	if got := rules.ifNoneMatch; len(got) != 2 || got[0] != "" || got[1] != `"v1"` {
		t.Errorf("If-None-Match = %q", got)
	}

	rules.set("@host new.example.com check=N\n", `"v2"`, 0)
	f.fetch(context.Background())
	if rs := f.Rules(); rs.ETag != "v2" || rs.Host("new.example.com") == nil || rs.Host("promo.example.com") != nil {
		t.Errorf("after change: %+v", rs)
	}
}

func TestFetchFailureKeepsLastGoodCopy(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"v1"`)
	f := newFetcher(rules.URL, nil, "")
	f.fetch(context.Background())
	good := f.Rules()

	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusForbidden} {
		rules.set(sampleRules, "", status)
		f.fetch(context.Background())
		if f.Rules() != good {
			t.Errorf("status %d replaced the last good copy", status)
		}
	}
	// a body that is not text counts as a parse failure
	rules.set("@host x.example.com\x00", `"bin"`, 0)
	f.fetch(context.Background())
	if f.Rules() != good {
		t.Error("binary body replaced the last good copy")
	}
	// a file in a format this module does not understand
	rules.set("@format 2\n@host other.example.com check=X\n", `"v9"`, 0)
	f.fetch(context.Background())
	if f.Rules() != good {
		t.Error("unknown @format replaced the last good copy")
	}
	// network failure
	rules.Close()
	f.fetch(context.Background())
	if f.Rules() != good {
		t.Error("network failure replaced the last good copy")
	}
}

func TestFetchTimeout(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 100 * time.Millisecond
	defer func() { fetchTimeout = old }()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	f := newFetcher(srv.URL, nil, "")
	start := time.Now()
	f.fetch(context.Background())
	if time.Since(start) > 2*time.Second || f.Rules() != nil {
		t.Error("slow server must time out without rules")
	}
}

// The header timeout does not cover the body: a large file over a slow
// link still arrives, within the body deadline.
func TestFetchSlowBodyWithinTheBodyDeadline(t *testing.T) {
	oldHeader, oldBody := fetchTimeout, fetchBodyTimeout
	fetchTimeout, fetchBodyTimeout = 100*time.Millisecond, 3*time.Second
	defer func() { fetchTimeout, fetchBodyTimeout = oldHeader, oldBody }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 400 ms for the body, four times the header timeout
		for _, line := range strings.SplitAfter(sampleRules, "\n") {
			fmt.Fprint(w, line)
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer srv.Close()
	f := newFetcher(srv.URL, nil, "")
	if err := f.fetchOnce(context.Background()); err != nil {
		t.Fatalf("slow body: %v", err)
	}
	if f.Rules().Host("promo.example.com") == nil {
		t.Error("slow body not used")
	}
}

func TestFetchBodyDeadline(t *testing.T) {
	oldHeader, oldBody := fetchTimeout, fetchBodyTimeout
	fetchTimeout, fetchBodyTimeout = 100*time.Millisecond, 300*time.Millisecond
	defer func() { fetchTimeout, fetchBodyTimeout = oldHeader, oldBody }()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "@host promo.example.com check=X\n")
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	f := newFetcher(srv.URL, nil, "")
	start := time.Now()
	err := f.fetchOnce(context.Background())
	if err == nil || f.Rules() != nil {
		t.Fatalf("a body that never ends must fail: %v", err)
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 2*time.Second {
		t.Errorf("gave up after %v, want the body deadline", took)
	}
}

func TestPersistFailureWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	rules := newRulesServer(t, sampleRules, `"v1"`)
	// the parent of the copy is a file: every save fails
	f := newFetcher(rules.URL, zap.New(core), filepath.Join(blocker, "rules"))
	f.fetch(context.Background())
	rules.set(sampleRules+"\n", `"v2"`, 0)
	f.fetch(context.Background())
	if f.Rules() == nil || f.Rules().ETag != "v2" {
		t.Fatal("a failed save must not stop the rules from being used")
	}
	saves := logs.FilterMessageSnippet("could not save")
	if saves.Len() != 1 || saves.All()[0].Level != zap.WarnLevel {
		t.Errorf("save failures logged %d times: %v", saves.Len(), saves.All())
	}
}

func TestUnknownFormatBeforeFirstSuccess(t *testing.T) {
	rules := newRulesServer(t, "@format 2\n@host promo.example.com check=X\n", `"v1"`)
	f := newFetcher(rules.URL, nil, "")
	f.fetch(context.Background())
	if f.Rules() != nil {
		t.Error("unknown format must not be used")
	}
}

func TestPersistedCopy(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rules")
	rules := newRulesServer(t, sampleRules, `"v1"`)
	f := newFetcher(rules.URL, nil, file)
	f.fetch(context.Background())

	// a restart while the rules server is down redirects from the saved copy
	rules.set("", "", http.StatusServiceUnavailable)
	g := newFetcher(rules.URL, nil, file)
	if rs := g.Rules(); rs == nil || rs.ETag != "v1" || rs.Host("promo.example.com") == nil {
		t.Fatalf("persisted copy = %+v", rs)
	}
	g.fetch(context.Background())
	if g.Rules() == nil {
		t.Fatal("failure dropped the persisted copy")
	}

	// the saved ETag is revalidated with If-None-Match
	rules.set(sampleRules, `"v1"`, 0)
	g.fetch(context.Background())
	if last := rules.ifNoneMatch[len(rules.ifNoneMatch)-1]; last != `"v1"` {
		t.Errorf("If-None-Match = %q", last)
	}
}

func TestPollLoop(t *testing.T) {
	oldPoll, oldRetry := pollOverride, retryInterval
	pollOverride, retryInterval = 20*time.Millisecond, 10*time.Millisecond
	defer func() { pollOverride, retryInterval = oldPoll, oldRetry }()

	rules := newRulesServer(t, sampleRules, `"v1"`)
	f := newFetcher(rules.URL, nil, "")
	f.start()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rules.mu.Lock()
		n := rules.requests
		rules.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d polls", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.Destruct()
	f.Destruct() // idempotent
	if f.Rules() == nil {
		t.Error("no rules after polling")
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://rules.example.test/abc/SECRET.txt"); got != "https://rules.example.test/…" {
		t.Errorf("got %q", got)
	}
}

func TestFetchErrorDoesNotLogTheToken(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // nothing listens any more: the download fails in the client
	f := newFetcher(addr+"/abc/SECRET.txt", zap.New(core), "")
	f.fetch(context.Background())

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("logged %d entries", len(entries))
	}
	for k, v := range entries[0].ContextMap() {
		if s := fmt.Sprint(v); strings.Contains(s, "SECRET") {
			t.Errorf("%s = %q holds the token", k, s)
		}
	}
	if got := fmt.Sprint(entries[0].ContextMap()["error"]); !strings.Contains(got, "…") {
		t.Errorf("error = %q, want the redacted URL", got)
	}
}
