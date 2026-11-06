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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// Fetch timing. Variables so tests can shorten them.
var (
	pollOverride  time.Duration      // tests only: replaces @refresh when set
	retryInterval = 10 * time.Second // until the first good copy
	// fetchTimeout bounds connecting, the TLS handshake and the wait for
	// the response headers; fetchBodyTimeout the whole download, so a
	// large file still arrives over a slow link.
	fetchTimeout     = 5 * time.Second
	fetchBodyTimeout = 60 * time.Second
)

// saveWarnInterval throttles the warning for a persisted copy that cannot
// be saved: it repeats on every changed download.
const saveWarnInterval = time.Hour

// maxRulesSize bounds the rules file; a larger body counts as a failure.
const maxRulesSize = 32 << 20

// rulesPool holds one fetcher per rules URL, shared by every handler and
// permission module that names it, across config reloads.
var rulesPool = caddy.NewUsagePool()

// fetcher polls one rules URL and holds the last good copy.
type fetcher struct {
	url     string
	client  *http.Client
	logger  *zap.Logger
	file    string // persisted last good copy; empty disables it
	current atomic.Pointer[RuleSet]

	etag    string // raw ETag sent as If-None-Match; only the poll loop uses it
	failing bool
	// lastSaveWarn is when a failed save of the persisted copy was last
	// logged; only the poll loop uses it.
	lastSaveWarn time.Time

	// notify is the push subscription of the rules' @notify, or nil; only
	// the poll loop changes it.
	notify atomic.Pointer[notifier]

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// acquireFetcher returns the shared fetcher for url, starting it when it
// is new. Every call must be paired with releaseFetcher.
func acquireFetcher(url string, logger *zap.Logger) (*fetcher, error) {
	val, _, err := rulesPool.LoadOrNew(url, func() (caddy.Destructor, error) {
		f := newFetcher(url, logger, persistPath(url))
		f.start()
		return f, nil
	})
	if err != nil {
		return nil, err
	}
	return val.(*fetcher), nil
}

func releaseFetcher(url string) {
	_, _ = rulesPool.Delete(url)
}

func newFetcher(url string, logger *zap.Logger, file string) *fetcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	f := &fetcher{
		url:    url,
		client: newFetchClient(),
		logger: logger,
		file:   file,
		done:   make(chan struct{}),
	}
	f.loadPersisted()
	return f
}

// newFetchClient waits fetchTimeout for a server to connect and answer, and
// fetchBodyTimeout for the whole body.
func newFetchClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: fetchTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = fetchTimeout
	transport.ResponseHeaderTimeout = fetchTimeout
	return &http.Client{Transport: transport, Timeout: fetchBodyTimeout}
}

// Rules returns the rules in use, or nil before the first good copy.
func (f *fetcher) Rules() *RuleSet {
	return f.current.Load()
}

func (f *fetcher) start() {
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go f.loop(ctx)
}

// Destruct stops polling; it implements caddy.Destructor.
func (f *fetcher) Destruct() error {
	f.once.Do(func() {
		if f.cancel != nil {
			f.cancel()
			<-f.done
		}
	})
	return nil
}

// loop downloads the rules, then waits for the next poll or for a push
// message, whichever comes first. While a @notify subscription is connected
// the poll is stretched to notifiedPoll; the moment it drops, the wait is
// measured against @refresh again.
func (f *fetcher) loop(ctx context.Context) {
	defer close(f.done)
	defer func() { f.notify.Load().stop() }()
	for {
		f.fetch(ctx)
		f.syncNotifier(ctx)
		last := time.Now()
		for waiting := true; waiting; {
			n := f.notify.Load()
			var wake, changed chan struct{}
			if n != nil {
				wake, changed = n.wake, n.changed
			}
			t := time.NewTimer(max(time.Until(last.Add(f.pollInterval())), 0))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				waiting = false
			case <-wake:
				t.Stop()
				waiting = false
			case <-changed:
				t.Stop() // connected or dropped: measure the wait again
			}
		}
	}
}

// pollInterval is how long after a download the next one is due.
func (f *fetcher) pollInterval() time.Duration {
	rs := f.Rules()
	switch {
	case rs != nil && f.notify.Load().Connected():
		return notifiedPoll
	case pollOverride > 0:
		return pollOverride
	case rs == nil:
		return retryInterval
	}
	return rs.Refresh
}

// syncNotifier keeps one subscription for the @notify of the rules in use:
// started when the directive appears, replaced when it changes (a new
// token), stopped when it goes.
func (f *fetcher) syncNotifier(ctx context.Context) {
	var notifyURL, token string
	if rs := f.Rules(); rs != nil {
		notifyURL, token = rs.NotifyURL, rs.NotifyToken
	}
	current := f.notify.Load()
	if notifyURL == "" {
		if current != nil {
			current.stop()
			f.notify.Store(nil)
		}
		return
	}
	if current.same(notifyURL, token) {
		return
	}
	current.stop()
	n := newNotifier(notifyURL, token, f.logger)
	f.notify.Store(n)
	n.start(ctx)
}

// fetch downloads the rules once. Any failure keeps the last good copy.
func (f *fetcher) fetch(ctx context.Context) {
	if err := f.fetchOnce(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		if !f.failing {
			f.logger.Warn("rules download failed; keeping the last good copy",
				zap.String("rules_url", redactURL(f.url)),
				zap.Bool("have_rules", f.Rules() != nil),
				zap.Error(err))
		}
		f.failing = true
		return
	}
	if f.failing {
		f.logger.Info("rules download recovered", zap.String("rules_url", redactURL(f.url)))
	}
	f.failing = false
}

func (f *fetcher) fetchOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "caddy-redirects/"+Version)
	if f.etag != "" && f.Rules() != nil {
		req.Header.Set("If-None-Match", f.etag)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		// The client's error quotes the URL, whose path holds the file's token.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = redactURL(ue.URL)
		}
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if f.Rules() == nil {
			return errors.New("304 without a copy")
		}
		return nil
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRulesSize+1))
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxRulesSize {
		return errors.New("rules file too large")
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return errors.New("rules file is not text")
	}
	etag := resp.Header.Get("ETag")
	rs, err := ParseRules(body, etag)
	if err != nil {
		return err
	}
	f.current.Store(rs)
	f.etag = etag
	f.persist(etag, body)
	return nil
}

// persistPath is where the last good copy of url is kept in Caddy's data
// directory, so a restart can redirect before the first download.
func persistPath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(caddy.AppDataDir(), "redirects", hex.EncodeToString(sum[:12])+".rules")
}

// The persisted file is the raw ETag on the first line, then the body.
func (f *fetcher) persist(etag string, body []byte) {
	if f.file == "" {
		return
	}
	if err := writeFileAtomic(f.file, append([]byte(strings.ReplaceAll(etag, "\n", "")+"\n"), body...)); err != nil {
		if time.Since(f.lastSaveWarn) >= saveWarnInterval {
			f.lastSaveWarn = time.Now()
			f.logger.Warn("could not save the rules copy; a restart cannot redirect until the first download",
				zap.String("file", f.file), zap.Error(err))
		}
	}
}

func (f *fetcher) loadPersisted() {
	if f.file == "" {
		return
	}
	data, err := os.ReadFile(f.file)
	if err != nil {
		return
	}
	etag, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return
	}
	rs, err := ParseRules(body, string(etag))
	if err != nil {
		return
	}
	f.etag = string(etag)
	f.current.Store(rs)
}

func writeFileAtomic(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".rules-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// redactURL keeps the rules URL's host and hides its path, which holds
// the file's token.
func redactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "(invalid)"
	}
	host, _, _ := strings.Cut(rest, "/")
	return scheme + "://" + host + "/…"
}
