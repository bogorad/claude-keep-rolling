# Source audit: claude-keep-rolling v0.3.0

- Subject: `https://github.com/265866/claude-keep-rolling`, tag `v0.3.0`,
  commit `8874095d2bf353f6d10ce37e1cdaba33d64d70e0` (2026-11-02).
- Reason: ADR-028 §12 (a plugin is trusted native code; install only after a
  source audit) and the 2026-11-17 amendment (keep-rolling fork).
- Auditor: Claude agent, 2026-11-17, Bead `nix-config-cloud.88.2`.
- Method: read every non-test source file in full (`main.go`, `plugin.go`,
  `roller.go`, `internal/rolling/rolling.go`, `entry.c`, `abi.h`,
  `status.html`), the build and release scripts, and the CI workflow. Grep for
  network, file, process, and browser-storage APIs. Cross-check the host side in
  CLIProxyAPI SDK `v8.0.63` (`internal/pluginhost`).
- Scope limit: this is a read of the source, not of the released binaries.
  The fork is built from source, so the upstream release assets are not used.

## Summary

No finding blocks use as a base for the fork. Two findings were fixed in the
fork (F1, F2). The others are documented and accepted.

| ID | Severity | Finding | Fork action |
| --- | --- | --- | --- |
| F1 | Medium | Status page decodes the Management Center key from `localStorage` | Removed |
| F2 | Low | Pings every 5 h 01 min (five-hour logic) waste requests on accounts without a 5-hour limit | Replaced by weekly logic |
| F3 | Low | Reads the host `config.yaml` (contains client API keys) to check its own enabled flag | Kept, read-only, documented |
| F4 | Info | `entry.c` darwin/amd64 TLS and signal workaround | Kept; inactive on linux/arm64 |
| F5 | Info | No network calls outside host callbacks | None needed |
| F6 | Info | No file writes, no `os/exec`, no environment reads | None needed |
| F7 | Info | CI and release scripts use the network | Not part of the plugin |

## Findings

### F1. Management key decoded from Management Center storage (Medium)

`status.html` function `centerKey()` reads `localStorage["cli-proxy-auth"]`.
The Management Center stores the management key there, XOR-masked with
`"cli-proxy-api-webui::secure-storage|" + location.host + "|" + userAgent`.
The page reverses the mask and uses the plaintext key as a Bearer token for
same-origin management calls.

- The mask is obfuscation, not encryption. Any script on the same origin can
  do this. All plugin resource pages share the CLIProxyAPI origin
  (`/v0/resource/plugins/<id>/...`, served without authentication).
- Upstream does not send the key anywhere except same-origin `fetch` calls to
  `/v0/management/...` and `/v8/management/credentials/models`. No exfiltration
  was found.
- Risk: the pattern teaches plugin pages to harvest the operator's key. A
  later, less careful change could leak it.
- Fork: `centerKey()` is removed. The page asks for the key and keeps it in
  `sessionStorage` for the tab only (upstream behavior after a failed decode).

### F2. Five-hour ping cadence (Low)

Upstream v0.2.4 (live on cloud) pings every 5 h 01 min when no five-hour reset
header exists (`WindowFallback`). Probe evidence (Bead `nix-config-cloud.88.1`):
the cloud Claude accounts carry only `Anthropic-Ratelimit-Unified-7d-*`, so
those pings start nothing. v0.3.0 added `no_five_hour` and `weekly_reached`
modes to stop that. Fork: all five-hour logic and both modes are removed.
The fork schedules only against the weekly reset (see README).

### F3. Reads the host config file (Low)

`roller.go` `hostEnablesPlugin()` reads the file named by `-config` (or
`./config.yaml`) with `os.ReadFile` before every round, and parses only
`plugins.enabled` and `plugins.configs.<id>.enabled`.

- Reason (confirmed in SDK v8.0.63 `internal/pluginhost/host.go`): the host
  skips a disabled plugin at apply time but does not quiesce or shut down an
  already loaded library. Quiesce runs only when a new library path replaces
  the old one. Without this check a disabled plugin keeps pinging.
- The live `config.yaml` holds client API keys in plaintext. The plugin reads
  the file into memory, decodes two booleans, and drops the bytes. It does not
  log, store, or send the content.
- Fork: kept unchanged except for the plugin ID. It is a read, not a write, and
  not a network call. Error text logged on failure contains only the path and
  the YAML or OS error.

### F4. `entry.c` darwin/amd64 workaround (Info)

On `__APPLE__ && __x86_64__` only, `entry.c` swaps the Go `g` pointer in TLS
slot `%gs:0x30` around each host/plugin crossing and copies the host's
`SIGCHLD` handler onto `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPIPE`, and `SIGURG`.
On every other platform the macros compile to no-ops and
`return_signals_to_host()` is empty. The cloud target is linux/arm64, so this
code is not compiled into the fork's library. The C entry points otherwise
only forward calls and free buffers with `free()` (plugin buffers come from
`C.CBytes`, which uses `malloc`). No issue found.

### F5. Network (Info)

No Go source imports a network client. `net/http` is used only for
`http.Header` and status constants. All model traffic goes through
`host.model.execute` with `AuthID` set and `ForcedProvider`, so the host
executor, its proxy settings, and its logging apply. `status.html` loads no
external resources and calls only same-origin paths.

### F6. Files, processes, environment (Info)

No `os.WriteFile`, `os.Create`, `os.OpenFile`, `os.Remove`, `os/exec`,
`syscall`, or environment access. Only `os.ReadFile` (F3), `os.Args`, and
`os.Getwd` are used. `unsafe` appears only in cgo buffer handling in `main.go`.

### F7. CI and release (Info)

`.github/workflows/ci.yml`, `scripts/release.sh`, and `scripts/smoke-test.sh`
download tools and CLIProxyAPI releases (`gh release download`, `apt-get`).
They are not part of the built library. The fork builds locally with the
system Go toolchain and does not use them.

## Host-side facts checked (SDK v8.0.63)

- `host.model.execute` refuses `stream=true`. With `AuthID` it pins execution
  to that credential (`WithPinnedAuthID`). Internal calls get upstream response
  headers after `FilterUpstreamHeaders`, which keeps `Anthropic-Ratelimit-*`
  and `X-Codex-*`.
- The Codex executor forces upstream `stream=true`, aggregates the SSE reply,
  and returns the upstream headers. It raises `max_output_tokens` below 16 to
  16. `instructions` is set to `""` when missing.
- `host.routing.reset_cooldown` takes `auth_index` (not the auth ID) and calls
  `Manager.ResetQuota` with skip-persist, the same reset as
  `POST /v8/management/routing/cooldown/reset`. It does not touch token files.
- `host.auth.list` and `host.auth.get_runtime` return `HostAuthFileEntry`,
  which has `Status`, `StatusMessage`, `Unavailable`, and `NextRetryAfter`,
  but no quota fields. The saved `quota.signals` (`QuotaState.Signals`) is not
  exposed to plugins. `host.auth.get` returns the raw auth file, which holds
  OAuth tokens; the fork does not call it.
- The plugin ID comes from the library file name: `<id>-v<version>.so`.
