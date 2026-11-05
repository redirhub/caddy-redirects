---
title: redirhub (Caddyfile directive)
---

# redirhub

Serves the redirects of a [RedirHub](https://www.redirhub.com) workspace ("Integration Mode"):
redirects, branded short links, QR code links and website migration mappings managed outside
Caddy, in the RedirHub dashboard, API or MCP server. Caddy downloads the workspace rules file
every 60 seconds and serves the redirects itself; changes need no config edit or reload.

For `GET` and `HEAD` requests on a hostname listed in the rules:

- the check path `/_redirhub/check` is answered with the hostname's check token, which proves
  ownership of the hostname to RedirHub;
- a rule for the exact URL redirects before the rest of the route;
- a host's catch-all rule redirects only when the rest of the route answers `404`.

All other requests pass through unchanged. If the rules cannot be downloaded, the last good copy
stays in use; before the first copy, nothing is redirected. Redirects, and optionally the site's
own `404`s, are reported to RedirHub in the background, in batches, without delaying responses.

This directive is not standard; build Caddy with it:

```
xcaddy build --with github.com/redirhub/caddy-redirhub
```

Module: [`http.handlers.redirhub`](https://github.com/redirhub/caddy-redirhub)

## Syntax

```caddy-d
redirhub [<rules_url>] {
	rules_url <url>
	key       <integration_key>
}
```

- **rules_url** is the **Workspace rules address** from the RedirHub dashboard (Integration
  key panel). Required, inline or in the block.
- **key** is the workspace integration key, used only to report redirects and 404s. Without it,
  nothing is reported. Use a placeholder such as `{env.REDIRHUB_INTEGRATION_KEY}`.

The directive is ordered before [`redir`](/docs/caddyfile/directives/redir) by default. Handlers
with the same `rules_url` share one download, across sites and config reloads.

## On-demand TLS

The module also provides the `tls.permission.redirhub` permission module, which allows an
on-demand certificate only for hostnames listed in the RedirHub rules:

```caddy
{
	on_demand_tls {
		permission redirhub [<rules_url>]
	}
}
```

Without `rules_url`, a hostname listed in any rules file loaded by a `redirhub` handler is allowed.

## Examples

Serve the workspace's redirects in front of an application, with certificates on demand for
every hostname in the rules:

```caddy
{
	on_demand_tls {
		permission redirhub
	}
}

https:// {
	tls {
		on_demand
	}
	redirhub {
		rules_url https://rules.rediredge.com/abc123/TOKEN.txt
		key {env.REDIRHUB_INTEGRATION_KEY}
	}
	reverse_proxy localhost:8080
}
```

Serve them on a static site; a missing file falls through to the host's catch-all rule:

```caddy
example.com {
	redirhub https://rules.rediredge.com/abc123/TOKEN.txt {
		key {env.REDIRHUB_INTEGRATION_KEY}
	}
	root * /srv
	file_server
}
```
