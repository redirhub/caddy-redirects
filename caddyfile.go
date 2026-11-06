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
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("redirects", parseCaddyfile)
	// Exact redirects run before redir and the site's own routes, so no
	// global `order` option is needed.
	httpcaddyfile.RegisterDirectiveOrder("redirects", httpcaddyfile.Before, "redir")
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(Handler)
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens:
//
//	redirects [<rules_url>] {
//		rules_url  <url>
//		key        <integration key>
//		report_404 rules|all
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	if d.NextArg() {
		h.RulesURL = d.Val()
	}
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "rules_url":
			if !d.AllArgs(&h.RulesURL) {
				return d.ArgErr()
			}
		case "key":
			if !d.AllArgs(&h.Key) {
				return d.ArgErr()
			}
		case "report_404":
			if !d.AllArgs(&h.Report404) {
				return d.ArgErr()
			}
			if h.Report404 != Report404Rules && h.Report404 != Report404All {
				return d.Errf("report_404 must be '%s' or '%s'", Report404Rules, Report404All)
			}
		default:
			return d.Errf("unrecognized redirects option '%s'", d.Val())
		}
	}
	if h.RulesURL == "" {
		return d.Err("redirects: rules_url is required")
	}
	return nil
}

// UnmarshalCaddyfile sets up the permission module:
//
//	permission redirects [<rules_url>]
//
// or
//
//	permission redirects {
//		rules_url <url>
//	}
func (p *Permission) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // module name
	if d.NextArg() {
		p.RulesURL = d.Val()
	}
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "rules_url":
			if !d.AllArgs(&p.RulesURL) {
				return d.ArgErr()
			}
		default:
			return d.Errf("unrecognized redirects permission option '%s'", d.Val())
		}
	}
	return nil
}

// Interface guards
var (
	_ caddyfile.Unmarshaler = (*Handler)(nil)
	_ caddyfile.Unmarshaler = (*Permission)(nil)
)
