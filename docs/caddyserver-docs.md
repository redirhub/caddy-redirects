---
title: redirects (Caddyfile directive)
---

# redirects

Bulk redirects and 404 monitoring. Serves the redirects of a [RedirHub](https://www.redirhub.com) workspace ("Integration Mode"):
redirects, branded short links, QR code links and website migration mappings managed outside
Caddy, in the RedirHub dashboard, API or MCP server. Caddy downloads the workspace rules file
every 60 seconds and serves the redirects itself; changes need no config edit or reload. When the
rules file names a push hub (`@notify`), Caddy keeps one subscription open and downloads a change
within a second or two, polling only hourly while it is connected and every 60 seconds again when
it is not.

For `GET` and `HEAD` requests on a hostname listed in the rules:

- the check path `/_redirhub/check` is answered with the hostname's check token, which proves
  ownership of the hostname to RedirHub;
- a rule for the exact hostname and path (any scheme) redirects before the rest of the route;
- a host's catch-all rule redirects only when the rest of the route answers `404`;
- with `@powered-by <value>` in the rules file, those redirects carry `X-Powered-By: <value>`.

All other requests pass through unchanged. If the rules cannot be downloaded, the last good copy
stays in use; before the first copy, nothing is redirected. Redirects, and optionally the site's
own `404`s, are reported to RedirHub in the background, in batches, without delaying responses;
a redirect's report names the rule applied (`file=`, the rule's source column, verbatim) and the
`Location` sent.

This directive is not standard; build Caddy with it:

```
xcaddy build --with github.com/redirhub/caddy-redirects
```

Module: [`http.handlers.redirects`](https://github.com/redirhub/caddy-redirects)

## Syntax

```caddy-d
redirects [<rules_url>] {
	rules_url  <url>
	key        <integration_key>
	report_404 rules|all
}
```

- **rules_url** is the **Workspace rules address** from the RedirHub dashboard (Integration
  key panel). Required, inline or in the block.
- **key** is the workspace integration key, used only to report redirects and 404s. Without it,
  nothing is reported. Use a placeholder such as `{env.REDIRHUB_INTEGRATION_KEY}`.
- **report_404** chooses which of the site's `404` answers are reported: `rules` (default) for
  the hostnames whose 404 reporting is on in RedirHub, `all` for every hostname the site block
  serves, listed in the rules or not (404 monitoring without adding each domain). Hostnames that
  are not in the rules are never redirected and get no check path.

The directive is ordered before [`redir`](/docs/caddyfile/directives/redir) by default. Handlers
with the same `rules_url` share one download, across sites and config reloads.

## On-demand TLS

The module also provides the `tls.permission.redirects` permission module, which allows an
on-demand certificate only for hostnames listed in the RedirHub rules:

```caddy
{
	on_demand_tls {
		permission redirects [<rules_url>]
	}
}
```

Without `rules_url`, a hostname listed in any rules file loaded by a `redirects` handler is allowed.

## Examples

Serve the workspace's redirects in front of an application, with certificates on demand for
every hostname in the rules:

```caddy
{
	on_demand_tls {
		permission redirects
	}
}

https:// {
	tls {
		on_demand
	}
	redirects {
		rules_url https://rules.rediredge.com/abc123/TOKEN.txt
		key {env.REDIRHUB_INTEGRATION_KEY}
	}
	reverse_proxy localhost:8080
}
```

Serve them on a static site; a missing file falls through to the host's catch-all rule, and the
404s of every hostname are reported:

```caddy
example.com {
	redirects https://rules.rediredge.com/abc123/TOKEN.txt {
		key {env.REDIRHUB_INTEGRATION_KEY}
		report_404 all
	}
	root * /srv
	file_server
}
```
