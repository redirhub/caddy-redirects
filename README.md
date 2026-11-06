# caddy-redirects

**Bulk redirects and 404 monitoring for [Caddy](https://caddyserver.com) v2.** Your domains keep
pointing at your own Caddy server; Caddy serves thousands of redirects from a remote rules file
and reports every redirect and every 404 back, so broken links show up before your visitors find
them.

- Redirects, short links, QR code links and whole website migrations, live within a minute,
  without editing a Caddyfile or reloading Caddy.
- 404 monitoring: the site's own 404s that no rule redirected are reported in the background, in
  batches, without delaying responses.
- The last good copy of the rules is kept on disk, so a restart redirects at once even when the
  rules server is out of reach.
- On-demand HTTPS only for the hostnames in your rules.

The rules file format is open ([below](#rules-file-format-v1)). [RedirHub](https://www.redirhub.com)
publishes it for you ("Integration Mode"): you manage the redirects in its dashboard, API or MCP
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
	rules_url <url>
	key       <integration key>
}
```

- **rules_url** (required): the Workspace rules address.
- **key**: the integration key. Without it, redirects work but nothing is reported.

The directive is ordered `before redir` automatically, so it runs before `redir` and the
handlers that serve your site (`reverse_proxy`, `php_fastcgi`, `file_server`, …), and after
`rewrite`, `uri` and `header`. You may still write `order redirects before redir` (or another
position) in the global options.

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

`rules_url` and `key` accept Caddy placeholders such as `{env.NAME}`. Full examples are in
[`examples/`](examples).

## Behaviour

Only **GET and HEAD** requests on a **hostname listed** in the rules are touched; everything else
goes to the rest of the route unchanged.

1. **Check path** (`/_redirhub/check`): answered with `200`, `text/plain`, `Cache-Control:
   no-store` and `token=<check token> etag=<ETag of the rules in use>`. This proves the hostname
   is yours and tells RedirHub whether Caddy has the latest rules.
2. **Exact rules** (a full source URL) redirect **before** your site's own routes, even when the
   page still exists.
3. Otherwise your site answers. Only when it answers **404** and the hostname has a **catch-all
   rule** (`https://{host}/*`), the 404 is discarded and Caddy redirects instead. A working site
   is never taken over. A 404 that a handler returns as an error (e.g. `file_server`) counts too.
   Unlike a domain connected to RedirHub by DNS, where the catch-all takes the whole domain, the
   home page `/` and every other page your site serves stay with your site; to send them away
   too, add an exact rule for each (e.g. `https://{host}/`).
   Responses other than 404 stream through without buffering.

Matching uses the request's hostname (any letter case, port ignored) and path (case-sensitive,
compared percent-decoded); the scheme and query string are ignored. Without the `q` flag the
query string is dropped from the target; with `q` it is kept; with `p` (catch-all only) the
request path is appended to the target.

**Fail open.** The rules are downloaded every 60 seconds (or the file's `@refresh`), with
`If-None-Match`, a 5 second timeout and no credentials. Any failure (network, 404, 5xx, a file
that cannot be read) keeps the last good copy. Until the first good copy, nothing is redirected
and the check path is not answered. The last good copy is saved in Caddy's data directory
(`$XDG_DATA_HOME/caddy/redirects/`), so a restart redirects immediately even when the rules
server is unreachable.

**Reporting.** Every redirect served (exact and catch-all), and for hostnames with
`report404` the site's own 404s that no rule redirected, are reported to RedirHub. Lines are
batched in memory and sent gzip-compressed every 5 seconds, or at 100 lines or 64 KB, with a 2
second timeout. A failed or rejected report is dropped, never retried, and never blocks or slows
a request; when the in-memory queue is full, lines are dropped. Other statuses are never reported.

**On-demand TLS.** With `permission redirects`, Caddy obtains a certificate only for hostnames
listed in the rules, so you can serve `https://` for all your RedirHub hostnames without listing
them in the Caddyfile and without letting anyone else make Caddy request certificates.

## Rules file format v1

The module reads a plain-text workspace rules file:

```
@format 1
@refresh 60
@hits https://rediredge.com/_edge/hits
@check /_redirhub/check
@host promo.example.com check=EEEXQ4RS report404
@host shop.example.com check=K3Vb9QmX
https://promo.example.com/old-pricing	https://promo.example.com/pricing
https://promo.example.com/promo	https://sale.example.com/	302
https://shop.example.com/*	https://www.example.net/	301	pq
```

- `@format 1`: the first line. A file whose major version is not 1 counts as a failed download:
  the last good copy stays in use. A file without `@format` is read as v1.
- `@refresh <seconds>`: the poll interval, clamped to 30–3600 seconds, default 60.
- `@hits <url>`: where reports go. Without it, nothing is reported.
- `@check <path>`: the check path (default `/_redirhub/check`).
- `@host <hostname> check=<token> [report404]`: one per served hostname. Unknown `key=value`
  fields are ignored.
- Unknown `@` directives are ignored.
- A rule line has tab-separated columns:
  `source<TAB>target[<TAB>status[<TAB>flags[<TAB>options]]]`.
  - **source**: a full URL `https://{host}/path`; `https://{host}/*` is the host's catch-all.
    Rules for hostnames without an `@host` line are ignored.
  - **target**: an absolute URL, used as is.
  - **status**: 301, 302, 307 or 308; empty means 301.
  - **flags** (may be empty): `p` appends the request path (catch-all only, no double slash),
    `q` keeps the query string (with `?` or `&`). Flags are must-understand: a rule with an
    unknown flag letter is skipped.
  - **options**: space-separated `key=value`, may be ignored; further columns are ignored.
- A line that cannot be parsed is skipped; the first rule for a source wins.

## Privacy

Each report line is the Apache combined log format with the absolute URL:

```
203.0.113.9 - - [31/Oct/2026:19:10:00 +0000] "GET https://promo.example.com/old-pricing?utm=x HTTP/1.1" 301 0 "https://www.google.com/" "Mozilla/5.0 …"
```

That is: the visitor's IP address (Caddy's client IP, which honours `trusted_proxies`), the
time (UTC), the method, the requested URL with its query string, the protocol, the status, the
response size, the `Referer` and the `User-Agent`. No cookies, other headers, request bodies or
response bodies are sent. Only redirects, and 404s of hostnames with 404 reporting turned on
(the **Report 404 pages** switch in the dashboard), are reported; all other traffic stays on your
server. Requests are sent only to the `@hits` address in your rules file, with your integration
key.

## Limits

- Redirect types the edge renders itself (HTML pages, frames, password-protected links,
  destinations split between several targets) and wildcard hostnames need DNS mode; the rules
  file only contains plain 301/302/307/308 redirects.
- The rules file is at most 32 MB here (RedirHub keeps each hostname under 50,000 rules).
- Redirect changes take effect within the poll interval (60 seconds by default).
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
