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
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// Report batching. Variables so tests can shorten them.
var (
	flushInterval = 5 * time.Second
	reportTimeout = 2 * time.Second
)

const (
	maxBatchLines = 100
	maxBatchBytes = 64 << 10
	queueSize     = 10000 // lines waiting to be batched; more are dropped

	dropLogInterval = 10 * time.Minute

	// maxHeaderField caps the Referer and User-Agent in a report line.
	maxHeaderField = 1024
)

// reporterPool holds one reporter per rules URL and key.
var reporterPool = caddy.NewUsagePool()

// reporter batches report lines and posts them to the @hits URL of the
// rules in use. It never blocks a request: when the queue is full, lines
// are dropped, and a failed post is dropped too.
type reporter struct {
	key    string
	hits   func() string
	client *http.Client
	logger *zap.Logger
	queue  chan string

	interval time.Duration // flush interval
	timeout  time.Duration // per post

	dropMu       sync.Mutex
	lastDropLog  time.Time
	droppedSince int

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func reporterKey(rulesURL, key string) string {
	return rulesURL + "\x00" + key
}

func acquireReporter(rulesURL, key string, f *fetcher, logger *zap.Logger) (*reporter, error) {
	val, _, err := reporterPool.LoadOrNew(reporterKey(rulesURL, key), func() (caddy.Destructor, error) {
		r := newReporter(key, func() string {
			if rs := f.Rules(); rs != nil {
				return rs.HitsURL
			}
			return ""
		}, logger)
		r.start()
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	return val.(*reporter), nil
}

func releaseReporter(rulesURL, key string) {
	_, _ = reporterPool.Delete(reporterKey(rulesURL, key))
}

func newReporter(key string, hits func() string, logger *zap.Logger) *reporter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &reporter{
		key:    key,
		hits:   hits,
		client: &http.Client{Timeout: reportTimeout},
		logger: logger,
		queue:  make(chan string, queueSize),
		done:   make(chan struct{}),

		interval: flushInterval,
		timeout:  reportTimeout,
	}
}

func (r *reporter) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.loop(ctx)
}

// Destruct sends what is queued, then stops; it implements caddy.Destructor.
func (r *reporter) Destruct() error {
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
			<-r.done
		}
	})
	return nil
}

// Enqueue adds one line without blocking; it is dropped when the queue is full.
func (r *reporter) Enqueue(line string) {
	select {
	case r.queue <- line:
	default:
	}
}

func (r *reporter) loop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	var batch bytes.Buffer
	lines := 0
	flush := func() {
		if lines > 0 {
			r.send(batch.Bytes(), lines)
		}
		batch.Reset()
		lines = 0
	}
	add := func(line string) {
		if lines > 0 && batch.Len()+len(line)+1 > maxBatchBytes {
			flush()
		}
		batch.WriteString(line)
		batch.WriteByte('\n')
		lines++
		if lines >= maxBatchLines || batch.Len() >= maxBatchBytes {
			flush()
		}
	}

	for {
		select {
		case line := <-r.queue:
			add(line)
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			// Send what is queued, but never spend more than one report
			// timeout on it: the rest is dropped.
			deadline := time.Now().Add(r.timeout)
		drain:
			for time.Now().Before(deadline) {
				select {
				case line := <-r.queue:
					add(line)
				default:
					break drain
				}
			}
			if time.Now().Before(deadline) {
				flush()
			}
			return
		}
	}
}

// send posts one gzip batch; failures are dropped.
func (r *reporter) send(body []byte, lines int) {
	url := r.hits()
	if url == "" || r.key == "" {
		return
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(body)
	_ = zw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &gz)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", "caddy-redirects/"+Version)
	resp, err := r.client.Do(req)
	if err != nil {
		r.dropped(lines, zap.Error(err))
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		r.dropped(lines, zap.Int("status", resp.StatusCode))
	}
}

// dropped logs a dropped batch at most once per dropLogInterval.
func (r *reporter) dropped(lines int, field zap.Field) {
	now := time.Now()
	r.dropMu.Lock()
	r.droppedSince += lines
	if now.Sub(r.lastDropLog) < dropLogInterval {
		r.dropMu.Unlock()
		return
	}
	n := r.droppedSince
	r.droppedSince = 0
	r.lastDropLog = now
	r.dropMu.Unlock()
	r.logger.Warn("hit reports dropped", zap.Int("lines", n), field)
}

// hitLine formats one Apache combined line with the absolute URL; a
// redirect adds the rule applied (file=, its source column verbatim) and
// the Location sent:
//
//	203.0.113.9 - - [31/Oct/2026:19:10:00 +0000] "GET https://host/path?q HTTP/1.1" 301 0 "referer" "agent" file=host/path destination=https://...
func hitLine(req *http.Request, host string, status, size int, now time.Time, source, location string) string {
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	var b strings.Builder
	b.Grow(256)
	b.WriteString(clientIP(req))
	b.WriteString(" - - [")
	b.WriteString(now.UTC().Format("02/Jan/2006:15:04:05 -0700"))
	b.WriteString(`] "`)
	b.WriteString(escape(req.Method))
	b.WriteByte(' ')
	// Percent-encoded, not backslash-escaped: a space (a raw query may
	// hold one) would split the request line.
	b.WriteString(fieldValue(scheme + "://" + host + req.URL.RequestURI()))
	b.WriteByte(' ')
	b.WriteString(escape(req.Proto))
	b.WriteString(`" `)
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(size))
	b.WriteString(` "`)
	b.WriteString(orDash(escape(truncate(req.Referer(), maxHeaderField))))
	b.WriteString(`" "`)
	b.WriteString(orDash(escape(truncate(req.UserAgent(), maxHeaderField))))
	b.WriteByte('"')
	if source != "" {
		b.WriteString(" file=")
		b.WriteString(fieldValue(source))
		b.WriteString(" destination=")
		b.WriteString(orDash(fieldValue(location)))
	}
	return b.String()
}

// fieldValue percent-encodes what cannot stand in a key=value field of
// the line: spaces, quotes, backslashes and control bytes.
func fieldValue(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c == '"' || c == '\\' || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c == '"' || c == '\\' || c == 0x7f {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// clientIP is Caddy's client IP, which honours trusted_proxies; outside
// Caddy it falls back to the connection's address.
func clientIP(req *http.Request) string {
	if ip, ok := caddyhttp.GetVar(req.Context(), caddyhttp.ClientIPVarKey).(string); ok && ip != "" {
		return escape(ip)
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if host == "" {
		return "-"
	}
	return escape(host)
}

// truncate cuts s to at most n bytes, on a UTF-8 boundary when s is valid
// UTF-8. It runs before escape, so a cut never splits an escape sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > n-utf8.UTFMax && cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if !utf8.RuneStart(s[cut]) {
		cut = n // not UTF-8: cut where the limit is
	}
	return s[:cut]
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// escape backslash-escapes " and \ and writes control bytes as \xHH, so
// a value can never break the line or its quotes.
func escape(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '"' || c == '\\' || c < 0x20 || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
