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

// Package redirects is a Caddy module for bulk redirects and 404 monitoring:
// it serves the redirects of a remote rules file (format v1, published by
// RedirHub Integration Mode), answers the ownership check path, reports
// redirects and 404s back, and can gate on-demand TLS to the listed hostnames.
package redirects

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// Version is sent in the User-Agent of rules downloads and reports.
const Version = "0.3.2"

func init() {
	caddy.RegisterModule(Handler{})
	caddy.RegisterModule(Permission{})
}

// Handler serves the redirects of a RedirHub workspace rules file.
//
// For GET and HEAD requests on a hostname listed in the rules file it
// answers the check path, redirects exact sources (host/path) before the rest of
// the route runs and, when the site itself answers 404, redirects to the
// host's catch-all rule. Every other request passes through untouched.
type Handler struct {
	// The workspace rules address from the RedirHub dashboard
	// (Integration key panel, "Workspace rules address").
	RulesURL string `json:"rules_url,omitempty"`

	// The workspace integration key, used only to report redirects and
	// 404s. Without it nothing is reported.
	Key string `json:"key,omitempty"`

	// Which 404 answers of the site are reported: "rules" (default) for
	// the hostnames whose @host line has report404, "all" for every
	// hostname this handler serves, listed in the rules or not.
	Report404 string `json:"report_404,omitempty"`

	rulesURL string
	key      string
	rules    *fetcher
	reporter *reporter
	logger   *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.redirects",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up the shared rules poller and reporter.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	repl := caddy.NewReplacer()
	h.rulesURL = repl.ReplaceAll(h.RulesURL, "")
	h.key = repl.ReplaceAll(h.Key, "")
	// Checked here, before the shared poller starts: Caddy calls Validate
	// only after Provision.
	if err := validateReport404(h.Report404); err != nil {
		return err
	}
	if err := validateRulesURL(h.rulesURL); err != nil {
		return err
	}

	f, err := acquireFetcher(h.rulesURL, h.logger)
	if err != nil {
		return err
	}
	h.rules = f
	r, err := acquireReporter(h.rulesURL, h.key, f, h.logger)
	if err != nil {
		releaseFetcher(h.rulesURL)
		h.rules = nil
		return err
	}
	h.reporter = r
	return nil
}

func validateReport404(value string) error {
	switch value {
	case "", Report404Rules, Report404All:
		return nil
	}
	return fmt.Errorf("report_404 must be %q or %q, not %q", Report404Rules, Report404All, value)
}

// The values of report_404.
const (
	Report404Rules = "rules"
	Report404All   = "all"
)

func validateRulesURL(raw string) error {
	if raw == "" {
		return errors.New("rules_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("rules_url: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("rules_url must be an absolute http(s) URL")
	}
	return nil
}

// Cleanup releases the shared poller and reporter.
func (h *Handler) Cleanup() error {
	if h.reporter != nil {
		releaseReporter(h.rulesURL, h.key)
	}
	if h.rules != nil {
		releaseFetcher(h.rulesURL)
	}
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return next.ServeHTTP(w, r)
	}
	var rs *RuleSet
	if h.rules != nil {
		rs = h.rules.Rules()
	}
	host := rs.Host(r.Host)
	listed := host != nil
	if !listed {
		if h.Report404 != Report404All || rs == nil {
			return next.ServeHTTP(w, r)
		}
		// report_404 all: an unlisted hostname has no rules and no check
		// path; only its 404s are reported.
		host = &Host{Name: normalizeHost(r.Host)}
		if host.Name == "" {
			return next.ServeHTTP(w, r)
		}
	}
	report404 := host.Report404 || h.Report404 == Report404All

	if listed && r.URL.Path == rs.CheckPath {
		body := "token=" + host.Token + " etag=" + rs.ETag
		hdr := w.Header()
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Cache-Control", "no-store")
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
		return nil
	}

	if rule, ok := host.Match(r); ok {
		h.redirect(w, r, rs, host, rule)
		return nil
	}

	if host.CatchAll == nil && !report404 {
		return next.ServeHTTP(w, r)
	}

	// Let the site answer; a 404 that the catch-all replaces is discarded
	// as it is written, never buffered. Everything else streams through.
	catchAll := host.CatchAll
	saved := w.Header().Clone()
	rec := &notFoundRecorder{ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w}, discard404: catchAll != nil}
	err := next.ServeHTTP(rec, r)

	if err != nil {
		// A handler such as file_server reports a missing file as an error
		// that the error routes turn into the 404 page.
		var he caddyhttp.HandlerError
		if errors.As(err, &he) && he.StatusCode == http.StatusNotFound && rec.Status() == 0 {
			if catchAll != nil {
				restoreHeader(w.Header(), saved)
				h.redirect(w, r, rs, host, *catchAll)
				return nil
			}
			if report404 {
				h.report(r, host, http.StatusNotFound, 0, "", "")
			}
		}
		return err
	}

	if rec.discarded {
		restoreHeader(w.Header(), saved)
		h.redirect(w, r, rs, host, *catchAll)
		return nil
	}
	if rec.Status() == http.StatusNotFound && report404 {
		h.report(r, host, http.StatusNotFound, rec.Size(), "", "")
	}
	return nil
}

// redirect answers a rule; with @powered-by in the rules file it names the
// platform in X-Powered-By, as the platform's own edge does.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, rs *RuleSet, host *Host, rule Rule) {
	location := rule.Location(r)
	w.Header().Set("Location", location)
	if rs.PoweredBy != "" {
		w.Header().Set("X-Powered-By", rs.PoweredBy)
	}
	w.WriteHeader(rule.Status)
	h.report(r, host, rule.Status, 0, rule.Source, location)
}

// report queues one report line; a redirect carries the rule's source and
// the Location it sent, a 404 neither.
func (h *Handler) report(r *http.Request, host *Host, status, size int, source, location string) {
	if h.reporter == nil || h.key == "" {
		return
	}
	h.reporter.Enqueue(hitLine(r, host.Name, status, size, clock(), source, location))
}

// clock is the time a report line is stamped with; tests fix it.
var clock = time.Now

// restoreHeader makes dst equal to saved, dropping what the discarded
// 404 response set.
func restoreHeader(dst, saved http.Header) {
	for k := range dst {
		delete(dst, k)
	}
	for k, v := range saved {
		dst[k] = v
	}
}

// notFoundRecorder passes the site's response through and records its
// status and size; with discard404 a 404 is not sent but discarded as it is
// written, so its body costs no memory however large it is.
type notFoundRecorder struct {
	*caddyhttp.ResponseWriterWrapper
	discard404 bool

	status      int
	size        int
	wroteHeader bool
	discarded   bool // the 404 was discarded: nothing was sent
}

func (rec *notFoundRecorder) WriteHeader(status int) {
	if rec.wroteHeader {
		return
	}
	rec.status = status
	if status >= 100 && status <= 199 {
		// informational, not the final answer: passed on
		rec.ResponseWriterWrapper.WriteHeader(status)
		return
	}
	rec.wroteHeader = true
	if rec.discard404 && status == http.StatusNotFound {
		rec.discarded = true
		return
	}
	rec.ResponseWriterWrapper.WriteHeader(status)
}

func (rec *notFoundRecorder) Write(p []byte) (int, error) {
	rec.WriteHeader(http.StatusOK)
	if rec.discarded {
		rec.size += len(p)
		return len(p), nil
	}
	n, err := rec.ResponseWriterWrapper.Write(p)
	rec.size += n
	return n, err
}

func (rec *notFoundRecorder) ReadFrom(r io.Reader) (int64, error) {
	rec.WriteHeader(http.StatusOK)
	var n int64
	var err error
	if rec.discarded {
		n, err = io.Copy(io.Discard, r)
	} else {
		n, err = rec.ResponseWriterWrapper.ReadFrom(r)
	}
	rec.size += int(n)
	return n, err
}

// FlushError flushes what is sent; a discarded 404 has nothing to flush.
func (rec *notFoundRecorder) FlushError() error {
	if rec.discarded {
		return nil
	}
	//nolint:bodyclose
	return http.NewResponseController(rec.ResponseWriterWrapper).Flush()
}

// Status is the status the site wrote, 0 for none.
func (rec *notFoundRecorder) Status() int { return rec.status }

// Size is the number of body bytes the site wrote.
func (rec *notFoundRecorder) Size() int { return rec.size }

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
