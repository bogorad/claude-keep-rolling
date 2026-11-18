# Keep Rolling

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that
starts each account's **weekly** usage window right after its reset, so idle
time does not delay the next window.

This is a fork of
[265866/claude-keep-rolling](https://github.com/265866/claude-keep-rolling)
v0.3.0 (MIT). The fork is generalized to several providers, tracks only the
weekly window, and builds against CLIProxyAPI SDK v8.0.63. The source audit of
upstream is in [AUDIT.md](AUDIT.md). Plugin ID: `keep-rolling`.

## How it works

- A provider adapter knows the provider's accounts, a minimal ping, and how
  to read the weekly reset from the reply headers.
- Each ping goes through `host.model.execute`, pinned to one account by its
  auth ID, with `stream=false`. The plugin makes no direct network calls and
  uses no management key. It declares only the management capability: it is
  not a scheduler and does not change account selection.
- After a successful ping, the next ping is at the reported weekly reset plus
  a random delay (`delay_min` to `delay_max`, default 1 to 10 minutes).
- After a start, first pings are spread at least `stagger` apart (default
  2 minutes) with a random offset, so a restart sends no burst. Ping history
  is in memory only, so after a restart every selected account is pinged once
  to learn its reset.
- Upstream retry rules are kept: a reset reported at or up to 10 minutes
  before the ping means clock skew, retry in 2 minutes; a reply without a
  usable reset is re-checked after 24 hours; a failed ping is retried after
  15 minutes, or after the host cooldown plus 1 minute if that is later; a
  failed manual ping does not move the scheduled ping.
- A due ping for an account in an active host cooldown waits until the
  cooldown ends, instead of failing into it.
- There is no five-hour logic for claude and codex.
- meta is the exception: it is pinged on a fixed interval (`meta_interval`,
  default 60m, no jitter), first at plugin start without stagger. A ping
  after a Muse window ended starts a new window at ping time. The weekly
  reset rules above do not apply to meta.

### Cooldown reset

A restored quota does not clear an active credential cooldown (CLIProxyAPI
issue #7125). With `cooldown_reset: true` (default), on the first round after
an account's known weekly reset passes (within 1 hour), the plugin checks the
host's runtime state for the account. If the host shows a cooldown
(`next_retry_after` in the future, or `unavailable`), it calls
`host.routing.reset_cooldown` for that account once. The decision is made
once per reset: a cooldown that starts later is left alone. The plugin never
clears a meta cooldown; a due meta ping waits until the cooldown ends.

The plugin learns resets only from its own ping replies (or the calendar, for
meta). `host.auth.list` and `host.auth.get_runtime` do not expose the host's
saved `quota.signals`, so the plugin cannot learn a reset without a ping.

### Providers

| Provider | Default | Window starts on first use | Reset source |
| --- | --- | --- | --- |
| claude | on | yes | `Anthropic-Ratelimit-Unified-7d-Reset` (Unix seconds or RFC 3339); `-7d-Status`, `-7d-Utilization` |
| codex | on | yes | `X-Codex-Primary-*` / `X-Codex-Secondary-*`: the window of 10080 minutes, else the longest window of at least one day; `-Reset-At` (Unix seconds), else `-Reset-After-Seconds`; `-Used-Percent` |
| meta | off | no (fixed Monday 00:00 UTC) | calendar; pinged every `meta_interval`; never a cooldown reset |
| xai | off | unknown | none in headers; stub, no ping |
| antigravity | off | unknown | none known; stub, no ping |

Pings:

- claude: Claude Messages, `max_tokens: 1`, no tools. Default model
  `claude-haiku-4-5-20251001`.
- codex: OpenAI Responses (`openai-response` in and out, forced provider
  `codex`), `instructions: ""`, `input` = prompt, `max_output_tokens: 16`
  (the Codex minimum), `store: false`. The host sends `stream: true` upstream
  and aggregates the reply. Default model `gpt-6-luna`.
- meta: OpenAI Responses (`openai-response` in and out, forced provider
  `meta`), `input: "ping"` (the prompt setting does not apply),
  `reasoning.effort: minimal`, `max_output_tokens: 16`, `store: false`.
  Measured cost 2026-11-18: 9 input + 16 output tokens. Default model
  `muse-spark-1.3-contributor`. A reply with status `incomplete` and reason
  `max_output_tokens` is a success; other `incomplete` reasons are failures.

Only OAuth accounts are used. API-key accounts and disabled accounts are
skipped.

## Configuration

Under `plugins.configs.keep-rolling` in `config.yaml`. The plugin acts only
when `plugins.enabled` and `plugins.configs.keep-rolling.enabled` are both
true (it reads these two flags from the host config file, see AUDIT.md F3).

```yaml
plugins:
  enabled: true
  configs:
    keep-rolling:
      enabled: true
      prompt: hi            # default
      delay_min: 1m         # default; at least 1m
      delay_max: 10m        # default; at most 6h
      stagger: 2m           # default; 0s to 1h, 0s turns it off
      cooldown_reset: true  # default
      meta_interval: 60m    # default; 5m to 24h
      providers:            # every key is optional
        claude:
          enabled: true
          model: claude-haiku-4-5-20251001
          select_all: true  # false: only the IDs in accounts
          accounts: []
        codex:
          enabled: true
          model: gpt-6-luna
        meta:
          enabled: false
```

Durations are Go duration strings or integer seconds. An invalid config keeps
the previous settings and shows the error on the status page. An unknown
provider key is an error.

## Status page

**Keep Rolling** in the Management Center sidebar
(`/v0/resource/plugins/keep-rolling/status`). The page is server-rendered HTML
driven by htmx 2.0.11 (vendored, no CDN); see [docs/ui.md](docs/ui.md) for
routes, validation, the script exception and tests. It asks for the
management key and keeps it in memory only: reloading the page clears it. It
needs JavaScript to send the key.

It shows, for each account: provider, last ping and its result, weekly use and
reset, last reset seen, next ping, host cooldown, and a **Ping now** button.
Every column sorts on the server; the table refreshes every 15 seconds while
the tab is visible and keeps its sort. Accounts can be kept rolling or not,
providers can be switched on and off and their model set, and the settings
form is validated on the server (for example `meta_interval` 5m to 24h). A
valid change is saved through
`PATCH /v0/management/plugins/keep-rolling/config` (shallow merge; the page
sends the whole `providers` object).

Management API: `GET /v0/management/keep-rolling/state`,
`POST /v0/management/keep-rolling/ping` with `{"id": "<auth id>"}` (JSON;
with `HX-Request: true` it returns the HTML fragment instead).

## Build (linux/arm64)

With the system Go toolchain on an arm64 host:

```sh
export GOTOOLCHAIN=local
go vet ./...
go test -race ./...
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.pluginVersion=0.6.0" \
  -o dist/keep-rolling-v0.6.0-linux-arm64.so .
```

The host takes the plugin ID and version from the file name
`<id>-v<version>.so`. Install the library as
`plugins/linux/arm64/keep-rolling-v0.6.0.so`. The host hot-reloads a plugin
only when its file path changes.

Before installing, remove the old `claude-keep-rolling` plugin. Its ID differs,
so both would run and ping at the same time.

Upstream builds release libraries in `golang:1.26-bookworm` to match the glibc
of the official CLIProxyAPI image (`scripts/release.sh`). A library built on
another host can need newer glibc symbols; check with
`objdump -T <lib> | grep GLIBC_` before installing.

## Account risk

Automated pings through subscription logins can break provider terms and put
the accounts at risk. Use at your own discretion.

## License

[MIT](LICENSE). Upstream copyright is kept.
