package redirhub

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseFormat(t *testing.T) {
	for _, v := range []string{"1", "1.0", "1.7"} {
		if _, err := ParseRules([]byte("@format "+v+"\n@host a.example.com check=T\n"), ""); err != nil {
			t.Errorf("@format %s: %v", v, err)
		}
	}
	for _, v := range []string{"2", "0", "2.0", "x", ""} {
		if _, err := ParseRules([]byte("@format "+v+"\n@host a.example.com check=T\n"), ""); !errors.Is(err, ErrUnknownFormat) {
			t.Errorf("@format %q: err = %v", v, err)
		}
	}
}

func TestParseRefreshClamp(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"5": MinRefresh, "30": 30 * time.Second, "90": 90 * time.Second,
		"3600": time.Hour, "99999": MaxRefresh, "abc": DefaultRefresh,
	} {
		rs, err := ParseRules([]byte("@format 1\n@refresh "+in+"\n"), "")
		if err != nil || rs.Refresh != want {
			t.Errorf("@refresh %s = %v (%v), want %v", in, rs.Refresh, err, want)
		}
	}
}

func TestParseRules(t *testing.T) {
	body := "@format 1\n" +
		"@refresh 120\n" +
		"@hits https://rediredge.com/_edge/hits\n" +
		"@check /_redirhub/check\n" +
		"@future something\n" +
		"@host Promo.Example.com check=EEEXQ4RS future=yes report404\n" +
		"@host shop.example.com check=K3Vb9QmX\n" +
		"@host\n" +
		"https://promo.example.com/old-pricing\thttps://promo.example.com/pricing\r\n" +
		"https://promo.example.com/promo\thttps://sale.example.com/\t302\n" +
		"https://promo.example.com/promo\thttps://ignored.example.com/\t307\n" + // duplicate: first wins
		"https://promo.example.com/bad-status\thttps://x.example.com/\t200\n" +
		"https://promo.example.com/relative\t/pricing\n" +
		"not a rule\n" +
		"https://promo.example.com/no-target\n" +
		"https://unlisted.example.com/a\thttps://x.example.com/\n" +
		"https://shop.example.com/*\thttps://www.example.net/\t301\tpq\n" +
		"https://shop.example.com/caf%C3%A9?x=1\thttps://www.example.net/cafe\t308\tq\n" +
		"https://shop.example.com/empty-flags\thttps://www.example.net/e\t302\t\tweight=3 future=x\textra column\n" +
		"https://shop.example.com/unknown-flag\thttps://www.example.net/u\t301\tpz\n" +
		"https://shop.example.com/no-status\thttps://www.example.net/n\t\tq\n" +
		"\n"
	rs, err := ParseRules([]byte(body), `W/"abc123"`)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Refresh != 120*time.Second {
		t.Errorf("Refresh = %v", rs.Refresh)
	}

	if rs.HitsURL != "https://rediredge.com/_edge/hits" {
		t.Errorf("HitsURL = %q", rs.HitsURL)
	}
	if rs.CheckPath != "/_redirhub/check" {
		t.Errorf("CheckPath = %q", rs.CheckPath)
	}
	if rs.ETag != "abc123" {
		t.Errorf("ETag = %q", rs.ETag)
	}
	if len(rs.Hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(rs.Hosts))
	}
	promo := rs.Host("PROMO.example.com:443")
	if promo == nil || promo.Token != "EEEXQ4RS" || !promo.Report404 {
		t.Fatalf("promo = %+v", promo)
	}
	if len(promo.Exact) != 2 || promo.CatchAll != nil {
		t.Errorf("promo rules = %+v catch-all %v", promo.Exact, promo.CatchAll)
	}
	if r := promo.Exact["/old-pricing"]; r.Status != 301 || r.Target != "https://promo.example.com/pricing" {
		t.Errorf("old-pricing = %+v", r)
	}
	if r := promo.Exact["/promo"]; r.Status != 302 || r.Target != "https://sale.example.com/" {
		t.Errorf("promo = %+v", r)
	}
	shop := rs.Host("shop.example.com")
	if shop == nil || shop.Token != "K3Vb9QmX" || shop.Report404 {
		t.Fatalf("shop = %+v", shop)
	}
	if shop.CatchAll == nil || !shop.CatchAll.AppendPath || !shop.CatchAll.KeepQuery || shop.CatchAll.Status != 301 {
		t.Errorf("catch-all = %+v", shop.CatchAll)
	}
	if r, ok := shop.Exact["/café"]; !ok || r.Status != 308 || !r.KeepQuery {
		t.Errorf("decoded exact rule = %+v %v", r, ok)
	}
	if r, ok := shop.Exact["/empty-flags"]; !ok || r.Status != 302 || r.KeepQuery || r.AppendPath {
		t.Errorf("empty flags with options = %+v %v", r, ok)
	}
	if _, ok := shop.Exact["/unknown-flag"]; ok {
		t.Error("a rule with an unknown flag must be skipped")
	}
	if r, ok := shop.Exact["/no-status"]; !ok || r.Status != 301 || !r.KeepQuery {
		t.Errorf("empty status = %+v %v", r, ok)
	}
	if rs.Host("unlisted.example.com") != nil {
		t.Error("unlisted host must not be served")
	}
}

func TestParseRulesDefaults(t *testing.T) {
	rs, err := ParseRules([]byte("@host a.example.com check=T\n@hits not-a-url\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Refresh != DefaultRefresh {
		t.Errorf("Refresh = %v", rs.Refresh)
	}
	if rs.HitsURL != "" {
		t.Errorf("HitsURL = %q, want empty", rs.HitsURL)
	}
	if rs.CheckPath != DefaultCheckPath {
		t.Errorf("CheckPath = %q", rs.CheckPath)
	}
	if rs.ETag != "" {
		t.Errorf("ETag = %q", rs.ETag)
	}
}

func TestLocation(t *testing.T) {
	tests := []struct {
		name   string
		rule   Rule
		req    string
		expect string
	}{
		{"plain drops query", Rule{Target: "https://t.example/new"}, "https://a.example/old?x=1", "https://t.example/new"},
		{"q adds ?", Rule{Target: "https://t.example/new", KeepQuery: true}, "https://a.example/old?x=1&y=2", "https://t.example/new?x=1&y=2"},
		{"q adds &", Rule{Target: "https://t.example/new?a=b", KeepQuery: true}, "https://a.example/old?x=1", "https://t.example/new?a=b&x=1"},
		{"q without request query", Rule{Target: "https://t.example/new", KeepQuery: true}, "https://a.example/old", "https://t.example/new"},
		{"p no double slash", Rule{Target: "https://t.example/", AppendPath: true}, "https://a.example/blog/post", "https://t.example/blog/post"},
		{"p onto path", Rule{Target: "https://t.example/base", AppendPath: true}, "https://a.example/blog", "https://t.example/base/blog"},
		{"p root", Rule{Target: "https://t.example/", AppendPath: true}, "https://a.example/", "https://t.example/"},
		{"p keeps escaping", Rule{Target: "https://t.example/", AppendPath: true}, "https://a.example/a%20b", "https://t.example/a%20b"},
		{"pq", Rule{Target: "https://t.example/", AppendPath: true, KeepQuery: true}, "https://a.example/x?y=1", "https://t.example/x?y=1"},
		{"p with target query", Rule{Target: "https://t.example/?ref=a", AppendPath: true, KeepQuery: true}, "https://a.example/x?y=1", "https://t.example/x?ref=a&y=1"},
		{"fragment stays last", Rule{Target: "https://t.example/new#top", KeepQuery: true}, "https://a.example/old?x=1", "https://t.example/new?x=1#top"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.rule.Location(httptest.NewRequest("GET", tt.req, nil))
			if got != tt.expect {
				t.Errorf("Location = %q, want %q", got, tt.expect)
			}
		})
	}
}
