// Copyright 2026 RedirHub
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redirects

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultCheckPath is the check path used when the rules file has no
// @check directive.
const DefaultCheckPath = "/_redirhub/check"

// FormatMajor is the rules file format this module understands.
const FormatMajor = 1

// Poll interval bounds for @refresh.
const (
	DefaultRefresh = 60 * time.Second
	MinRefresh     = 30 * time.Second
	MaxRefresh     = 3600 * time.Second
)

// ErrUnknownFormat is returned for a rules file whose @format major
// version this module does not understand.
var ErrUnknownFormat = errors.New("unknown rules file format")

// RuleSet is one parsed copy of a workspace rules file. It is immutable
// once built and safe for concurrent use.
type RuleSet struct {
	// HitsURL is where redirects and 404s are reported; empty disables
	// reporting.
	HitsURL string
	// CheckPath is the path answered with the host's check token.
	CheckPath string
	// ETag is the ETag of the file without quotes or the W/ prefix.
	ETag string
	// Refresh is the poll interval from @refresh, clamped to
	// MinRefresh..MaxRefresh; DefaultRefresh when absent.
	Refresh time.Duration
	// Hosts maps a lowercase hostname to its rules.
	Hosts map[string]*Host
	// NotifyURL and NotifyToken come from @notify: the Mercure subscribe
	// URL that announces a change of this file, and the bearer token
	// allowed to subscribe to it. Empty: poll only.
	NotifyURL   string
	NotifyToken string
	// PoweredBy is the platform's name from @powered-by, sent as
	// X-Powered-By on every redirect from the rules. Empty: no header.
	PoweredBy string
}

// Host holds the rules of one hostname listed with @host.
type Host struct {
	Name      string
	Token     string
	Report404 bool
	// Exact maps a decoded path, slashes trimmed (normalizePath), to its rule.
	Exact map[string]Rule
	// CatchAll is the {host}/* rule, or nil.
	CatchAll *Rule
}

// Rule is one redirect or frame page.
type Rule struct {
	// Source is the rule's source column as written, reported back with
	// each redirect it serves.
	Source string
	Target string
	// Status is the redirect status; 200 for a frame page.
	Status int
	// Frame answers with a page that shows the Location in a full-window
	// iframe instead of redirecting (status column "frame").
	Frame      bool
	AppendPath bool
	KeepQuery  bool
}

// Host returns the rules for a hostname, or nil when the hostname is not
// listed. The hostname may carry a port and any letter case.
func (rs *RuleSet) Host(hostname string) *Host {
	if rs == nil {
		return nil
	}
	return rs.Hosts[normalizeHost(hostname)]
}

// ParseRules parses a workspace rules file (format v1). Lines that cannot
// be parsed, rules with an unknown flag letter and unknown @ directives
// are skipped. Rules for hostnames that are not listed with @host are
// dropped: only listed hosts are served.
//
// The only error is ErrUnknownFormat: an @format whose major version is
// not 1. A file without @format is read as v1.
func ParseRules(body []byte, etag string) (*RuleSet, error) {
	rs := &RuleSet{
		CheckPath: DefaultCheckPath,
		ETag:      cleanETag(etag),
		Refresh:   DefaultRefresh,
		Hosts:     make(map[string]*Host),
	}
	type pending struct {
		host string
		path string
		all  bool
		rule Rule
	}
	var rules []pending

	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "@") {
			if err := rs.parseDirective(line, strings.Fields(line)); err != nil {
				return nil, err
			}
			continue
		}
		host, path, all, rule, ok := parseRuleLine(line)
		if !ok {
			continue
		}
		rules = append(rules, pending{host: host, path: path, all: all, rule: rule})
	}

	for _, p := range rules {
		h := rs.Hosts[p.host]
		if h == nil {
			continue
		}
		if p.all {
			if h.CatchAll == nil {
				r := p.rule
				h.CatchAll = &r
			}
			continue
		}
		if _, exists := h.Exact[p.path]; !exists {
			r := p.rule
			r.AppendPath = false // p only applies to the catch-all
			h.Exact[p.path] = r
		}
	}
	return rs, nil
}

func (rs *RuleSet) parseDirective(line string, f []string) error {
	switch f[0] {
	case "@format":
		if len(f) < 2 {
			return fmt.Errorf("%w: @format without a version", ErrUnknownFormat)
		}
		major, _, _ := strings.Cut(f[1], ".")
		if n, err := strconv.Atoi(major); err != nil || n != FormatMajor {
			return fmt.Errorf("%w: %s", ErrUnknownFormat, f[1])
		}
	case "@refresh":
		if len(f) >= 2 {
			if secs, err := strconv.Atoi(f[1]); err == nil {
				rs.Refresh = min(max(time.Duration(secs)*time.Second, MinRefresh), MaxRefresh)
			}
		}
	case "@hits":
		if len(f) >= 2 {
			if u, err := url.Parse(f[1]); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
				rs.HitsURL = f[1]
			}
		}
	case "@check":
		if len(f) >= 2 && strings.HasPrefix(f[1], "/") {
			rs.CheckPath = f[1]
		}
	case "@notify":
		// @notify <subscribe url> token=<jwt>
		if len(f) >= 2 {
			if u, err := url.Parse(f[1]); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
				rs.NotifyURL = f[1]
				for _, opt := range f[2:] {
					if token, ok := strings.CutPrefix(opt, "token="); ok {
						rs.NotifyToken = token
					}
				}
			}
		}
	case "@powered-by":
		// @powered-by <value>: the rest of the line, trimmed, used
		// verbatim. Printable ASCII only, up to 128 bytes.
		_, name, _ := strings.Cut(line, " ")
		name = strings.TrimSpace(name)
		if len(name) <= 128 && validHeaderValue(name) {
			rs.PoweredBy = name
		}
	case "@host":
		if len(f) < 2 {
			return nil
		}
		name := normalizeHost(f[1])
		if name == "" || strings.ContainsAny(name, "/*") {
			return nil
		}
		h := &Host{Name: name, Exact: make(map[string]Rule)}
		for _, opt := range f[2:] {
			switch {
			case strings.HasPrefix(opt, "check="):
				h.Token = strings.TrimPrefix(opt, "check=")
			case opt == "report404":
				h.Report404 = true
			}
		}
		if _, exists := rs.Hosts[name]; !exists {
			rs.Hosts[name] = h
		}
	}
	return nil
}

// parseRuleLine parses
// source<TAB>target[<TAB>status[<TAB>flags[<TAB>options]]], the source
// a hostname and path without a scheme (host/path, host/* the catch-all).
// The status is 301, 302, 307, 308 or frame. Flags are must-understand:
// an unknown letter skips the rule. Options (space-separated key=value,
// password=, destinations=, routing=, plugins= among them) and any further
// columns are ignored: such a rule answers as if they were absent.
func parseRuleLine(line string) (host, path string, catchAll bool, rule Rule, ok bool) {
	f := strings.Split(line, "\t")
	if len(f) < 2 {
		return "", "", false, Rule{}, false
	}
	source := strings.TrimSpace(f[0])
	host, rawPath, ok := splitSource(source)
	if !ok {
		return "", "", false, Rule{}, false
	}
	target := strings.TrimSpace(f[1])
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", "", false, Rule{}, false
	}
	rule = Rule{Source: source, Target: target, Status: http.StatusMovedPermanently}
	if len(f) >= 3 && strings.TrimSpace(f[2]) == "frame" {
		rule.Status, rule.Frame = http.StatusOK, true
	} else if len(f) >= 3 && strings.TrimSpace(f[2]) != "" {
		code, err := strconv.Atoi(strings.TrimSpace(f[2]))
		if err != nil {
			return "", "", false, Rule{}, false
		}
		switch code {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			rule.Status = code
		default:
			return "", "", false, Rule{}, false
		}
	}
	if len(f) >= 4 {
		for _, c := range strings.TrimSpace(f[3]) {
			switch c {
			case 'p':
				rule.AppendPath = true
			case 'q':
				rule.KeepQuery = true
			default:
				return "", "", false, Rule{}, false
			}
		}
	}
	if rawPath == "/*" {
		return host, "", true, rule, true
	}
	return host, normalizePath(decodePath(rawPath)), false, rule, true
}

// splitSource splits host/path?query into host and path; the query is
// ignored. A source with a scheme, or without a path, is not a rule.
func splitSource(src string) (host, path string, ok bool) {
	if strings.Contains(src, "://") || strings.ContainsAny(src, " \"\\") {
		return "", "", false
	}
	rest := src
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", false
	}
	host, path = normalizeHost(rest[:i]), rest[i:]
	if host == "" {
		return "", "", false
	}
	return host, path, true
}

// normalizePath is the form a path is matched in, the edge's: leading and
// trailing slashes trimmed ("/old/" and "//old" are "/old"), the root "/".
func normalizePath(p string) string {
	return "/" + strings.Trim(p, "/")
}

func decodePath(p string) string {
	if d, err := url.PathUnescape(p); err == nil {
		return d
	}
	return p
}

// normalizeHost lowercases a hostname and strips a port and trailing dot.
func normalizeHost(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// cleanETag removes the weak prefix and quotes from an ETag.
func cleanETag(etag string) string {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	return strings.Trim(etag, `"`)
}

// Location builds the redirect target for a request, and a frame page's
// iframe src.
//
// With AppendPath the escaped request path is appended to the target's
// path without a double slash; with KeepQuery the request query is added
// with ? or &. Without KeepQuery the request query is dropped. A fragment
// in the target stays last.
func (r Rule) Location(req *http.Request) string {
	target, fragment, _ := strings.Cut(r.Target, "#")
	base, query, hasQuery := strings.Cut(target, "?")
	if r.AppendPath {
		base = strings.TrimSuffix(base, "/") + req.URL.EscapedPath()
	}
	loc := base
	if hasQuery {
		loc += "?" + query
	}
	if r.KeepQuery && req.URL.RawQuery != "" {
		if hasQuery && query != "" {
			loc += "&" + req.URL.RawQuery
		} else if hasQuery {
			loc += req.URL.RawQuery
		} else {
			loc += "?" + req.URL.RawQuery
		}
	}
	if fragment != "" || strings.Contains(r.Target, "#") {
		loc += "#" + fragment
	}
	return loc
}

// Match returns the exact rule for the request path. Both the request
// path and the source path are compared percent-decoded, without leading
// and trailing slashes, as the edge looks them up: /old/ and //old match
// the rule host/old.
func (h *Host) Match(req *http.Request) (Rule, bool) {
	r, ok := h.Exact[normalizePath(decodePath(req.URL.EscapedPath()))]
	return r, ok
}

// validHeaderValue reports whether s is printable ASCII, safe in a header.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
