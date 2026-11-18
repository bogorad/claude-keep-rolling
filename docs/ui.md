# Status page UI

This is the local binding of the global ADR "HTMX Hypermedia Interface"
(`app-ui/htmx-hypermedia-interface`). The status page is server-rendered HTML.
The browser asks for HTML fragments with htmx. The plugin validates every
input, renders every error, and decides what a request means.

## Routes

The page shell is a plugin resource. CLIProxyAPI serves resources without the
management key, so the shell carries no data. Everything else is a management
route, and the host checks the key (`Authorization: Bearer <key>` or
`X-Management-Key: <key>`) before it calls the plugin.

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/v0/resource/plugins/keep-rolling/status` | Full page shell (no key, no data) |
| GET | `/v0/resource/plugins/keep-rolling/assets/htmx-2.0.11.min.js` | Vendored htmx |
| GET | `/v0/resource/plugins/keep-rolling/assets/json-enc-2.0.3.js` | Vendored htmx json-enc extension |
| GET | `/v0/management/keep-rolling/view?sort=&dir=` | Accounts, providers and settings fragments |
| GET | `/v0/management/keep-rolling/table?sort=&dir=` | Accounts fragment |
| POST | `/v0/management/keep-rolling/ping` | With `HX-Request: true`: accounts fragment. Without: JSON, as before |
| POST | `/v0/management/keep-rolling/select` | Accounts fragment plus a config write |
| POST | `/v0/management/keep-rolling/provider` | Providers fragment plus a config write |
| POST | `/v0/management/keep-rolling/settings` | Settings fragment plus a config write |
| GET | `/v0/management/keep-rolling/state` | JSON, unchanged |

POST bodies are form-encoded (`application/x-www-form-urlencoded`), the htmx
default:

- `ping`: `id`, `sort`, `dir`.
- `select`: `account`, `on=1` to keep the account rolling (absent: off),
  `sort`, `dir`.
- `provider`: `provider`, `field` (`enabled`, `model` or `select_all`), and
  the value of that field (`enabled=on`, `model=<id>`, `select_all=on|off`).
- `settings`: `prompt`, `delay_min`, `delay_max`, `stagger`, `meta_interval`,
  `cooldown_reset=on` (absent: off). An empty field means the default.

URLs inside the page and its fragments are relative to the page
(`../../../management/keep-rolling/...`), so a reverse-proxy prefix in front
of CLIProxyAPI keeps working.

## Sorting

Every column of the accounts table sorts on the server:

| `sort` | Column | Key |
| --- | --- | --- |
| `selected` | Keep | ascending: kept rolling first |
| `name` | Account | name, case-insensitive |
| `provider` | Provider | provider name, case-insensitive (default sort) |
| `last_ping` | Last ping | time of the last ping |
| `weekly` | Weekly | weekly use; "limit reached" without a number counts as 100% |
| `last_reset` | Last reset seen | time |
| `next_ping` | Next ping | time; empty when the account is not rolling |
| `cooldown` | Cooldown | end of an active cooldown; "unavailable" without an end sorts after every timed one |
| `ping` | Ping | ascending: ready, then pinging, then no ping possible |

`dir` is `asc` or `desc`. The sort is stable. Empty values sort last in both
directions. Ties fall back to the account name, then the account ID, both
ascending. An unknown `sort` gives the default sort (`provider`, `asc`); an
unknown `dir` gives `asc`. Neither is an error.

Each header is a button with `hx-get` to the table with the next `dir`, and
the `th` carries `aria-sort`. The active column shows ▲ or ▼.

## No hidden state

The sort lives only in URLs and form fields:

- The accounts fragment root has `hx-get` to its own URL with the current
  `sort` and `dir`, and `hx-trigger="every 15s [!document.hidden]"`. Polling
  keeps the sort and pauses while the tab is hidden.
- Each row form (ping, keep rolling) carries `sort` and `dir` as hidden
  inputs, so its response draws the table the same way.
- `hx-sync`: a poll is dropped while a click is in flight. A click aborts
  an in-flight poll. A sort click while a manual ping is in flight aborts the
  browser request. The ping still finishes on the server and shows at the
  next refresh.

## Validation ownership

The plugin validates every input. The page has no client-side checks.

- Settings use `rolling.Settings.Validate`, the same function that checks the
  host config on reload. It names the failing field (`rolling.FieldError`).
  The fragment comes back with status 422. It shows the submitted value with
  `aria-invalid` and the message under the field. Ranges: `delay_min` at
  least 1m; `delay_max` from `delay_min` to 6h; `stagger` 0s to 1h;
  `meta_interval` 5m to 24h.
- Provider changes: known provider and field; a model has no spaces and at
  most 200 characters; no model for a provider without pings.
- Account selection: the account must exist and must not be disabled. The
  server computes the new `providers` object from its own current settings,
  not from the browser.
- Ping: the same checks as the JSON route (known, not disabled, provider can
  ping, no ping already running).

Fragment status codes: 200, 409 (conflict) and 422 (invalid input). The page's
htmx config swaps those. 401 and 403 from the host replace `#app` with the
host's message, which also stops polling: repeated bad keys would make the host
ban the client. Any other 4xx or 5xx goes to the error line as text.

### Config writes

The plugin cannot write the host config: no host callback exists for that.
It does not relay the user's key through `host.http.do` to the host's own
API, because that would turn the plugin into a credential relay that must find
the host's listen address. So a valid change comes back with the fragment plus
an out-of-band `#write` element. The element sends one
`PATCH /v0/management/plugins/keep-rolling/config` with a JSON body (htmx
`json-enc`, `hx-trigger="load"`, `hx-swap="none"`). The plugin computes the
body and renders it into `hx-vals`; the browser adds nothing. The host
validates and saves the config, then reloads it asynchronously. On reload the
plugin checks it again with `rolling.ParseSettings`. An invalid config keeps
the previous settings and shows the error on the page. The fragment draws the
new values at once. Polling shows the applied state within 15 s.

## Management key: the named script exception

The only custom script on the page is one `htmx:configRequest` listener. It
copies the value of the password field `#key` into the `X-Management-Key`
header of every htmx request. The field has no `name`, so it is never sent as a
form value. The key lives only in that field: no cookie, no `localStorage`, no
`sessionStorage`. Reloading the page clears it. htmx history, which would copy
fragments to `localStorage`, is off (`historyEnabled: false`,
`historyCacheSize: 0`). `selfRequestsOnly: true` keeps htmx requests, and so
the key, on the page's origin. `allowScriptTags: false` stops scripts in
swapped fragments.

The page sets htmx options with `<meta name="htmx-config">`, which is
configuration, not script. The polling filter `[!document.hidden]` is an htmx
trigger expression, so htmx's `allowEval` stays on.

## Fallback

The page needs JavaScript. Without it, the browser cannot send the key header,
so no data loads and the page says so in a `<noscript>` notice. No
JavaScript-free fallback exists. A plain browser request to a management route
gets 401 from the host. Scripts and tools use the JSON routes (`state`, and
`ping` without `HX-Request`).

## Templates and assets

- `templates/page.html`: the full page (`page`). Styles are inline and keep
  the light and dark colours of the earlier page.
- `templates/fragments.html`: `view`, `accounts`, `providers`, `settings`, the
  `*-fragment` wrappers that add out-of-band parts, `status-oob` (version,
  error line, key status) and `write-oob` (the config PATCH).
- `ui.go`: view models, sorting, validation and handlers. `plugin.go` routes
  requests to them.
- Templates use `html/template`, which escapes every value for its context.
  Handlers prepare the display strings (relative times, 24-hour UTC titles).
  Templates only lay them out.

Vendored files, unmodified, from the npm registry (tarball integrity checked),
embedded with `go:embed` and served with `Content-Type: text/javascript`.
The page loads them with Subresource Integrity (`sha384`). No CDN is used.

| File | Package | License | sha256 |
| --- | --- | --- | --- |
| `assets/htmx/htmx.min.js` | `htmx.org@2.0.11` `dist/htmx.min.js` | 0BSD (`assets/htmx/LICENSE.htmx`) | `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717` |
| `assets/htmx/json-enc.js` | `htmx-ext-json-enc@2.0.3` `json-enc.js` | 0BSD (`assets/htmx/LICENSE.json-enc`) | `9fe14faaedb792c70c187e219b2d8e72fed92891375205dad9dfefe18d9accea` |

To update, replace the files, update the constants in `ui.go`, and update
this table. The asset paths carry the version, so browsers may cache them
with `immutable`.

## Tests

- `ui_test.go` (always runs): sorting for every column in both directions,
  ties, empty values, bad `sort`/`dir`; fragment HTML (escaping, `aria-sort`,
  arrows, the refresh URL that holds the sort, out-of-band parts); ping
  fragment and JSON route; settings validation errors and the config write;
  provider and selection writes; asset routes (content type, sha256);
  registration; full page (scripts, SRI, htmx config, one inline script, no
  storage); request decoding of `Headers` and `Query`.
- `browser_test.go`: runs only with
  `KEEP_ROLLING_BROWSER=/path/to/chromium`. It loads the real page in
  headless Chromium against a fake host that checks the key. It drives a bad
  key, the right key, a sort, a bad and a good Meta interval, and an account
  toggle. Then it checks the requests (key header, sorted URL, form bodies,
  JSON PATCH bodies) and the final DOM. CI does not run it.
