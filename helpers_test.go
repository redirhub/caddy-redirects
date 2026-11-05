package redirhub

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "redirhub-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_DATA_HOME", dir)
	flushInterval = 50 * time.Millisecond
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const sampleRules = "@format 1\n" +
	"@refresh 60\n" +
	"@hits HITS\n" +
	"@check /_redirhub/check\n" +
	"@host promo.example.com check=EEEXQ4RS report404\n" +
	"@host shop.example.com check=K3Vb9QmX\n" +
	"https://promo.example.com/old-pricing\thttps://promo.example.com/pricing\n" +
	"https://promo.example.com/promo\thttps://sale.example.com/\t302\n" +
	"https://shop.example.com/*\thttps://www.example.net/\t301\tpq\n" +
	"https://shop.example.com/caf%C3%A9\thttps://www.example.net/cafe\t308\n"

// rulesServer serves a rules file with ETag / 304 support.
type rulesServer struct {
	*httptest.Server
	mu          sync.Mutex
	body        string
	etag        string
	status      int // forced status when non-zero
	requests    int
	ifNoneMatch []string
}

func newRulesServer(t *testing.T, body, etag string) *rulesServer {
	t.Helper()
	rs := &rulesServer{body: body, etag: etag}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.requests++
		rs.ifNoneMatch = append(rs.ifNoneMatch, r.Header.Get("If-None-Match"))
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "no auth expected", http.StatusBadRequest)
			return
		}
		if rs.status != 0 {
			w.WriteHeader(rs.status)
			return
		}
		if rs.etag != "" {
			w.Header().Set("ETag", rs.etag)
			if r.Header.Get("If-None-Match") == rs.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, rs.body)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *rulesServer) set(body, etag string, status int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.body, rs.etag, rs.status = body, etag, status
}

// hitsServer records report posts.
type hitsServer struct {
	*httptest.Server
	mu    sync.Mutex
	posts []hitsPost
	got   chan struct{}
}

type hitsPost struct {
	header http.Header
	lines  []string
	raw    int
}

func newHitsServer(t *testing.T) *hitsServer {
	t.Helper()
	hs := &hitsServer{got: make(chan struct{}, 1000)}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := raw
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				http.Error(w, "bad gzip", http.StatusBadRequest)
				return
			}
			body, _ = io.ReadAll(zr)
		}
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		hs.mu.Lock()
		hs.posts = append(hs.posts, hitsPost{header: r.Header.Clone(), lines: lines, raw: len(raw)})
		hs.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		hs.got <- struct{}{}
	}))
	t.Cleanup(hs.Close)
	return hs
}

func (hs *hitsServer) allLines() []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	var out []string
	for _, p := range hs.posts {
		out = append(out, p.lines...)
	}
	return out
}

// waitLines waits until at least n lines arrived.
func (hs *hitsServer) waitLines(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if l := hs.allLines(); len(l) >= n {
			return l
		}
		select {
		case <-hs.got:
		case <-deadline:
			t.Fatalf("got %d report lines, want %d", len(hs.allLines()), n)
		}
	}
}

// testHandler builds a handler on a fresh fetcher (no persistence) and a
// running reporter, with the rules downloaded once.
type testEnv struct {
	h     *Handler
	f     *fetcher
	rules *rulesServer
	hits  *hitsServer
}

func newTestEnv(t *testing.T, body string) *testEnv {
	t.Helper()
	hits := newHitsServer(t)
	rules := newRulesServer(t, strings.ReplaceAll(body, "HITS", hits.URL+"/_edge/hits"), `"etag-1"`)
	f := newFetcher(rules.URL+"/abc/TOKEN.txt", nil, "")
	f.fetch(context.Background())
	r := newReporter("reh_int_test", func() string {
		if rs := f.Rules(); rs != nil {
			return rs.HitsURL
		}
		return ""
	}, nil)
	r.start()
	t.Cleanup(func() { r.Destruct() })
	h := &Handler{rules: f, reporter: r, key: "reh_int_test",
		now: func() time.Time { return time.Date(2026, 10, 31, 19, 10, 0, 0, time.UTC) }}
	return &testEnv{h: h, f: f, rules: rules, hits: hits}
}

// site is a next handler that answers 200 for /exists and 404 otherwise.
type site struct {
	calls int
}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	s.calls++
	if r.URL.Path == "/exists" || r.URL.Path == "/old-pricing" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "site page")
		return nil
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("X-Site-404", "1")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "site 404 page")
	return nil
}

var _ caddyhttp.Handler = (*site)(nil)

func request(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	vars := map[string]any{caddyhttp.ClientIPVarKey: "203.0.113.9"}
	return r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, vars))
}

func serve(t *testing.T, h *Handler, next caddyhttp.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	if err := h.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return w
}
