package redirects

import (
	"bufio"
	"bytes"
	"context"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden sample of format v1 in spec/v1, a copy of redirhub/integration-spec's
// (its README.md there says what each file is): the module must read rules.txt,
// answer every case of requests.tsv as it says, and report exactly the lines of
// reports.txt.

const specDir = "spec/v1"

// specTime is the time every line of spec/v1/reports.txt is stamped with.
var specTime = time.Date(2026, 10, 31, 19, 10, 0, 0, time.UTC)

func readSpec(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(specDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type specCase struct {
	name, method, url, referer, userAgent string
	site                                  int
	outcome                               string
	status                                int
	location, body                        string
	report                                int // line of reports.txt, 1-based; 0 for none
}

func specCases(t *testing.T) []specCase {
	t.Helper()
	var cases []specCase
	sc := bufio.NewScanner(bytes.NewReader(readSpec(t, "requests.tsv")))
	header := true
	for sc.Scan() {
		if header {
			header = false
			if !strings.HasPrefix(sc.Text(), "name\tmethod\turl\t") {
				t.Fatalf("requests.tsv header = %q", sc.Text())
			}
			continue
		}
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 11 {
			t.Fatalf("requests.tsv: %d columns in %q", len(f), sc.Text())
		}
		dash := func(s string) string {
			if s == "-" {
				return ""
			}
			return s
		}
		num := func(s string) int {
			if s == "-" {
				return 0
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("requests.tsv: %q is no number", s)
			}
			return n
		}
		cases = append(cases, specCase{
			name: f[0], method: f[1], url: f[2], referer: dash(f[3]), userAgent: dash(f[4]),
			site: num(f[5]), outcome: f[6], status: num(f[7]), location: dash(f[8]), body: dash(f[9]),
			report: num(f[10]),
		})
	}
	return cases
}

// specSite answers every request with the status the case gives, and an
// empty body.
type specSite struct {
	status int
	calls  int
}

func (s *specSite) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	s.calls++
	w.WriteHeader(s.status)
	return nil
}

func TestSpecRulesFileParses(t *testing.T) {
	rs, err := ParseRules(readSpec(t, "rules.txt"), `"spec-v1"`)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Refresh != 60*time.Second || rs.HitsURL != "https://edge.example.test/_edge/hits" || rs.CheckPath != "/_redirhub/check" || rs.PoweredBy != "RedirHub (integrated)" {
		t.Errorf("directives: refresh %v hits %q check %q powered-by %q", rs.Refresh, rs.HitsURL, rs.CheckPath, rs.PoweredBy)
	}
	if rs.NotifyURL != "https://hub.example.test/.well-known/mercure?topic=integration%2FFAKEworkspaceFILEtoken" || rs.NotifyToken != "FAKE.subscriber.jwt" {
		t.Errorf("@notify: %q %q", rs.NotifyURL, rs.NotifyToken)
	}
	promo, shop := rs.Hosts["promo.example.test"], rs.Hosts["shop.example.test"]
	if len(rs.Hosts) != 2 || promo == nil || shop == nil {
		t.Fatalf("hosts = %v", rs.Hosts)
	}
	if promo.Token != "FAKEprmo" || !promo.Report404 || shop.Token != "FAKEshop" || shop.Report404 {
		t.Errorf("@host: promo %+v shop %+v", promo, shop)
	}
	// The rule with the unknown flag x is skipped; the one with an unknown
	// option is kept.
	if _, ok := promo.Exact["/beta"]; ok {
		t.Error("a rule with an unknown flag must be skipped")
	}
	if r, ok := promo.Exact["/spring"]; !ok || r.Status != 307 {
		t.Errorf("a rule with an unknown option must be kept: %+v", r)
	}
	if r, ok := promo.Exact["/tour"]; !ok || !r.Frame || r.Status != 200 || r.Target != "https://tour.example.test/?v=1" {
		t.Errorf("a frame rule: %+v", r)
	}
	// password=, destinations=, routing= and plugins= are ignored: a plain
	// 302.
	if r, ok := promo.Exact["/vip"]; !ok || r.Frame || r.Status != 302 || r.Target != "https://promo.example.test/members" {
		t.Errorf("a rule with options this module ignores: %+v", r)
	}
	if len(promo.Exact) != 6 || promo.CatchAll != nil || len(shop.Exact) != 1 || shop.CatchAll == nil {
		t.Errorf("rules: promo %d exact, shop %d exact, catch-all %v", len(promo.Exact), len(shop.Exact), shop.CatchAll)
	}
	if r := shop.CatchAll; r.Source != "shop.example.test/*" || r.Status != 301 || !r.AppendPath || !r.KeepQuery {
		t.Errorf("catch-all = %+v", r)
	}
	if r := promo.Exact["/preise/über-uns"]; r.Source != "promo.example.test/preise/%C3%BCber-uns" {
		t.Errorf("percent-encoded source = %+v", r)
	}
}

func TestSpecRequestsAndReports(t *testing.T) {
	clock = func() time.Time { return specTime }
	t.Cleanup(func() { clock = time.Now })

	hits := newHitsServer(t)
	rules := newRulesServer(t, string(readSpec(t, "rules.txt")), `"spec-v1"`)
	f := newFetcher(rules.URL+"/abc/TOKEN.txt", nil, "")
	f.fetch(context.Background())
	if f.Rules() == nil {
		t.Fatal("spec/v1/rules.txt was not loaded")
	}
	// The file's @hits is a placeholder host; reports go to the test server.
	r := newReporter("reh_int_test", func() string { return hits.URL + "/_edge/hits" }, nil)
	r.start()
	t.Cleanup(func() { r.Destruct() })
	h := &Handler{rules: f, reporter: r, key: "reh_int_test"}

	want := strings.Split(strings.TrimSuffix(string(readSpec(t, "reports.txt")), "\n"), "\n")
	reported := 0
	for _, c := range specCases(t) {
		req := request(c.method, c.url)
		if c.referer != "" {
			req.Header.Set("Referer", c.referer)
		}
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}
		s := &specSite{status: c.site}
		w := serve(t, h, s, req)

		if w.Code != c.status || w.Header().Get("Location") != c.location {
			t.Errorf("%s: got %d %q, want %d %q", c.name, w.Code, w.Header().Get("Location"), c.status, c.location)
		}
		switch c.outcome {
		case "redirect":
			if w.Header().Get("X-Powered-By") != "RedirHub (integrated)" {
				t.Errorf("%s: X-Powered-By = %q", c.name, w.Header().Get("X-Powered-By"))
			}
		case "frame":
			checkSpecFrame(t, c, w.Header(), w.Body.String())
		case "site":
			if s.calls != 1 || w.Header().Get("X-Powered-By") != "" {
				t.Errorf("%s: the site must answer (%d calls, X-Powered-By %q)", c.name, s.calls, w.Header().Get("X-Powered-By"))
			}
		case "check":
			if s.calls != 0 || w.Body.String() != c.body {
				t.Errorf("%s: check answer %q, want %q", c.name, w.Body.String(), c.body)
			}
		default:
			t.Fatalf("%s: unknown outcome %q", c.name, c.outcome)
		}

		if c.report == 0 {
			continue
		}
		reported++
		if c.report != reported {
			t.Fatalf("%s: report column is %d, but it is report line %d", c.name, c.report, reported)
		}
		got := hits.waitLines(t, reported)
		if got[reported-1] != want[reported-1] {
			t.Errorf("%s: report line\n got %s\nwant %s", c.name, got[reported-1], want[reported-1])
		}
	}
	time.Sleep(3 * flushInterval)
	got := hits.allLines()
	if len(got) != len(want) {
		t.Fatalf("%d report lines, reports.txt has %d", len(got), len(want))
	}
	if body := strings.Join(got, "\n") + "\n"; body != string(readSpec(t, "reports.txt")) {
		t.Errorf("the reports differ from spec/v1/reports.txt:\n%s", body)
	}
}

// iframeSrc matches an <iframe> tag and its src attribute.
var iframeSrc = regexp.MustCompile(`<iframe\s[^>]*\bsrc="([^"]*)"`)

// checkSpecFrame checks a frame page: HTML with exactly one <iframe>,
// whose src, HTML-unescaped, is the body column's src=, and X-Powered-By.
func checkSpecFrame(t *testing.T, c specCase, hdr http.Header, body string) {
	t.Helper()
	src, ok := strings.CutPrefix(c.body, "src=")
	if !ok {
		t.Fatalf("%s: frame body column %q", c.name, c.body)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("%s: Content-Type = %q", c.name, ct)
	}
	if hdr.Get("X-Powered-By") != "RedirHub (integrated)" {
		t.Errorf("%s: X-Powered-By = %q", c.name, hdr.Get("X-Powered-By"))
	}
	if n := strings.Count(body, "<iframe"); n != 1 {
		t.Fatalf("%s: %d <iframe> in\n%s", c.name, n, body)
	}
	m := iframeSrc.FindStringSubmatch(body)
	if m == nil || html.UnescapeString(m[1]) != src {
		t.Errorf("%s: iframe src %q, want %q", c.name, m, src)
	}
}

// reports.expected.tsv names, for each line of reports.txt, what the edge
// logs; the module itself only sends the lines. Its columns are read here
// from the lines themselves, so the two files cannot disagree.
func TestSpecReportsExpected(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(string(readSpec(t, "reports.txt")), "\n"), "\n")
	rows := strings.Split(strings.TrimSuffix(string(readSpec(t, "reports.expected.tsv")), "\n"), "\n")
	if len(rows) != len(lines)+1 || rows[0] != "client_ip\tmethod\turl\tstatus\treferer\tuser_agent\tfile\tdestination" {
		t.Fatalf("reports.expected.tsv: %d rows for %d lines, header %q", len(rows), len(lines), rows[0])
	}
	for i, line := range lines {
		f := strings.Split(rows[i+1], "\t")
		if len(f) != 8 {
			t.Fatalf("reports.expected.tsv row %d: %d columns", i+1, len(f))
		}
		ip, method, url, status, referer, agent, file, destination := f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7]
		prefix := ip + ` - - [31/Oct/2026:19:10:00 +0000] "` + method + " " + url + ` HTTP/1.1" ` + status + ` 0 "` + referer + `" "` + agent + `"`
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			t.Errorf("line %d: %s\ndoes not start with %s", i+1, line, prefix)
			continue
		}
		if status == "404" {
			if rest != "" || file != "-" || destination != "-" {
				t.Errorf("line %d: a 404 carries no fields: %q, file %q, destination %q", i+1, rest, file, destination)
			}
			continue
		}
		source, loc, ok := strings.Cut(strings.TrimPrefix(rest, " file="), " destination=")
		if !ok || !strings.HasPrefix(rest, " file=") {
			t.Errorf("line %d: fields %q", i+1, rest)
			continue
		}
		if decoded := decodePath(strings.TrimSuffix(source, "/*")); decoded != file || loc != destination {
			t.Errorf("line %d: file %q destination %q, want %q %q", i+1, decoded, loc, file, destination)
		}
	}
}
