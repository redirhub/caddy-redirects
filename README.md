# caddy-redirects

**Bulk redirects and 404 monitoring for [Caddy](https://caddyserver.com) v2.** Your domains keep
pointing at your own Caddy server; Caddy serves thousands of redirects from a remote rules file
and reports every redirect and every 404 back, so broken links show up before your visitors find
them.

- Redirects, short links, QR code links and whole website migrations, live within a second or two
  (a push from the rules server, `@notify`) or a minute (polling), without editing a Caddyfile or
  reloading Caddy.
- 404 monitoring: the site's own 404s that no rule redirected are reported in the background, in
  batches, without delaying responses.
- The last good copy of the rules is kept on disk, so a restart redirects at once even when the
  rules server is out of reach.
- On-demand HTTPS only for the hostnames in your rules.

The rules file format is open:
[Integration Mode format v1](https://github.com/redirhub/integration-spec).
[RedirHub](https://www.redirhub.com) publishes it for you ("Integration Mode"): you manage the redirects in its dashboard, API or MCP
server and see the redirects and 404s in its analytics.

The module adds two Caddy modules:

| Module | What it does |
| --- | --- |
| `http.handlers.redirects` | HTTP handler: answers the ownership check, serves redirects, reports hits |
| `tls.permission.redirects` | On-demand TLS permission: certificates only for hostnames in your rules |

Caddy's own [`redir`](https://caddyserver.com/docs/caddyfile/directives/redir) and
[`map`](https://caddyserver.com/docs/caddyfile/directives/map) remain the right tool for a few
static rules. This module is for redirect sets that change, many domains or many Caddy servers,
migrations, and people who edit redirects without touching Caddy.

## Install

Build Caddy with the module using [xcaddy](https://github.com/caddyserver/xcaddy):

```sh
xcaddy build --with github.com/redirhub/caddy-redirects
```

Check that both modules are in the binary:

```sh
./caddy list-modules | grep redirects
# http.handlers.redirects
# tls.permission.redirects
```

## Get the rules address and the key

1. In the RedirHub dashboard, add each domain Caddy serves and pick **Connect through your own
   platform** for it (no DNS records are needed).
2. The **Integration key** panel shows the workspace's integration key and the **Workspace rules
   address**: one URL for all integration hostnames of the workspace, made for a server such as
   Caddy that serves several of your domains.
3. Put the rules address in your config (it is not a secret, just don't publish it) and the key in
   an environment variable (it is a secret: it authenticates your reports).

Once deployed, the first answer on the check path proves each hostname is yours and connects it;
the dashboard checks every 15 minutes, or immediately with **Check now**.

## Caddyfile

```caddyfile
{
	# Optional: on-demand certificates only for hostnames in your RedirHub rules.
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

Syntax:

```caddyfile
redirects [<rules_url>] {
	rules_url  <url>
	key        <integration key>
	report_404 rules|all
}
```

- **rules_url** (required): the Workspace rules address. It also says where reports go (`@hits`),
  so it stays required even when you only want 404 monitoring.
- **key**: the integration key. Without it, redirects work but nothing is reported.
- **report_404**: which 404 answers of your site are reported. `rules` (the default) follows the
  dashboard's **Report 404 pages** switch of each hostname in the rules (`report404`). `all`
  reports every 404 of every hostname this site block serves, listed in the rules or not, so you
  can monitor 404s without adding each domain to RedirHub first. Hostnames that are not in the
  rules get nothing else: no redirects, no check path.

The directive is ordered `before redir` automatically: it runs after `header` and before `redir`,
`rewrite`, `uri`, `try_files` and the handlers that serve your site (`reverse_proxy`,
`php_fastcgi`, `file_server`, …). Rules match the path as the client sent it, before any rewrite.
A catch-all rule only fires when the rest of the route answers 404: behind `try_files` with a
fallback (a single-page app's `try_files {path} /index.html`, `php_fastcgi`'s own fallback) the
site never answers 404, so the catch-all never fires. You may still write
`order redirects before redir` (or another position) in the global options.

The permission module:

```caddyfile
permission redirects [<rules_url>]
```

Without `rules_url` it allows a hostname listed in any rules file a `redirects` handler of this
Caddy instance has loaded; with it, only that file (which it polls itself if no handler does).

The handler and the permission module share one poller per `rules_url`, however many sites and
handler instances use it, and it survives config reloads.

## JSON

```json
{
	"apps": {
		"http": {
			"servers": {
				"srv0": {
					"listen": [":443"],
					"routes": [
						{
							"handle": [
								{
									"handler": "redirects",
									"rules_url": "https://rules.rediredge.com/abc123/TOKEN.txt",
									"key": "{env.REDIRHUB_INTEGRATION_KEY}"
								},
								{
									"handler": "reverse_proxy",
									"upstreams": [{ "dial": "localhost:8080" }]
								}
							]
						}
					]
				}
			}
		},
		"tls": {
			"automation": {
				"policies": [{ "on_demand": true }],
				"on_demand": {
					"permission": { "module": "redirects" }
				}
			}
		}
	}
}
```

`report_404` is `"rules"` (default) or `"all"`. `rules_url` and `key` accept Caddy placeholders such as `{env.NAME}`. Full examples are in
[`examples/`](examples).

## Behaviour

The module follows [format v1](https://github.com/redirhub/integration-spec): for **GET and HEAD**
on a **hostname listed** in the rules it answers the check path (`/_redirhub/check`), redirects
exact rules **before** your site's own routes, and redirects to the hostname's catch-all
(`{host}/*`) only where your site answers **404**; everything else goes to the rest of the route
unchanged. The specification has the matching rules, the check answer and the report lines. What
is particular to Caddy:

- **The catch-all and your site.** A 404 that a handler returns as an error (e.g. `file_server`)
  counts too. Responses other than 404 stream through without buffering, and a 404 the catch-all
  replaces is discarded as it is written, never held in memory. Unlike a domain connected to
  RedirHub by DNS, where the catch-all takes the whole domain, the home page `/` and every other
  page your site serves stay with your site; to send them away too, add an exact rule for each
  (e.g. `{host}/`).
- **`report_404 all`.** The 404 answers of hostnames that are not in the rules are reported too;
  their responses are not changed.
- **Push, with polling as the fallback.** When the rules file has a `@notify` line, Caddy keeps
  one [Mercure](https://mercure.rocks) subscription (server-sent events) open per `rules_url` and
  downloads the rules at once when a message arrives, so a change is served within a second or
  two. The message is only a wake-up: nothing in it is trusted, Caddy downloads the file as on
  every poll. Every new connection, the first and each reconnect, also downloads once (a change
  may have come while it was closed). While the subscription is connected and healthy (the hub's
  heartbeat arrives at least once a minute), Caddy polls only **once an hour**; the moment it
  drops, it polls every `@refresh` seconds again and reconnects after 1 second, doubling up to 30
  seconds (back to 1 second after a connection that lasted over a minute), resuming with
  `Last-Event-ID`. The hub's token is never logged, nor the topic, which names the rules file.
  Without `@notify`, Caddy polls every `@refresh` seconds.
- **The last good copy is kept on disk**, in Caddy's data directory
  (`$XDG_DATA_HOME/caddy/redirects/`), so a restart redirects immediately even when the rules
  server is unreachable. Any failed download keeps it in use.
- **Reports** are batched in memory and sent every 5 seconds, or at 100 lines or 64 KB. A failed
  or rejected report is dropped, never retried, and never blocks or slows a request; when the
  in-memory queue is full, lines are dropped.
- **On-demand TLS.** With `permission redirects`, Caddy obtains a certificate only for hostnames
  listed in the rules, so you can serve `https://` for all your RedirHub hostnames without listing
  them in the Caddyfile and without letting anyone else make Caddy request certificates.

## Rules file format

The module reads Integration Mode **format v1**, specified in
[redirhub/integration-spec](https://github.com/redirhub/integration-spec): the rules file, the
check path and the report lines. `spec/v1/` here is a copy of its golden sample: this module's
tests load it, and CI fails when it differs from the specification's.

## Privacy

Each report line is the Apache combined log format with the absolute URL:

```
203.0.113.9 - - [31/Oct/2026:19:10:00 +0000] "GET https://promo.example.com/old-pricing?utm=x HTTP/1.1" 301 0 "https://www.google.com/" "Mozilla/5.0 …" file=promo.example.com/old-pricing destination=https://promo.example.com/pricing
```

That is: the visitor's IP address (Caddy's client IP, which honours `trusted_proxies`), the
time (UTC), the method, the requested URL with its query string, the protocol, the status, the
response size, the `Referer` and the `User-Agent` (each cut to 1 KB); for a redirect also the rule's source and the
`Location` sent. No cookies, other headers, request bodies or
response bodies are sent. Only redirects, and 404s of hostnames with 404 reporting turned on
(the **Report 404 pages** switch in the dashboard, or every hostname with `report_404 all`), are
reported; all other traffic stays on your
server. Requests are sent only to the `@hits` address in your rules file, with your integration
key.

## Limits

- Redirect types the edge renders itself (HTML pages, frames, password-protected links,
  destinations split between several targets) and wildcard hostnames need DNS mode; the rules
  file only contains plain 301/302/307/308 redirects.
- The rules file is at most 32 MB here (RedirHub keeps each hostname under 50,000 rules).
- Redirect changes take effect within a second or two while the push subscription (`@notify`)
  is connected, otherwise within the poll interval (`@refresh`, 60 seconds by default).
- At most 10,000 report lines wait in memory; beyond that, lines are dropped until the queue
  drains.

## Development

```sh
go vet ./...
go test -race ./...
xcaddy build --with github.com/redirhub/caddy-redirects=.
```

## License

Apache License 2.0, see [LICENSE](LICENSE). Copyright 2026 RedirHub.
