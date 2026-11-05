package redirhub

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestExactRedirectBeforeSite(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	s := &site{}
	// /old-pricing exists on the site, but the exact rule wins.
	w := serve(t, env.h, s, request("GET", "https://promo.example.com/old-pricing?utm=x"))
	if w.Code != 301 || w.Header().Get("Location") != "https://promo.example.com/pricing" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
	if s.calls != 0 {
		t.Error("site must not run for an exact rule")
	}
	w = serve(t, env.h, s, request("GET", "https://promo.example.com/promo"))
	if w.Code != 302 || w.Header().Get("Location") != "https://sale.example.com/" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestExactMatchIgnoresSchemeAndCase(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	w := serve(t, env.h, &site{}, request("GET", "http://PROMO.example.com:8080/old-pricing"))
	if w.Code != 301 {
		t.Fatalf("got %d", w.Code)
	}
	// paths are case-sensitive and trailing slashes matter
	w = serve(t, env.h, &site{}, request("GET", "https://promo.example.com/Old-Pricing"))
	if w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestNonASCIIPath(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	// the source is written percent-encoded; the request arrives encoded
	// or as raw UTF-8, both match after decoding
	for _, u := range []string{"https://shop.example.com/caf%C3%A9", "https://shop.example.com/café"} {
		s := &site{}
		w := serve(t, env.h, s, request("GET", u))
		if w.Code != 308 || w.Header().Get("Location") != "https://www.example.net/cafe" || s.calls != 0 {
			t.Errorf("%s: got %d %q", u, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestCatchAllOnlyOnSite404(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	s := &site{}

	w := serve(t, env.h, s, request("GET", "https://shop.example.com/exists?a=1"))
	if w.Code != 200 || w.Body.String() != "site page" {
		t.Fatalf("working page: got %d %q", w.Code, w.Body.String())
	}

	w = serve(t, env.h, s, request("GET", "https://shop.example.com/gone/page?a=1"))
	if w.Code != 301 {
		t.Fatalf("got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "https://www.example.net/gone/page?a=1" {
		t.Errorf("Location = %q", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("404 body leaked: %q", w.Body.String())
	}
	if w.Header().Get("X-Site-404") != "" || w.Header().Get("Content-Type") != "" {
		t.Errorf("404 headers leaked: %v", w.Header())
	}
}

func TestCatchAllOnHandlerError404(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return caddyhttp.Error(http.StatusNotFound, nil)
	})
	w := serve(t, env.h, next, request("GET", "https://shop.example.com/missing"))
	if w.Code != 301 || w.Header().Get("Location") != "https://www.example.net/missing" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestCheckPath(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	s := &site{}
	w := serve(t, env.h, s, request("GET", "https://promo.example.com/_redirhub/check"))
	if w.Code != 200 || w.Body.String() != "token=EEEXQ4RS etag=etag-1" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("headers = %v", w.Header())
	}
	w = serve(t, env.h, s, request("HEAD", "https://shop.example.com/_redirhub/check"))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD got %d %q", w.Code, w.Body.String())
	}
	if s.calls != 0 {
		t.Error("site must not run for the check path")
	}
	// the check path wins over a catch-all rule and is never reported
	if len(env.hits.allLines()) != 0 {
		t.Error("check path must not be reported")
	}
}

func TestCheckPathBeforeExactRule(t *testing.T) {
	env := newTestEnv(t, "@host a.example.com check=TOK\n@check /_redirhub/check\nhttps://a.example.com/_redirhub/check\thttps://x.example.com/\n")
	w := serve(t, env.h, &site{}, request("GET", "https://a.example.com/_redirhub/check"))
	if w.Code != 200 || w.Body.String() != "token=TOK etag=etag-1" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestHEADRedirect(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	w := serve(t, env.h, &site{}, request("HEAD", "https://promo.example.com/old-pricing"))
	if w.Code != 301 || w.Header().Get("Location") == "" {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPassthrough(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	for _, tc := range []struct{ method, url string }{
		{"POST", "https://promo.example.com/old-pricing"},
		{"PUT", "https://shop.example.com/missing"},
		{"POST", "https://promo.example.com/_redirhub/check"},
		{"GET", "https://unlisted.example.com/old-pricing"},
		{"GET", "https://unlisted.example.com/_redirhub/check"},
	} {
		s := &site{}
		w := serve(t, env.h, s, request(tc.method, tc.url))
		if s.calls != 1 || w.Header().Get("Location") != "" {
			t.Errorf("%s %s: calls=%d code=%d location=%q", tc.method, tc.url, s.calls, w.Code, w.Header().Get("Location"))
		}
	}
	// the site's own 404 for a host without catch-all passes through
	w := serve(t, env.h, &site{}, request("GET", "https://promo.example.com/nope"))
	if w.Code != 404 || w.Body.String() != "site 404 page" {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestNothingBeforeFirstSuccess(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"e"`)
	rules.set("", "", http.StatusInternalServerError)
	f := newFetcher(rules.URL, nil, "")
	f.fetch(context.Background())
	h := &Handler{rules: f}
	for _, u := range []string{"https://promo.example.com/old-pricing", "https://promo.example.com/_redirhub/check", "https://shop.example.com/missing"} {
		s := &site{}
		w := serve(t, h, s, request("GET", u))
		if s.calls != 1 || w.Header().Get("Location") != "" {
			t.Errorf("%s: redirected or answered before the first download (%d)", u, w.Code)
		}
	}
	// a handler with no fetcher at all also passes through
	s := &site{}
	serve(t, &Handler{}, s, request("GET", "https://promo.example.com/old-pricing"))
	if s.calls != 1 {
		t.Error("nil fetcher must pass through")
	}
}

func TestSiteErrorPassesThrough(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	boom := caddyhttp.Error(http.StatusBadGateway, io.ErrUnexpectedEOF)
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { return boom })
	w := newRecorder()
	err := env.h.ServeHTTP(w, request("GET", "https://shop.example.com/x"), next)
	if err == nil || w.Header().Get("Location") != "" {
		t.Fatalf("err=%v location=%q", err, w.Header().Get("Location"))
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	d := caddyfile.NewTestDispenser(`redirhub {
		rules_url https://rules.example.test/abc/TOKEN.txt
		key reh_int_secret
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.RulesURL != "https://rules.example.test/abc/TOKEN.txt" || h.Key != "reh_int_secret" {
		t.Errorf("got %+v", h)
	}

	d = caddyfile.NewTestDispenser(`redirhub https://rules.example.test/x.txt`)
	h = Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil || h.RulesURL != "https://rules.example.test/x.txt" {
		t.Errorf("inline form: %v %+v", err, h)
	}

	for _, bad := range []string{`redirhub`, `redirhub { key k }`, `redirhub { bogus 1 }`, `redirhub a b`, "redirhub {\n rules_url\n}", "redirhub {\n rules_url a b\n}"} {
		h = Handler{}
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}

	var p Permission
	if err := p.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`redirhub`)); err != nil || p.RulesURL != "" {
		t.Errorf("permission: %v %+v", err, p)
	}
	if err := p.UnmarshalCaddyfile(caddyfile.NewTestDispenser("redirhub {\n rules_url https://r.example.test/x.txt\n}")); err != nil || p.RulesURL != "https://r.example.test/x.txt" {
		t.Errorf("permission block: %v %+v", err, p)
	}
}

func TestProvisionSharesOnePoller(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"e"`)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	a := &Handler{RulesURL: rules.URL + "/shared.txt", Key: "k1"}
	b := &Handler{RulesURL: rules.URL + "/shared.txt", Key: "k2"}
	for _, h := range []*Handler{a, b} {
		if err := h.Provision(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if a.rules != b.rules {
		t.Error("handlers with the same rules_url must share one poller")
	}
	if a.reporter == b.reporter {
		t.Error("different keys need different reporters")
	}
	if n, _ := rulesPool.References(rules.URL + "/shared.txt"); n != 2 {
		t.Errorf("references = %d", n)
	}
	a.Cleanup()
	b.Cleanup()
	if _, ok := rulesPool.References(rules.URL + "/shared.txt"); ok {
		t.Error("poller must be released after the last cleanup")
	}

	bad := &Handler{RulesURL: "/relative"}
	if err := bad.Provision(ctx); err == nil {
		t.Error("expected an error for a relative rules_url")
	}
}

func newRecorder() *responseRecorder { return &responseRecorder{header: http.Header{}} }

type responseRecorder struct {
	header http.Header
	code   int
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *responseRecorder) WriteHeader(code int)        { r.code = code }
