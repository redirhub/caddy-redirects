package redirects

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReportLineFormat(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	r := request("GET", "https://promo.example.com/old-pricing?utm=x")
	r.Header.Set("Referer", "https://google.example/")
	r.Header.Set("User-Agent", `Mozilla/5.0 "quoted" \slash`)
	serve(t, env.h, &site{}, r)

	lines := env.hits.waitLines(t, 1)
	want := `203.0.113.9 - - [31/Oct/2026:19:10:00 +0000] "GET https://promo.example.com/old-pricing?utm=x HTTP/1.1" 301 0 "https://google.example/" "Mozilla/5.0 \"quoted\" \\slash" file=promo.example.com/old-pricing destination=https://promo.example.com/pricing`
	if lines[0] != want {
		t.Errorf("line\n got %s\nwant %s", lines[0], want)
	}
	env.hits.mu.Lock()
	hdr := env.hits.posts[0].header
	env.hits.mu.Unlock()
	if hdr.Get("Authorization") != "Bearer reh_int_test" || hdr.Get("Content-Type") != "text/plain" || hdr.Get("Content-Encoding") != "gzip" {
		t.Errorf("headers = %v", hdr)
	}
}

func TestReportLineDefaults(t *testing.T) {
	r := httptest.NewRequest("HEAD", "http://shop.example.com:8080/a%20b", nil)
	r.RemoteAddr = "198.51.100.7:5555"
	got := hitLine(r, "shop.example.com", 404, 12, time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)), "", "")
	want := `198.51.100.7 - - [02/Jan/2026:02:04:05 +0000] "HEAD http://shop.example.com/a%20b HTTP/1.1" 404 12 "-" "-"`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if e := escape("a\nb\"c"); e != `a\x0ab\"c` {
		t.Errorf("escape = %q", e)
	}
}

func TestReportCatchAllAnd404(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	// catch-all redirect on shop (report404 off) is reported as a redirect
	serve(t, env.h, &site{}, request("GET", "https://shop.example.com/missing"))
	// the site's own 404 on shop is not reported (report404 off)
	serve(t, env.h, &site{}, request("GET", "https://unlisted.example.com/missing"))
	// promo has report404 and no catch-all: its 404 is reported
	serve(t, env.h, &site{}, request("GET", "https://promo.example.com/missing"))
	// 200s are never reported
	serve(t, env.h, &site{}, request("GET", "https://promo.example.com/exists"))
	serve(t, env.h, &site{}, request("GET", "https://shop.example.com/exists"))

	lines := env.hits.waitLines(t, 2)
	time.Sleep(3 * flushInterval)
	lines = env.hits.allLines()
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	if !strings.HasSuffix(lines[0], `"GET https://shop.example.com/missing HTTP/1.1" 301 0 "-" "-" file=shop.example.com/* destination=https://www.example.net/missing`) {
		t.Errorf("catch-all line = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"GET https://promo.example.com/missing HTTP/1.1" 404 13`) || strings.Contains(lines[1], "file=") || strings.Contains(lines[1], "destination=") {
		t.Errorf("404 line = %s", lines[1])
	}
}

func TestReportLineFieldsArePercentEncoded(t *testing.T) {
	r := request("GET", "https://a.example.com/x")
	got := hitLine(r, "a.example.com", 302, 0, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "a.example.com/x", "https://b.example.com/a b\"c\\d")
	if !strings.HasSuffix(got, ` file=a.example.com/x destination=https://b.example.com/a%20b%22c%5Cd`) {
		t.Errorf("got %s", got)
	}
	if got := hitLine(r, "a.example.com", 302, 0, time.Now(), "a.example.com/x", ""); !strings.HasSuffix(got, " file=a.example.com/x destination=-") {
		t.Errorf("got %s", got)
	}
}

func TestReport404All(t *testing.T) {
	env := newTestEnv(t, sampleRules)
	env.h.Report404 = Report404All
	// shop has no report404 and a catch-all: the catch-all still redirects
	serve(t, env.h, &site{}, request("GET", "https://shop.example.com/missing"))
	// an unlisted hostname: its 404 is reported, nothing else changes
	s := &site{}
	w := serve(t, env.h, s, request("GET", "https://Unlisted.example.com:8443/missing?q=1"))
	if w.Code != 404 || w.Body.String() != "site 404 page" || s.calls != 1 {
		t.Fatalf("unlisted 404 = %d %q", w.Code, w.Body.String())
	}
	// an unlisted hostname gets no check path and no rules
	w = serve(t, env.h, &site{}, request("GET", "https://unlisted.example.com/_redirhub/check"))
	if w.Code != 404 {
		t.Fatalf("check path on an unlisted host = %d", w.Code)
	}
	// 200s are never reported
	serve(t, env.h, &site{}, request("GET", "https://unlisted.example.com/exists"))

	env.hits.waitLines(t, 3)
	time.Sleep(3 * flushInterval)
	lines := env.hits.allLines()
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if !strings.Contains(lines[0], `301 0 "-" "-" file=shop.example.com/* destination=https://www.example.net/missing`) {
		t.Errorf("catch-all line = %s", lines[0])
	}
	if !strings.HasSuffix(lines[1], `"GET https://unlisted.example.com/missing?q=1 HTTP/1.1" 404 13 "-" "-"`) {
		t.Errorf("unlisted 404 line = %s", lines[1])
	}
	if !strings.Contains(lines[2], `"GET https://unlisted.example.com/_redirhub/check HTTP/1.1" 404 13`) {
		t.Errorf("unlisted check 404 line = %s", lines[2])
	}
}

func TestReport404Off(t *testing.T) {
	env := newTestEnv(t, "@hits HITS\n@host a.example.com check=T\n")
	s := &site{}
	w := serve(t, env.h, s, request("GET", "https://a.example.com/missing"))
	if w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
	time.Sleep(4 * flushInterval)
	if l := env.hits.allLines(); len(l) != 0 {
		t.Errorf("unexpected report: %q", l)
	}
}

func TestNoHitsNoKeyNoReport(t *testing.T) {
	env := newTestEnv(t, "@host a.example.com check=T\na.example.com/x\thttps://b.example.com/\n")
	w := serve(t, env.h, &site{}, request("GET", "https://a.example.com/x"))
	if w.Code != 301 {
		t.Fatalf("got %d", w.Code)
	}
	time.Sleep(4 * flushInterval)
	if l := env.hits.allLines(); len(l) != 0 {
		t.Errorf("reported without @hits: %q", l)
	}
	env2 := newTestEnv(t, sampleRules)
	env2.h.key = ""
	serve(t, env2.h, &site{}, request("GET", "https://promo.example.com/old-pricing"))
	time.Sleep(4 * flushInterval)
	if l := env2.hits.allLines(); len(l) != 0 {
		t.Errorf("reported without a key: %q", l)
	}
}

func TestBatching(t *testing.T) {
	hits := newHitsServer(t)
	r := newReporter("k", func() string { return hits.URL }, nil)
	r.interval = time.Hour // only size limits flush
	r.start()
	for i := range 250 {
		r.Enqueue(fmt.Sprintf("line %d", i))
	}
	hits.waitLines(t, 200)
	hits.mu.Lock()
	if len(hits.posts) != 2 || len(hits.posts[0].lines) != 100 || len(hits.posts[1].lines) != 100 {
		t.Errorf("batches = %d", len(hits.posts))
	}
	if hits.posts[0].lines[0] != "line 0" || hits.posts[1].lines[99] != "line 199" {
		t.Errorf("order: %q %q", hits.posts[0].lines[0], hits.posts[1].lines[99])
	}
	hits.mu.Unlock()
	// Destruct sends the remaining 50
	r.Destruct()
	if n := len(hits.allLines()); n != 250 {
		t.Errorf("after destruct: %d lines", n)
	}
}

func TestBatchByteLimit(t *testing.T) {
	hits := newHitsServer(t)
	r := newReporter("k", func() string { return hits.URL }, nil)
	r.interval = time.Hour
	r.start()
	long := strings.Repeat("x", 1000)
	for range 70 {
		r.Enqueue(long)
	}
	hits.waitLines(t, 65)
	hits.mu.Lock()
	first := hits.posts[0]
	hits.mu.Unlock()
	size := 0
	for _, l := range first.lines {
		size += len(l) + 1
	}
	if size > maxBatchBytes || len(first.lines) != 65 {
		t.Errorf("first batch: %d lines, %d bytes", len(first.lines), size)
	}
	if first.raw >= size/10 {
		t.Errorf("gzip did not compress: %d raw bytes for %d", first.raw, size)
	}
	r.Destruct()
}

func TestEnqueueNeverBlocks(t *testing.T) {
	// a hits server that never answers: posts time out, the queue fills, lines drop
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	r := newReporter("k", func() string { return srv.URL }, nil)
	r.timeout = 200 * time.Millisecond
	r.start()
	done := make(chan struct{})
	go func() {
		for range queueSize * 3 {
			r.Enqueue("x")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked")
	}
	r.Destruct()
}

func TestReportFailureDropped(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := newReporter("k", func() string { return srv.URL }, nil)
	r.send([]byte("a\n"), 1)
	r.send([]byte("b\n"), 1)
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d (no retries expected)", n)
	}
}
