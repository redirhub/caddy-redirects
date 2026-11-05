package redirhub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func TestPermissionWithRulesURL(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"v1"`)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	p := &Permission{RulesURL: rules.URL + "/perm.txt"}
	if err := p.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	// the shared poller downloads in the background
	deadline := time.Now().Add(3 * time.Second)
	for p.rules.Rules() == nil {
		if time.Now().After(deadline) {
			t.Fatal("rules never loaded")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := p.CertificateAllowed(context.Background(), "Promo.Example.com"); err != nil {
		t.Errorf("listed host denied: %v", err)
	}
	if err := p.CertificateAllowed(context.Background(), "evil.example.com"); !errors.Is(err, ErrNotListed) {
		t.Errorf("unlisted host: %v", err)
	}

	// a handler with the same rules_url shares the permission's poller
	h := &Handler{RulesURL: rules.URL + "/perm.txt"}
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Cleanup()
	if h.rules != p.rules {
		t.Error("permission and handler must share one poller")
	}
}

func TestPermissionBeforeFirstDownload(t *testing.T) {
	p := &Permission{rules: newFetcher("http://127.0.0.1:1/never", nil, "")}
	if err := p.CertificateAllowed(context.Background(), "promo.example.com"); err == nil {
		t.Error("allowed before any rules were loaded")
	}
}

func TestPermissionWithoutRulesURL(t *testing.T) {
	rules := newRulesServer(t, sampleRules, `"v1"`)
	f := newFetcher(rules.URL, nil, "")
	f.fetch(context.Background())
	key := rules.URL + "/any"
	rulesPool.LoadOrStore(key, f)
	defer rulesPool.Delete(key)

	p := &Permission{}
	if err := p.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatal(err)
	}
	if err := p.CertificateAllowed(context.Background(), "shop.example.com"); err != nil {
		t.Errorf("listed host denied: %v", err)
	}
	if err := p.CertificateAllowed(context.Background(), "other.example.com"); err == nil {
		t.Error("unlisted host allowed")
	}
}
