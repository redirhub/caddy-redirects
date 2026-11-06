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
	"context"
	"errors"
	"fmt"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
)

// ErrNotListed is returned for a hostname that no loaded rules file lists.
var ErrNotListed = errors.New("hostname is not listed in the RedirHub rules")

// Permission allows an on-demand TLS certificate only for hostnames listed
// with @host in a RedirHub rules file. It reads the same copy of the rules
// as the redirects handler.
//
// With rules_url it checks that file (and polls it itself if no handler
// does). Without it, a hostname listed in any rules file that a redirects
// handler of this Caddy instance has loaded is allowed.
type Permission struct {
	// The workspace rules address; optional.
	RulesURL string `json:"rules_url,omitempty"`

	rulesURL string
	rules    *fetcher
}

// CaddyModule returns the Caddy module information.
func (Permission) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "tls.permission.redirects",
		New: func() caddy.Module { return new(Permission) },
	}
}

// Provision sets up the module.
func (p *Permission) Provision(ctx caddy.Context) error {
	p.rulesURL = caddy.NewReplacer().ReplaceAll(p.RulesURL, "")
	if p.rulesURL == "" {
		return nil
	}
	if err := validateRulesURL(p.rulesURL); err != nil {
		return err
	}
	f, err := acquireFetcher(p.rulesURL, ctx.Logger())
	if err != nil {
		return err
	}
	p.rules = f
	return nil
}

// Cleanup releases the shared poller.
func (p *Permission) Cleanup() error {
	if p.rules != nil {
		releaseFetcher(p.rulesURL)
	}
	return nil
}

// CertificateAllowed implements caddytls.OnDemandPermission.
func (p *Permission) CertificateAllowed(_ context.Context, name string) error {
	if p.rules != nil {
		if p.rules.Rules().Host(name) != nil {
			return nil
		}
		return fmt.Errorf("%s: %w", name, ErrNotListed)
	}
	allowed := false
	rulesPool.Range(func(_, value any) bool {
		if f, ok := value.(*fetcher); ok && f.Rules().Host(name) != nil {
			allowed = true
			return false
		}
		return true
	})
	if allowed {
		return nil
	}
	return fmt.Errorf("%s: %w", name, ErrNotListed)
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Permission)(nil)
	_ caddy.CleanerUpper          = (*Permission)(nil)
	_ caddytls.OnDemandPermission = (*Permission)(nil)
)
