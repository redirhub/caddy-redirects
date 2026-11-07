package redirects

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strconv"
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

// The site's 404 page that the catch-all replaces is discarded as it is
// written, not buffered: a huge one costs no memory.
func TestCatchAllDiscardsALarge404Body(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	const total = 64 << 20
	chunk := make([]byte, 1<<20)
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		for written := 0; written < total/2; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
		http.NewResponseController(w).Flush()
		// the other half through ReadFrom, as file_server sends a file
		_, err := io.Copy(w, io.LimitReader(zeros{}, total/2))
		return err
	})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	w := serve(t, env.h, next, request("GET", "https://shop.example.com/missing"))
	runtime.ReadMemStats(&after)
	if w.Code != 301 || w.Header().Get("Location") != "https://www.example.net/missing" || w.Body.Len() != 0 {
		t.Fatalf("got %d %q body %d bytes", w.Code, w.Header().Get("Location"), w.Body.Len())
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("allocated %d MiB for a discarded 404", alloc>>20)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
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
	env := newTestEnv(t, "@host a.example.com check=TOK\n@check /_redirhub/check\na.example.com/_redirhub/check\thttps://x.example.com/\n")
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

func TestProvisionChecksReport404BeforeStartingThePoller(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"e"`)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	for value, ok := range map[string]bool{"": true, "rules": true, "all": true, "none": false} {
		url := rules.URL + "/report404-" + value + ".txt"
		h := &Handler{RulesURL: url, Report404: value}
		err := h.Provision(ctx)
		if (err == nil) != ok {
			t.Errorf("report_404 %q: %v", value, err)
		}
		_, started := rulesPool.References(url)
		if started != ok {
			t.Errorf("report_404 %q: poller started %v", value, started)
		}
		h.Cleanup()
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	d := caddyfile.NewTestDispenser(`redirects {
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

	d = caddyfile.NewTestDispenser(`redirects https://rules.example.test/x.txt`)
	h = Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil || h.RulesURL != "https://rules.example.test/x.txt" {
		t.Errorf("inline form: %v %+v", err, h)
	}

	d = caddyfile.NewTestDispenser("redirects https://rules.example.test/x.txt {\n report_404 all\n}")
	h = Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil || h.Report404 != Report404All {
		t.Errorf("report_404 all: %v %+v", err, h)
	}

	for _, bad := range []string{`redirects`, `redirects { key k }`, `redirects { bogus 1 }`, "redirects x {\n report_404 some\n}", "redirects x {\n report_404\n}", `redirects a b`, "redirects {\n rules_url\n}", "redirects {\n rules_url a b\n}"} {
		h = Handler{}
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}

	var p Permission
	if err := p.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`redirects`)); err != nil || p.RulesURL != "" {
		t.Errorf("permission: %v %+v", err, p)
	}
	if err := p.UnmarshalCaddyfile(caddyfile.NewTestDispenser("redirects {\n rules_url https://r.example.test/x.txt\n}")); err != nil || p.RulesURL != "https://r.example.test/x.txt" {
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

func TestPoweredByOnRedirectsOnly(t *testing.T) {
	env := newTestEnv(t, strings.Replace(sampleRules, "@check", "@powered-by RedirHub (integrated)\n@check", 1))
	for _, u := range []string{
		"https://promo.example.com/old-pricing", // exact rule
		"https://shop.example.com/missing",      // catch-all on the site's 404
	} {
		w := serve(t, env.h, &site{}, request("GET", u))
		if w.Code < 300 || w.Code > 399 || w.Header().Get("X-Powered-By") != "RedirHub (integrated)" {
			t.Errorf("%s: got %d X-Powered-By %q", u, w.Code, w.Header().Get("X-Powered-By"))
		}
	}
	for _, u := range []string{
		"https://promo.example.com/_redirhub/check", // the check path
		"https://promo.example.com/exists",          // the site's own page
		"https://promo.example.com/nothing-here",    // the site's 404
	} {
		w := serve(t, env.h, &site{}, request("GET", u))
		if got := w.Header().Get("X-Powered-By"); got != "" {
			t.Errorf("%s: got %d with X-Powered-By %q", u, w.Code, got)
		}
	}

	// without the directive, no header
	env = newTestEnv(t, sampleRules)
	w := serve(t, env.h, &site{}, request("GET", "https://promo.example.com/old-pricing"))
	if w.Code != 301 || w.Header().Get("X-Powered-By") != "" {
		t.Errorf("got %d X-Powered-By %q", w.Code, w.Header().Get("X-Powered-By"))
	}
}

const frameRules = "@format 1\n@hits HITS\n" +
	"@host frame.example.test check=FAKEfrme\n" +
	"frame.example.test/tour\thttps://tour.example.test/a b?x=1&y=\"2\"<\tframe\n" +
	"frame.example.test/*\thttps://www.example.test/\tframe\tpq\n"

func TestFramePage(t *testing.T) {
	env := newTestEnv(t, frameRules)
	s := &site{}
	w := serve(t, env.h, s, request("GET", "https://frame.example.test/tour?ref=nl"))
	if w.Code != 200 || w.Header().Get("Location") != "" || s.calls != 0 {
		t.Fatalf("got %d Location %q, %d site calls", w.Code, w.Header().Get("Location"), s.calls)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := w.Body.String()
	if w.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length %q for %d bytes", w.Header().Get("Content-Length"), len(body))
	}
	// The src is HTML-escaped, never able to close the attribute.
	for _, want := range []string{
		`<iframe src="https://tour.example.test/a b?x=1&amp;y=&#34;2&#34;&lt;" allowfullscreen></iframe>`,
		"<title>tour.example.test</title>",
		`<meta charset="utf-8">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s:\n%s", want, body)
		}
	}
	lines := env.hits.waitLines(t, 1)
	if !strings.Contains(lines[0], `" 200 0 "-" "-" file=frame.example.test/tour destination=https://tour.example.test/a%20b?x=1&y=%222%22<`) {
		t.Errorf("report line %s", lines[0])
	}
}

func TestFramePageHEAD(t *testing.T) {
	env := newTestEnv(t, frameRules)
	get := serve(t, env.h, &site{}, request("GET", "https://frame.example.test/tour"))
	w := serve(t, env.h, &site{}, request("HEAD", "https://frame.example.test/tour"))
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		w.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
		t.Fatalf("got %d, %d body bytes, headers %v", w.Code, w.Body.Len(), w.Header())
	}
}

func TestFrameCatchAllOnSite404(t *testing.T) {
	env := newTestEnv(t, frameRules)
	w := serve(t, env.h, &site{}, request("GET", "https://frame.example.test/exists"))
	if w.Code != 200 || w.Body.String() != "site page" {
		t.Fatalf("working page: got %d %q", w.Code, w.Body.String())
	}
	w = serve(t, env.h, &site{}, request("GET", "https://frame.example.test/gone/page?a=1"))
	if w.Code != 200 || w.Header().Get("X-Site-404") != "" || strings.Contains(w.Body.String(), "site 404 page") ||
		!strings.Contains(w.Body.String(), `<iframe src="https://www.example.test/gone/page?a=1"`) {
		t.Fatalf("got %d %v\n%s", w.Code, w.Header(), w.Body.String())
	}
}
