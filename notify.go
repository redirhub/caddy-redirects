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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Push timing. Variables so tests can shorten them.
var (
	// notifyIdleTimeout ends a connection that sent nothing; the hub sends a
	// heartbeat every 15 seconds.
	notifyIdleTimeout = 60 * time.Second
	// notifyMinBackoff doubles to notifyMaxBackoff between attempts and
	// starts over after a connection that lived longer than notifyStableAfter.
	notifyMinBackoff  = time.Second
	notifyMaxBackoff  = 30 * time.Second
	notifyStableAfter = time.Minute
	// notifiedPoll is the poll interval while the subscription is connected:
	// a change arrives as a message, the poll is only the safety net.
	notifiedPoll = time.Hour
)

// maxEventLine bounds one line of the stream; the messages are tiny.
const maxEventLine = 1 << 20

// notifier holds one Mercure subscription (server-sent events) for a rules
// file's @notify directive and wakes the fetcher when the file changed, so
// a change is served within a second or two instead of at the next poll.
// A message is only a wake-up: nothing in it is trusted, the fetcher still
// downloads the file. Polling stays the fallback while the subscription is
// down. Ported from RedirHub's edge-sync notifier.
type notifier struct {
	url    string // the full subscribe URL (…/.well-known/mercure?topic=…)
	token  string // the subscriber token; never logged
	client *http.Client
	logger *zap.Logger

	wake    chan struct{} // at most one pending "download now"
	changed chan struct{} // at most one pending "the connection state changed"

	mu        sync.Mutex
	connected bool
	lastID    string

	// An outage is logged once: Warn at its first failed or dropped
	// connection (down), Info at the first connection after it (upLogged).
	// Attempts in between, and connections that drop again before they
	// held for notifyStableAfter, log at Debug. Only run and connect use
	// these.
	down     bool
	upLogged bool

	cancel context.CancelFunc
	done   chan struct{}
}

func newNotifier(rawURL, token string, logger *zap.Logger) *notifier {
	return &notifier{
		url:   rawURL,
		token: token,
		// No overall timeout: the stream stays open; the idle timer ends it.
		client:  &http.Client{},
		logger:  logger,
		wake:    make(chan struct{}, 1),
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// same reports whether n subscribes with these settings.
func (n *notifier) same(rawURL, token string) bool {
	return n != nil && n.url == rawURL && n.token == token
}

func (n *notifier) start(ctx context.Context) {
	ctx, n.cancel = context.WithCancel(ctx)
	go func() {
		defer close(n.done)
		n.run(ctx)
	}()
}

// stop ends the subscription and waits for it.
func (n *notifier) stop() {
	if n == nil || n.cancel == nil {
		return
	}
	n.cancel()
	<-n.done
}

// Connected reports whether the subscription is open and healthy (it sent
// something within the idle timeout).
func (n *notifier) Connected() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connected
}

func pulse(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (n *notifier) setConnected(connected bool) {
	n.mu.Lock()
	was := n.connected
	n.connected = connected
	n.mu.Unlock()
	if was != connected {
		pulse(n.changed)
	}
}

// where names the hub in logs: never the token, nor the query, whose topic
// holds the rules file's token.
func (n *notifier) where() string {
	parsed, err := url.Parse(n.url)
	if err != nil {
		return "(invalid url)"
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}

// run keeps the subscription open until ctx ends, reconnecting with backoff.
func (n *notifier) run(ctx context.Context) {
	delay := notifyMinBackoff
	for {
		started := time.Now()
		err := n.connect(ctx)
		n.setConnected(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > notifyStableAfter {
			// a connection that held ended an outage: the next drop is news
			delay = notifyMinBackoff
			n.down = false
		}
		if !n.down {
			n.down, n.upLogged = true, false
			n.logger.Warn("rules notify disconnected; polling every @refresh until it is back",
				zap.String("hub", n.where()), zap.String("error", errString(err)), zap.Duration("retry_in", delay))
		} else {
			n.logger.Debug("rules notify still disconnected",
				zap.String("hub", n.where()), zap.String("error", errString(err)), zap.Duration("retry_in", delay))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, notifyMaxBackoff)
	}
}

func errString(err error) string {
	if err == nil {
		return "stream ended"
	}
	return err.Error()
}

// connect reads one connection until it ends. Every connection wakes the
// fetcher once it is open: a change may have come before it (between the
// download that named the hub and the first connection, or while
// reconnecting).
func (n *notifier) connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The idle timer covers the wait for the response too, and restarts on
	// every byte read.
	timer := time.AfterFunc(notifyIdleTimeout, cancel)
	defer timer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.url, nil)
	if err != nil {
		return errors.New("invalid subscribe url")
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "caddy-redirects/"+Version)
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	n.mu.Lock()
	if n.lastID != "" {
		req.Header.Set("Last-Event-ID", n.lastID)
	}
	n.mu.Unlock()

	resp, err := n.client.Do(req)
	if err != nil {
		// The client's error quotes the URL, whose query holds the topic.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = n.where()
		}
		return n.idleError(ctx, timer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub answered %s", resp.Status)
	}
	timer.Reset(notifyIdleTimeout)
	n.setConnected(true)
	switch {
	case n.down && !n.upLogged:
		n.upLogged = true
		n.logger.Info("rules notify reconnected", zap.String("hub", n.where()))
	case !n.down && !n.upLogged:
		n.upLogged = true
		n.logger.Info("rules notify connected", zap.String("hub", n.where()))
	default:
		n.logger.Debug("rules notify connected", zap.String("hub", n.where()))
	}
	pulse(n.wake)

	scanner := bufio.NewScanner(&idleReader{body: resp.Body, timer: timer, idle: notifyIdleTimeout})
	scanner.Buffer(make([]byte, 0, 4096), maxEventLine)
	var id string
	hasData := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if hasData {
				n.event(id)
			}
			hasData = false
			id = ""
			continue
		case strings.HasPrefix(line, ":"):
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			hasData = true
		case "id":
			id = value
		}
	}
	return n.idleError(ctx, timer, scanner.Err())
}

// idleError names a connection the idle timer ended.
func (n *notifier) idleError(ctx context.Context, timer *time.Timer, err error) error {
	if err != nil && ctx.Err() != nil && !timer.Stop() {
		return fmt.Errorf("no data for %s", notifyIdleTimeout)
	}
	return err
}

// event handles one complete event that carried data: whatever it says,
// the fetcher downloads the file (a conditional GET).
func (n *notifier) event(id string) {
	if id != "" {
		n.mu.Lock()
		n.lastID = id
		n.mu.Unlock()
	}
	pulse(n.wake)
}

// idleReader restarts the idle timer on every read that returned bytes.
type idleReader struct {
	body  io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	read, err := r.body.Read(p)
	if read > 0 {
		r.timer.Reset(r.idle)
	}
	return read, err
}
