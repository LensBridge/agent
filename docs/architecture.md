# MusallahBoard on-device architecture (v2, local-first)

This document is the contract for everything that runs on a board, and for the
pieces that feed it: the LensBridge backend, the frontend build, `mbpush` on a
laptop and the Android app. If code and this file disagree, fix one of them
deliberately. It replaces the v1 `docs/offline.md`.

## 1. Why v1 was replaced

The v1 offline design was bolted onto an online-only board. The audit found:

| # | v1 problem | Consequence |
|---|---|---|
| A1 | Two runtime paths. Online boards loaded the hosted site and the live API; offline boards were served by the agent. A manual `mode` switch picked one. | An online board that lost internet mid-day, or rebooted during an outage, had nothing to show. Offline behaviour was only ever exercised on offline boards. |
| A2 | Bundles were unsigned. Trust lived entirely in the transport (an SSH push account). | Anyone holding any push key, or any compromised laptop, could put arbitrary content on a board. The device id is not a secret (it was in the kiosk URL). |
| A3 | No anti-rollback. | An old bundle could be replayed over a new one. |
| A4 | The SPA update path (`app install`) accepted any tarball, and the agent itself could only be updated over SSH by an admin. | Offline boards were stuck on whatever software they left the building with. |
| A5 | The push path needed an extra Unix account, per-board key provisioning, sshd surgery, a sudo rule and a root gate parsing `SSH_ORIGINAL_COMMAND`. Host keys were not checked. | Large attack surface and a lot of operational steps for "copy a file to a board". |
| A6 | The clock could be set to anything in 2026-2100 by whoever held a push key. | Wrong prayer times, wrong day's content. |
| A7 | The kiosk and the local server raced at boot; a timer navigated away from Chromium's error page. | Occasional blank board after boot. |

## 2. Principles

1. **Local-first, always.** Every board, online or not, renders from state on
   its own disk: the kiosk always loads `http://127.0.0.1:8080/`, served by the
   agent. The network only changes how fresh that state is. There is no
   online/offline mode.
2. **Everything that changes the board is a signed package.** Content, the
   board app (SPA) and the agent binary all arrive as `.mbu` files, verified
   against keys pinned on the device. Transports (backend sync, USB stick,
   laptop, phone) are untrusted pipes that all feed one importer.
3. **Two trust roots.** The backend holds a *content* key and can only sign
   content. Software (app and agent) is signed by a separate *release* key held
   offline or in CI. A compromised backend can put the wrong posters on a
   screen; it cannot run code on a board.
4. **Monotonic.** Packages never go backwards (content by sequence, software by
   version). Re-sending what is installed is a harmless no-op.
5. **Least privilege.** The daemon runs unprivileged. Two small root entry
   points exist, each of which re-verifies what it is handed: the agent
   self-updater and the USB import helper.
6. **Atomic and recoverable.** Every install is stage, fsync, rename, symlink
   swap. A power cut at any point leaves the previous state serving. A new
   agent is on probation for its first minutes and is rolled back
   automatically if it fails it; an interrupted agent update is finished
   from its journal at the next boot.

## 3. Components

```
                 LensBridge backend (content key)             GitHub releases (release key, CI)
                   |  signed content .mbu                        |  signed app/agent .mbu + channel json
        online: device-signed HTTPS                    online: HTTPS (channel)
                   |                                             |
   laptop (mbpush / browser) --HTTP upload--+                    |
   Android app ----------------HTTP upload--+                    |
   USB stick --> usb-import helper (root) --+--> inbox           |
   sudo musallahboard-agent import ---------+      |             |
                                                   v             v
   +--------------------------- musallahboard-agent (daemon, unprivileged) ---------------------------+
   |  importer: verify signature, type, device, monotonic --> store (content / app) or stage (agent)  |
   |  local server 127.0.0.1:8080: SPA, payload, media, status, SSE events, update screen             |
   |  upload server on the service port (10.77.0.1:80): upload page + import API                      |
   |  sync: content (backend), release channels, refresh socket;  backend WS: telemetry + commands    |
   +--------------------------------------------------------------------------------------------------+
            | CDP 127.0.0.1:9222 (update screen, reload)            | staged agent package
            v                                                        v
      Chromium kiosk (cage)                           musallahboard-agent-update.service (root, re-verifies)
```

## 4. Package format (`.mbu`, format version 2)

A zip file. Entries:

| Entry | Meaning |
|---|---|
| `mbu.json` | The manifest. This exact byte sequence is what is signed. |
| `mbu.sig` | Signatures over `mbu.json`. |
| anything else | A file listed in `mbu.json` `files`, byte-for-byte. |

Rules for the zip, checked before anything is written outside a staging directory:

- Every entry other than `mbu.json` and `mbu.sig` must be listed in `files`, and
  every listed file must be present exactly once. **Exception:** a `content`
  package may omit listed `media/...` files that the device already holds in
  its media store (online delta sync, section 9.1). A missing media file that
  the store does not have is an error.
- Entry names: slash-separated segments, each matching `[A-Za-z0-9_][A-Za-z0-9._-]*`
  (so no empty, `.`, `..` or hidden segments), at most 8 segments, at most 200
  bytes. No backslashes, no leading `/`. Directory entries (trailing `/`) are
  ignored. Anything else rejects the package.
- Only regular files. Duplicate names reject the package.
- Size limits: package at most 512 MiB; `mbu.json` at most 1 MiB; `mbu.sig` at
  most 64 KiB; at most 20000 files; the sum of `files[].bytes` at most 1 GiB.
  Every file is read to EOF with a limit of its declared size plus one byte.

### 4.1 `mbu.json`

```json
{
  "format": "mbu",
  "formatVersion": 2,
  "type": "content",
  "createdAt": "2026-09-24T14:02:11Z",
  "sequence": 1790258531000,
  "deviceId": "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b",
  "version": null,
  "files": [
    { "path": "payloads/2026-09-24.json", "sha256": "<64 lowercase hex>", "bytes": 5121 },
    { "path": "media/<sha256>.jpg",       "sha256": "<same sha256>",      "bytes": 482113 }
  ],
  "content": {
    "timezone": "America/Toronto",
    "firstDay": "2026-09-24",
    "lastDay": "2026-10-07",
    "media": [ { "path": "media/<sha256>.jpg", "contentType": "image/jpeg" } ]
  }
}
```

Common fields, all required unless noted:

| Field | Rule |
|---|---|
| `format` | `"mbu"` |
| `formatVersion` | `2`. Anything else is rejected. |
| `type` | `"content"`, `"app"` or `"agent"` |
| `createdAt` | RFC 3339 UTC, second precision. Used as a signed lower bound for the clock (section 10). With an NTP-synchronised clock, a package whose `createdAt` is more than a day ahead of it is refused. |
| `files` | Array as above. `sha256` lowercase hex of the file bytes; `bytes` its exact size. Paths unique and valid per the entry-name rules. |

Per type:

**`content`** (signed by a *content* key)

- `deviceId`: required, the enrolled device id. Content is always device-bound.
- `sequence`: required, positive integer, strictly increasing per device. The
  backend uses the build time in epoch milliseconds (not truncated to seconds,
  so two builds in the same second still order).
- `content.timezone`: IANA zone, not empty, not `Local`, loadable on the board.
- `content.firstDay`, `content.lastDay`: `YYYY-MM-DD`, inclusive, first <= last,
  at most 92 days.
- `files` contains exactly one `payloads/<date>.json` for every date in the
  range and no other payloads; and `media/<sha256>.<ext>` files (ext
  `[a-z0-9]{1,10}`) whose name hash equals their `sha256`. Nothing else.
- `content.media` lists every media file with its `contentType`
  (`image/jpeg`, `image/png`, `image/webp`, `image/gif`, `image/avif`,
  `image/svg+xml`, or `application/octet-stream`).
- Each payload file is a JSON object: the backend's `MusallahBoardPayload`
  (built by `BoardPayloadAssembler.assembleForDay`) for the start of that day in `timezone`, with `weather: null`,
  posters active at any point that day, and every non-empty `posterUrl`
  rewritten to `/media/<sha256>.<ext>` naming a listed media file. The agent
  rejects any payload with a `posterUrl` (at any depth) that is not such a path.
- Limits: payload file at most 16 MiB; total media at most 512 MiB.

**`app`** (signed by a *release* key)

- `version`: required, `MAJOR.MINOR.PATCH` (digits only).
- `app.localApi`: required integer, the local API version (section 7) the build
  needs. The agent refuses a build needing a newer API than it serves, with
  "update the agent first".
- `files` are the build output, rooted at the build directory: `index.html`
  must be one of them.

**`agent`** (signed by a *release* key)

- `version`: required, `MAJOR.MINOR.PATCH`.
- `agent.arch`: `arm64` or `amd64`; must equal the board's `GOARCH`.
- `agent.binary`: the path in `files` of the executable (normally
  `musallahboard-agent`). It is the only file.

Unknown top-level or per-type fields are ignored (forward compatible). A field
that is required for the type and missing, or present with the wrong JSON type,
rejects the package.

### 4.2 `mbu.sig` and keys

`mbu.sig` is required; a package without it, or with no signature that
verifies, is refused.

```json
{ "signatures": [ { "keyId": "0123456789abcdef", "sig": "<base64 of 64 bytes>" } ] }
```

- Algorithm: Ed25519 (RFC 8032, pure).
- Signed message: the ASCII bytes `musallahboard-mbu-v2\n` followed by the exact
  bytes of `mbu.json` as stored in the zip. The prefix is domain separation
  from the WebSocket handshake (`musallahboard-auth-v1`) and device HTTP auth
  (`musallahboard-http-v1`).
- `keyId`: lowercase hex of the first 8 bytes of SHA-256 of the raw 32-byte
  public key.
- A package is authentic if at least one signature verifies under a key of the
  role its type requires: `content` -> content keys; `app` and `agent` ->
  release keys. Signatures by unknown keys are ignored, not errors (rotation).
- Private keys are stored as the base64 of the 32-byte Ed25519 seed. Public keys
  are exchanged as base64 of the raw 32 bytes.

### 4.3 File names (convention only; the agent does not parse them)

- `musallahboard-content-<first 8 of deviceId>-<firstDay>.mbu`
- `musallahboard-app-<version>.mbu`
- `musallahboard-agent-<version>-<arch>.mbu`

### 4.4 Tools

`mbpack` (agent repo, `cmd/mbpack`) builds, signs and inspects packages:

```
mbpack keygen                                   print a new seed, public key and key id
mbpack app   --version 2.1.0 --local-api 2 --key-file K dist/ -o musallahboard-app-2.1.0.mbu
mbpack agent --version 0.3.0 --arch arm64 --key-file K build/musallahboard-agent-arm64 -o ...
mbpack verify [--content-key B64]... [--release-key B64]... file.mbu
mbpack inspect file.mbu
```

`--key-file` may be replaced by `--key-env NAME` (base64 seed in that
environment variable). The frontend repo also carries a dependency-free Node
packer (`scripts/package-mbu.mjs`) producing the same format.

## 5. Trust store

`/etc/musallahboard/trust.json`, `root:root 0644`:

```json
{
  "content": [ { "keyId": "…", "publicKey": "<base64 32 bytes>", "note": "lensbridge" } ],
  "release": [ { "keyId": "…", "publicKey": "<base64 32 bytes>", "note": "ci" } ]
}
```

- **Content keys** are pinned at enrollment: `POST /api/agent/enroll` returns
  them (`contentSigningKeys`) over the same TLS connection that establishes the
  device identity. `sudo musallahboard-agent trust fetch`
  (`GET /api/agent/signing-keys`, over TLS) re-reads them, for example after a
  key rotation.
- **Release keys** are compiled into the agent (`-ldflags -X
  github.com/LensBridge/agent/internal/core/trust.BuiltinReleaseKeys=<b64>[,<b64>]`)
  and may be extended in `trust.json`. A dev build without a compiled key
  accepts only keys in `trust.json`.
- `musallahboard-agent trust show|add <role> <b64>|remove <keyId>|fetch` manage
  it (root). The daemon only reads it.
- The root self-updater trusts only `trust.json` and the keys compiled into the
  binary running as root. Because the daemon can write the directory (it owns
  `kiosk-url`), every root reader and writer of `trust.json` refuses a file
  that is not a regular, root-owned, singly linked file writable only by root:
  a file the daemon planted there is never trusted or carried forward.

## 6. On-device layout

| Path | Owner | Contents |
|---|---|---|
| `/usr/bin/musallahboard-agent` | root 0755 | the agent |
| `/usr/lib/musallahboard/agent.prev` | root 0755 | previous agent, for rollback |
| `/usr/lib/musallahboard/update-in-progress.json` | root 0644 | journal of an agent update not yet finished (section 12) |
| `/usr/lib/musallahboard/update-strikes.json` | root 0644 | per agent version, updates rolled back only for not reconnecting (section 12) |
| `/etc/musallahboard/agent.toml` | daemon 0600 | config (section 11) |
| `/etc/musallahboard/agent.key` | daemon 0600 | device identity key (unchanged) |
| `/etc/musallahboard/trust.json` | root 0644 | trust roots |
| `/var/lib/musallahboard/state.json` | daemon | high-water marks and clock floor (below) |
| `/var/lib/musallahboard/content/media/<sha256>.<ext>` | daemon | content-addressed media store |
| `/var/lib/musallahboard/content/bundles/<sequence>/` | daemon | `mbu.json`, `mbu.sig`, `payloads/*.json` |
| `/var/lib/musallahboard/content/current` | daemon | relative symlink to `bundles/<sequence>` |
| `/var/lib/musallahboard/app/releases/<version>/` | daemon | SPA build + its `mbu.json` |
| `/var/lib/musallahboard/app/current` | daemon | relative symlink to `releases/<version>` |
| `/var/lib/musallahboard/agent/staged/` | daemon | `package.mbu` + `ready` for the root updater |
| `/var/lib/musallahboard/agent/last-update.json` | root | outcome of the last self-update |
| `/var/lib/musallahboard/inbox/` | daemon 0770 | `*.mbu` dropped by the CLI and USB helper |
| `/var/lib/musallahboard/inbox/results/<name>.json` | daemon | outcome per inbox file |
| `/var/lib/musallahboard/updates/{agent,app}.mbu` | daemon 0750 | software from the release channels, waiting for its install window (section 9.4) |
| `/var/lib/musallahboard/updates/install-now[.result.json]` | daemon | `update now` request from the CLI, and the daemon's answer |

The daemon keeps the current and previous content bundle and app release and
prunes the rest; media not referenced by a kept bundle is deleted.

`state.json`:

```json
{
  "contentSequence": 1790258531000,
  "appVersion": "2.1.0",
  "agentVersionsRejected": ["0.3.1"],
  "clockFloor": 1790258531
}
```

High-water marks live here, not only in the installed directories, so deleting
a bundle cannot reopen a rollback. Written atomically (tmp, fsync, rename).


## 7. Local kiosk API (`127.0.0.1:8080`, local API version 2)

Loopback only. `GET`/`HEAD` only; anything else is `405`. All JSON errors are
`{"message": "..."}`. `X-Content-Type-Options: nosniff` everywhere.

| Path | Behaviour |
|---|---|
| `/api/musallah/payload` | Today's payload from the current content bundle, verbatim (plus the weather overlay). No content: `503 {"message":"no content installed"}`. `Cache-Control: no-store`. |
| `/api/local/status` | Status object below. `no-store`. |
| `/api/local/events` | Server-Sent Events, below. |
| `/api/*` (other) | `404` JSON, never the SPA. |
| `/media/<sha256>.<ext>` | From the media store, if the name is valid and the file exists. `Content-Type` from the current bundle's manifest when listed there, else by extension. `Cache-Control: public, max-age=31536000, immutable`. |
| `/_mb/updating` | The update screen (`update-anim/index.html`, embedded in the agent). `no-store`. |
| anything else | Static file from the current app release. Unknown paths fall back to `index.html` (`no-cache`); `/assets/*` is immutable, and a missing `/assets/*` file is `404`. No app installed: the agent's built-in "board app not installed" page, `503`. |

**Weather overlay.** Content packages carry `weather: null`. When the board is
online, the sync loop fetches `GET /api/agent/weather` (device-authenticated,
section 9.3) every 30 minutes. The payload endpoint substitutes that object for
`weather` while it is less than 3 hours old; otherwise `weather` stays `null`
and the page hides the chip.

**Picking today's payload**: `today` is the current date in
the bundle's `timezone`; serve `firstDay` if `today < firstDay`, `lastDay` if
`today > lastDay`, else `today`.

**`/api/local/status`**

```json
{
  "localApi": 2,
  "agentVersion": "0.3.0",
  "deviceId": "3f2a…",
  "app": { "version": "2.1.0" },
  "content": {
    "sequence": 1790258531000, "createdAt": "2026-09-24T14:02:11Z",
    "firstDay": "2026-09-24", "lastDay": "2026-10-07", "timezone": "America/Toronto",
    "source": "sync", "installedAt": "2026-09-24T14:03:00Z"
  },
  "today": "2026-09-30", "servingDay": "2026-09-30", "daysRemaining": 7, "staleDays": 0,
  "sync": { "enabled": true, "lastSuccessAt": "…", "lastAttemptAt": "…", "lastError": null },
  "update": { "active": false },
  "updates": {
    "available": [ { "type": "app", "version": "2.2.0", "description": "board app 2.2.0" } ],
    "installTime": "23:00",
    "installAt": "2026-09-30T23:00:00-04:00",
    "installing": false
  }
}
```

`app` and `content` are `null` when nothing is installed; `servingDay`,
`daysRemaining` and `staleDays` are then `null` and `today` is in the system
zone. `source` is one of `sync`, `usb`, `upload`, `cli`. `sync.lastError` is a
short human string or `null`. If installed content cannot be read, `content` is
`null` and an `error` string is added. `updates` (section 9.4) lists software
waiting for its install window; `installAt` is `null` when nothing waits, and
the current time when the window is open or the board has no app yet. The page
puts "Update available: ... will be installed at 11:00 PM tonight" on its
ticker while `available` is not empty.

**`/api/local/events`** (SSE, `text/event-stream`, `no-store`):

- `event: content` with `data: {"sequence": n}` after new content is installed
  whose payloads differ from what was being served. The page re-fetches the
  payload in place.
- `event: app` with `data: {"version": "…"}` after a new app release is
  installed. The page reloads itself.
- `event: updates` with the `updates` status object whenever an update starts
  or stops waiting, or starts installing. The page re-reads the status.
- `event: notice` with `{"tone", "headline", "lines", "seconds"}`: an update
  outcome to show as a banner over the board for `seconds` (0: until the next
  notice replaces it). See section 8.
- A comment line (`: ping`) every 25 s keeps the connection alive.

`EventSource` gives up for good on a non-200 answer (for example while the
agent restarts), which is one reason the page also polls: it polls the payload every 10 minutes (which is what rolls the day
over after midnight) and status every minute, so a missed event costs at most
that long.

## 8. Importer

One importer in the daemon serialises every install. Sources: `sync`, `usb`,
`upload`, `cli`. Input is a batch of package files.

Per package:

1. **Verify** in a private staging directory: zip rules, manifest schema,
   signature by the right role, type rules (section 4), and for content the
   device id. A content package for another device is reported as `skipped`
   ("for another board"), not as a failure: one USB stick can serve many
   boards.
2. **Decide** against `state.json`: newer -> install; equal -> `unchanged`;
   older -> `outdated` ("The board already has newer content"). Neither is a
   failure: an old USB stick at a board that synced since is the normal case.
   An agent version in `agentVersionsRejected` is `rejected`.
3. **Install**:
   - content: move media into the store (verified hash, fsync), write the
     bundle directory, fsync, rename into `bundles/`, swap `current`, record the
     sequence, prune, emit `content` if payloads changed.
   - app: extract into `releases/.staging-*`, fsync, rename, swap `current`,
     record the version, prune, emit `app`.
   - agent: write the verified package to `agent/staged/package.mbu` and create
     `agent/staged/ready`. The root updater does the rest (section 12).
4. **Clock**: every verified package raises `clockFloor` to its `createdAt`
   (section 10). With NTP synchronised, a `createdAt` more than a day ahead
   of the clock is refused before anything is recorded ("An update is dated
   in the future: the computer that made it has a wrong clock"), and one
   less than a day ahead raises the floor only to NTP time.

A batch processes `agent` packages first, then `app`, then `content`. When an
agent package is staged, the rest of the batch stays in the inbox and is
processed by the new agent after the restart.

**What the people at the board see.** Every batch ends in one *notice*
(`internal/notice`): a tone (`ok`, `neutral`, `problem`), a headline and at
most three plain-language lines, more folded into "and N more". Lines never
name files; technical reasons go to the logs and the result's `detail`.

| Outcome | Headline | Lines |
|---|---|---|
| something installed | "Update complete" | "Board app 2.1.0", "New content, through Wednesday, October 7" |
| installed, something refused | "Some updates were not installed" | the refusals, then what installed |
| nothing installed, something refused | "Update not installed" | "An update is not signed for this board" |
| nothing needed doing | "Already up to date" | "The board already has newer content" |
| only other boards' content | "Nothing here is for this board" | |

*Update screen or banner.* The update screen (`/_mb/updating`) goes up only
when something is about to be installed: content, app or agent from `usb`,
`upload` or `cli`, and app or agent from `sync`. The daemon navigates the
kiosk there over CDP, drives it through `window.mbUpdate`
(`caption(text)` while working, `complete({tone, headline, lines, seconds,
message})` at the end) and navigates back to `/` after the countdown. Each
switch is a dip to black: the daemon fades the page on screen to black (a
veil injected over CDP, 600 ms) and both pages fade in from black on load.

Everything else a person caused is a *banner* over the running board, sent
as the `notice` event on `/api/local/events` and drawn by the board app: a
progress banner as soon as a batch starts ("Checking updates · From the USB
stick"), then its outcome when nothing was installed. Background content sync
is silent either way: an admin editing a poster must not interrupt every
screen.

Results are JSON objects per package:

```json
{ "file": "musallahboard-app-2.1.0.mbu", "type": "app", "version": "2.1.0",
  "action": "installed", "message": "Board app 2.1.0" }
```

`action` is one of `installed`, `staged` (agent, pending restart),
`unchanged`, `outdated`, `skipped` (another board's content), `rejected`
(with `detail`), `queued` (behind a staged agent).

## 9. Transports

### 9.1 Online: backend content sync

The daemon, when `content_sync` is on and the board is enrolled:

- syncs at startup, every 30 minutes, and 10 s after any message on the
  backend's refresh channel (`wss://<backend>/api/refresh-musallahboard?deviceId=<id>`,
  debounced),
- with `POST /api/agent/content-bundle`, device-authenticated (9.2), body
  `{"days": <content_days, default 7>, "haveMedia": ["<sha256>", ...]}`,
- and imports the returned package with source `sync`.

`haveMedia` lists the media the store already holds; the backend leaves those
files out of the zip (they are still listed and signed in `mbu.json`). Sync
failures are recorded in `sync.lastError` and retried with backoff (1 min to 30
min); the board keeps serving what it has, and with a 7-day window an online
board survives a week-long outage with full content.

### 9.2 Device HTTP authentication (backend)

Headers on device-authenticated requests:

| Header | Value |
|---|---|
| `X-MB-Device-Id` | device UUID |
| `X-MB-Timestamp` | Unix milliseconds |
| `X-MB-Signature` | base64 Ed25519 signature by the device key over the message below |

Message (UTF-8, `\n` separated, no trailing newline):

```
musallahboard-http-v1
<METHOD>
<request path, no query string>
<deviceId>
<timestamp>
<lowercase hex SHA-256 of the request body (of the empty string if none)>
```

The backend rejects: unknown or revoked device (`401`), timestamp more than 5
minutes from its clock (`401`), bad signature (`401`). Replays inside the window
only repeat an idempotent read.

**Board report.** Every heartbeat on the backend WebSocket carries
`telemetry.board` (`localserver.BoardReport`): `appVersion`, `content`
(`firstDay`, `lastDay`, `createdAt`, `source`, `installedAt`,
`daysRemaining`, `staleDays`), `updates` (as in the status), `lastAgentUpdate`,
`clock` (`source`, `trusted`), `syncError` and `error`. The backend keeps the
latest per device and the admin portal shows it: a board nobody is standing
in front of still says what it runs, what it shows, and what is wrong.

### 9.3 Backend endpoints (additions)

| Endpoint | Auth | Returns |
|---|---|---|
| `POST /api/agent/content-bundle` | device (9.2) | `200 application/vnd.musallahboard.mbu` signed content package. `503` if no content signing key is configured. |
| `GET /api/agent/weather` | device (9.2) | `{"weather": <object> or null, "fetchedAt": "<ISO-8601>"}`. Never a 5xx for missing weather: `weather` is `null`. |
| `GET /api/agent/signing-keys` | none | `{"content": [{"keyId", "publicKey"}]}` |
| `POST /api/agent/enroll` | token | unchanged, plus `contentSigningKeys: [{"keyId", "publicKey"}]` |
| `GET /api/admin/board/devices/{id}/offline-bundle?days=14` | `BOARD_DEVICE_READ` | the signed `.mbu` (filename `musallahboard-content-<id8>-<firstDay>.mbu`), media always included |

Backend configuration: `musallahboard.content-signing.private-key` (base64
seed; env `MUSALLAHBOARD_CONTENT_SIGNING_KEY`), and optionally
`musallahboard.content-signing.previous-public-keys` (comma-separated base64)
still published in `signing-keys` during a rotation.

### 9.4 Online: release channels

When `auto_update` is on, the daemon checks every 6 hours (and 2 minutes after
start) two channel URLs. A failed check (usually a board with no internet) is
retried after 5 minutes, then 10, 20 and so on up to 6 hours, so the error
clears soon after the network returns. Each channel fetch gives up after 30
seconds and is retried once on a fresh connection. Each URL returns:

```json
{ "version": "2.1.0", "url": "https://…/musallahboard-app-2.1.0.mbu", "sha256": "…", "bytes": 1234567 }
```

If `version` is newer than what is installed and what is already waiting (and
not rejected), it downloads `url` (size-capped, sha256-checked), verifies it
and checks it would install, and keeps it in `updates/` instead of installing
it. The channel file is only a pointer; the package's own signature and the
monotonic rules are what make it safe. The agent channel is checked first: an
app needing a newer local API than this agent serves only waits when a new
enough agent waits with it.

Waiting updates install together, as one batch with source `sync` (so the
update screen shows), in the board's install window: from `update_time`
(default `23:00`, in the installed content's time zone, else the system's) for
4 hours. A board that was off at 23:00 but on by 03:00 still updates that
night. Two exceptions install at once: a board with no app installed (it has
nothing else to show), and `update now`:

- `sudo musallahboard-agent update now` on the board, or
- the `update.install_now` remote command from the admin portal,

checks both channels and installs whatever is new or waiting, whatever the
time. Waiting updates survive a restart; one that no longer applies (installed
from a USB stick meanwhile) is dropped. Updates carried to the board (USB,
upload, CLI `import`) never wait. Defaults:

- `app_channel_url = "https://github.com/LensBridge/MusallahBoard/releases/latest/download/app-channel.json"`
- `agent_channel_url = "https://github.com/LensBridge/agent/releases/latest/download/agent-channel-<arch>.json"`

### 9.5 Offline: service-port upload server (laptop and phone)

When `service_port` is on, the daemon listens on `10.77.0.1:80` (bound with
`IP_FREEBIND`, so it is ready before the address exists; the agent has
`CAP_NET_BIND_SERVICE`). Requests whose `Host` is not `10.77.0.1`,
`10.77.0.1:80` or `musallahboard.local` get `421` (DNS-rebinding guard).

| Method, path | Behaviour |
|---|---|
| `GET /` | A small self-contained upload page: pick or drop `.mbu` files, see progress and per-package results, see board status and clock drift. |
| `GET /api/status` | `{"deviceId", "agentVersion", "app", "content", "today", "daysRemaining", "staleDays", "clock": {"unix", "timezone"}, "rtc": bool, "update": {"active": bool}}` |
| `POST /api/import` | `multipart/form-data`, one to 16 `package` parts (each at most 512 MiB, total at most 1 GiB). Optional header `X-MB-Client-Time: <unix seconds>`. Each part is streamed to the inbox, then processed as one batch (source `upload`). Response `200 {"results": [...], "notice": {...}, "restarting": bool, "agentVersion": "…", "clock": {"driftSeconds": n, "adjusted": bool, "note": "…"}}`. Only one import runs at a time; a second gets `409`. |

Errors are `{"message": "..."}` with a 4xx status. `driftSeconds` is the
board's clock minus the uploader's, before any correction (positive: the board
was ahead).

No authentication: every package is signed, device-bound where it matters and
monotonic, so the worst an unauthenticated uploader can do is make the board
verify a file and refuse it. Sizes and the single-import rule bound that.
`ufw` allows `80/tcp` only on `eth0` from `10.77.0.0/24`.

Clients:

- **Browser**: `http://10.77.0.1/` from any laptop. Nothing to install.
- **`mbpush`** (agent repo, Windows/macOS/Linux): `mbpush [--host 10.77.0.1] <file.mbu>...`,
  `mbpush status`, `mbpush fetch [--dir D] [--arch arm64]` (downloads the
  latest app and agent packages from the channels ahead of a visit; the content
  package comes from LensBridge's "Download offline bundle"). `--ssh user@host`
  streams to `sudo musallahboard-agent import -` for boards reachable only by
  SSH.
- **Android app**: downloads the content package from LensBridge and the
  software packages from the channels while it has signal, then POSTs them over
  a USB ethernet adapter bound to that `Network`.

### 9.6 Offline: USB stick

Copy any `.mbu` files to the root of a USB stick, or to a `MusallahBoard/`
folder on it, and plug it into the board.

- udev (`/etc/udev/rules.d/90-musallahboard-usb.rules`) starts
  `musallahboard-usb-import@<kernel name>.service` (root, oneshot,
  `PrivateNetwork=yes`) for a USB block device with a filesystem.
- `musallahboard-agent usb-import <kernel name>` checks the name
  (`^sd[a-z]+[0-9]*$`), reads the type with `blkid` and allows only `vfat`,
  `exfat`, `ext4`, `ntfs3`/`ntfs`; mounts it at `/run/musallahboard/usb/<name>`
  with `ro,nosuid,nodev,noexec,noatime`; copies at most 16 regular files (no
  symlinks) named `*.mbu`, each at most 512 MiB, into the inbox; unmounts; then
  waits up to 15 minutes for the daemon's results and logs them.
- The helper tells the board what it is doing through banners: it drops a
  notice in the inbox (`inbox/usb-<dev>.notice.json`) that the daemon shows.
  "Reading USB stick" as soon as the stick is seen; then either the batch's
  own outcome (section 8), or why there is none: "Can't read this USB stick"
  (no or unsupported filesystem, mount failure, copy failure), "No updates on
  this USB stick", or "USB updates are turned off". The helper unmounts the
  stick before the daemon sees what it copied, so every outcome, on the
  screen or as a banner, ends with "You can remove the USB stick" (the
  notice's `footer`), even while the board is still installing.
- `usb_import = false` in `agent.toml` disables it (the helper says so on the
  board and exits).
- The helper's sandbox (`ProtectKernelModules=yes`) cannot load filesystem
  drivers, so `exfat` and `ntfs3` are loaded at boot from
  `/etc/modules-load.d/musallahboard.conf`. NTFS always mounts with `ntfs3`.

### 9.7 Admin CLI

`sudo musallahboard-agent import <file.mbu>... | -` copies into the inbox (`-`
reads one package from stdin) and waits for and prints the results.

`sudo musallahboard-agent update now` asks the daemon to check the release
channels and install a new app or agent at once (section 9.4), and prints the
results. `musallahboard-agent status` shows what is waiting and when it
installs.

## 10. Clock

Correct time decides which day's content is shown and the prayer times, and
the backend refuses a board's signed requests when its clock is more than 5
minutes off.

- **NTP is the authority.** When systemd-timesyncd has synchronised the
  clock (`/run/systemd/timesync/synchronized`), the agent never moves the
  clock, ignores uploaders' clocks, and refuses packages signed more than a
  day in its future (section 8).
- `clockFloor` in `state.json` is a lower bound on the time: the maximum of
  every verified package's `createdAt` (also kept as `signedClockFloor`) and
  the trusted time the daemon persists every 10 minutes. The persisted time
  may not raise the floor more than 400 days past `signedClockFloor`.
- **What is persisted.** With NTP: NTP time, once two checks 10 minutes
  apart agree with the monotonic clock (a clock stepped by hand between
  timesyncd's polls is not taken for NTP time). Without NTP: the last
  trusted time (the floor at daemon start, a newer signed `createdAt`, or NTP
  time earlier this boot) carried forward by the monotonic clock. The wall
  clock itself is never written to the floor without NTP: a manual `date`, a
  wrong RTC or an uploader's time cannot raise it.
- **A floor ahead of NTP time is wrong** (an earlier agent let an uploader,
  a manual date or a wrong RTC raise it, or a backend signed from the
  future). With NTP, once two checks agree, `clockFloor` and
  `signedClockFloor` are lowered to NTP time and the journal says so
  (`clock: the clock floor was ahead of NTP-synchronised time`). A board in
  that state recovers by itself within about 10 minutes of NTP
  synchronising.
- **Without NTP**, at daemon start and every 10 minutes: if the clock is
  more than 60 s behind `clockFloor`, set it to `clockFloor` (the agent has
  `CAP_SYS_TIME`) and write the RTC when one is present. This is what keeps
  a board with no RTC and no network from coming up in the past after a
  power cut.
- An upload's `X-MB-Client-Time` (the uploader's clock when it started
  sending; the board measures the offset when the request arrives) is
  applied only when the clock is not NTP-synchronised, only after at least
  one package in that request verified, only when it differs by more than
  5 s, and only within `[clockFloor, clockFloor + 90 days]`. It sets the
  clock and the RTC but not the floor, so a later uploader with a right
  clock, or NTP, undoes a wrong one. The response reports the drift.
- **Trust.** The floor keeps the clock from going backwards; it cannot say
  the clock is right. `clock` in `/api/local/status` does: `source` is `ntp`
  (systemd-timesyncd has synchronised it), `rtc` (a hardware clock is
  fitted), `uploader` (an upload with a verified package set or confirmed it
  since this boot, recorded against the kernel boot id), `starting` (the
  first 3 minutes after boot) or `unverified`. `trusted` is false only for
  `unverified`, and then the board shows "The clock may be wrong: connect the
  board to the internet, or send an update from a laptop", the upload page
  says the clock is not confirmed, and `status` suggests an RTC.
- **Trade-offs.** Without NTP, the floor resists rollback as before: nothing
  moves the clock below it, and an uploader moves it at most 90 days ahead.
  The floor now only counts time the board was running since its last
  trusted time, so an uploader's correction and time spent powered off are
  not in it, and a later uploader (with a verified package) can set the
  clock back as far as the floor. Whoever can answer the board's NTP
  queries (unauthenticated NTP: on the LAN, or on the path to the NTP
  servers) decides the time, as they always could through timesyncd; the
  floor no longer fights them, and they can lower it to their time. Set
  back more than a day, the board also refuses new packages as dated in the
  future until the spoofing stops. Signed content still cannot be replayed
  (sequence and versions are monotonic and do not depend on the clock).
- A DS3231 RTC is still recommended (`setup.sh --rtc`), and online boards keep
  `systemd-timesyncd`.

## 11. Configuration (`/etc/musallahboard/agent.toml`)

Existing keys (`device_id`, `backend_url`, `websocket_url`, `key_path`) are
unchanged, with one rule (`config.CheckTransport`): `backend_url` must be
`https://` and `websocket_url` `wss://` on the same host as `backend_url` (the
port may differ: TLS binds the host name, not the port; there is no override
for a socket on another host). `enroll` checks the backend URL before it sends
the token and the returned WebSocket URL before it saves anything, and the
daemon checks both at every start. A config that fails is still read, so the
board keeps showing what it has and the local CLI keeps working, but nothing
that talks to the backend is started (content sync, refresh channel, release
checks, the command WebSocket); the reason goes to the log, `systemctl status`
and the `sync`/backend errors in `/api/local/status`. New keys, all optional:

| Key | Default | Meaning |
|---|---|---|
| `service_port` | `false` | Run the eth0 service port and the upload server. Set by `musallahboard-agent service-port on|off`. |
| `usb_import` | `true` | Accept USB sticks. |
| `content_sync` | `true` | Sync content from the backend. |
| `content_days` | `7` | Days per synced content package (1-92). |
| `auto_update` | `true` | Follow the release channels. |
| `app_channel_url`, `agent_channel_url` | section 9.4 | Channel pointers. Empty disables that channel. |
| `update_time` | `"23:00"` | When software from the channels installs, `HH:MM` in the board's time zone (section 9.4). |
| `insecure_dev` | `false` | Development boards only: allow `http://` and `ws://` (the same-host rule still applies). Set by `enroll --insecure-dev`; the daemon logs a warning at every start while it is on. |

An offline board needs no configuration change: sync attempts fail fast with no
route and cost nothing, and the moment it gets a network it starts syncing.

## 12. Agent self-update

1. The daemon verifies an agent package, writes it to
   `agent/staged/package.mbu` and creates `agent/staged/ready`.
2. `musallahboard-agent-update.path` (`PathExists=` on `ready`) starts
   `musallahboard-agent-update.service` (root, oneshot):
   `musallahboard-agent selfupdate apply`.
3. As root, the updater: removes `ready`; finishes any unfinished update
   first (below) and stops there; copies `package.mbu` into a
   root-only temporary directory (so the daemon cannot swap it mid-check);
   verifies it from scratch against root-trusted keys only; checks arch,
   that the version is newer than its own and that it is not in
   `agentVersionsRejected`; extracts the binary to
   `/usr/bin/.musallahboard-agent.new`; runs `.new version` and requires the
   expected version; copies the current binary to
   `/usr/lib/musallahboard/agent.prev`; asks the running agent whether it is
   connected to LensBridge; writes the journal; renames the new binary into
   place; restarts `musallahboard-agent.service`.
4. Probation. The new agent must report its `agentVersion` on
   `GET http://127.0.0.1:8080/api/local/status` within 2 minutes; then
   systemd must not restart it (same `MainPID` and `NRestarts`, still
   `active`) for 4 minutes; and if the board was connected before the update
   (`backend.connected`, or `sync.lastSuccessAt` with no `lastError`), it
   must connect again within 3 minutes. A board that was offline is not
   asked to be online. If it passes, the outcome is written and the
   enrollment splash refreshed (`splash install`).
5. If it fails, the updater restores `agent.prev`, records the outcome in
   `agent/last-update.json`, and only then restarts the previous agent, so it
   can read it. Not coming up, a restart or crash loop, or a probation cut
   short 3 times adds the version to `agentVersionsRejected`, so a broken
   release cannot loop. Not reconnecting only counts a strike
   (`update-strikes.json`), because the backend or the network may be what
   failed; the second strike rejects the version. If `agent.prev` cannot be
   restored the outcome is `rollback-failed` and the new agent keeps running.

**The update journal.** `/usr/lib/musallahboard/update-in-progress.json` is
root-owned, outside the daemon's reach, and written (temp file, fsync,
rename, fsync of the directory) before the binary is replaced and at every
change of phase. It names both versions and the SHA-256 of the previous and
the new binary. The path unit watches it as well as `ready`, and the updater
also puts `ready` back while an update is open (boards whose path unit
predates the journal watch only `ready`), so an update cut short by a power
cut, a reboot or the updater being stopped is finished by the next run, at
boot at the latest. That run is the binary installed then, so the record's
format only ever gains fields. What it does:

| Journal phase | Installed binary | The next run |
|---|---|---|
| none | | nothing to finish; handles a staged package, if any |
| `swapping` | previous | the swap never happened: removes the extracted binary, outcome `refused` ("interrupted"), nothing else changes |
| `swapping` or `probation` | new | starts the probation again from scratch, without restarting the agent; after 3 cut-short probations, rolls back and rejects |
| `swapping` or `probation` | anything else | someone installed another agent: left alone, outcome `refused` |
| `committing` | | writes the `ok` outcome again (same timestamp), refreshes the splash, removes the journal |
| `rolling-back` | | repeats the rollback: restores `agent.prev` if it is not in place, rejects or strikes (idempotent), writes the outcome, removes the journal, restarts the agent |
| unreadable | | puts `agent.prev` back if it differs, outcome `rolled-back` |

The journal is removed only after the outcome is written, and whenever the
updater ends other than by being stopped, so the path unit cannot loop on
it. SIGTERM (shutdown, or the unit's 15 minute start timeout) stops a
probation and leaves the journal; a rollback that has begun always finishes.
Boards set up before the journal existed run the updater under a 5 minute
timeout; a typical update (seconds, then the 4 minute probation) fits, and
one that does not is resumed as above.

Not covered: a new agent that starts crashing after its probation is
restarted by systemd for ever (with a growing delay) and is not rolled
back.

Telling the people at the board (`internal/agentupdate`): the agent that
staged the package leaves the screen on "Restarting MusallahBoard" and its
batch's notice in `agent/pending-notice.json`. Then:

- **success**: the new agent shows that notice ("Update complete · Agent
  0.3.0 · Board app 2.1.0") at startup, which is before its probation ends:
  a rollback afterwards replaces it with the rollback notice below;
- **rollback**: the previous agent, restarted, finds an unseen `rolled-back`
  outcome and shows "Update not installed · Agent 0.3.0 did not start
  correctly, so the board went back to 0.2.1", with the batch's other lines;
- **refusal**: the updater never restarts anything, so the staging agent
  watches `last-update.json` for up to 3 minutes and shows "Agent 0.3.0 could
  not be installed on this board" itself, then resumes its inbox.

Each outcome is shown once (`agentUpdateSeen` in `state.json`), on the update
screen if the kiosk is still on it, else as a banner. If the batch had more
behind the agent (queued in the inbox), the screen is held with the agent's
outcome until that finishes, so the whole batch is one screen. The last
outcome is also `lastAgentUpdate` in `/api/local/status`.

## 13. Service port (eth0)

Two NetworkManager
profiles on eth0, `musallahboard-lan` (DHCP client, priority 0,
`ipv4.dhcp-timeout 30`) tried first, then `musallahboard-service-port`
(`ipv4.method shared`, `10.77.0.1/24`, priority -999), whose autoconnect is
owned by `musallahboard-agent service-port on|off`. A board plugged into a real
network stays a client there; a laptop or phone plugged straight in gets an
address from the board within about a minute. Turn the service port off before
connecting a board to a network permanently.

## 14. Kiosk

- `kiosk-url` is `http://127.0.0.1:8080/` once enrolled (no query string; the
  page asks the agent who it is). Before enrollment it is absent and the
  `waiting.html` splash shows. The splash is embedded in the agent
  (`packaging/waiting.html`) and written by `musallahboard-agent splash
  install`, which setup.sh runs and the self-updater re-runs after each
  update; the agent pushes the address and its own version into it over CDP.
- `musallahboard-kiosk.service` is ordered `After=` and `Wants=`
  `musallahboard-agent.service`, and the agent reports `READY=1` only after the
  local server is listening, so Chromium never starts before the server.
- On an enrolled board the kiosk opens `starting.html` (a local page, embedded
  in the agent with `waiting.html`) with the board's URL in its fragment. It
  loads `/_mb/alive.gif` from the agent every 2 s and moves to the board once
  that answers, so Chromium never shows its own "refused to connect" page
  while the agent starts or restarts.
- The agent's unit never gives up restarting (`StartLimitIntervalSec=0`); a
  crash loop backs off from 5 s to 2 minutes (`RestartSteps`,
  `RestartMaxDelaySec`, systemd 254 and later).
- systemd's watchdog (`WatchdogSec=60s`) restarts an agent that is alive but
  stuck. The agent pings only while a heartbeat is fresh: before enrollment
  its poll loop bumps it, once enrolled a probe of its own
  `/api/local/status` every 10 s (the local server and the locks of the sync,
  backend, update and clock state). A heartbeat older than 60 s withholds
  the ping, so a stuck agent is restarted within about 2 minutes. The probe
  never waits on the network, so an offline board is not restarted for it;
  a stall in the network loops themselves is not detected.
- The daemon still watches for Chromium's error page and navigates back to the
  board, as a backstop.
- Every 30 s it also probes the page over CDP: `Inspector.enable` (which
  Chromium answers with `Inspector.targetCrashed` when the tab's renderer is
  gone, "Aw, Snap!") and a trivial `Runtime.evaluate` with a 5 s limit. A
  crashed page (one probe) or one that has not answered three probes in a row
  is reloaded; if it is still dead at the next threshold the kiosk unit is
  restarted (`sudo -n systemctl restart`, as the `kiosk.restart` command).
  Restarts are at least 5 minutes apart, doubling to an hour, and the backoff
  starts over only after an hour of a healthy page. A reload is tried at most
  once until the page answers again or the kiosk restarts. Nothing is done
  while the update screen is up, nor for a page that answers without the
  board's `getStatus()` hook (splash, update screen, an old board app), a page
  mid-navigation, or a browser that is not up yet.
- The heartbeat's `kioskAlive` comes from the same probe: false when the
  browser is reachable but the page crashed or does not answer; the unit's
  state is used only when the browser cannot be reached.

## 15. Frontend (MusallahBoard)

The board app only ever runs served by the agent; there is no hosted board.
The backend's live `GET /api/musallah/payload` has been removed: boards read
per-day payloads from installed content, and weather comes through the agent.

- API calls are same-origin to the agent. The device id comes from
  `/api/local/status`; `/api/local/events` drives in-place refreshes and app
  reloads.
- No content yet: a clear "waiting for content" screen that says whether the
  board is syncing (with the last error) or needs a USB stick / upload.
- Stale content: the "Content last updated ..." note when `staleDays > 0`.
- Diagnostics (Alt+Shift+F) show agent and app versions, content range and
  source, days remaining, sync status.
- Waiting software updates (`updates` in the status) add "Update available:
  ... will be installed at 11:00 PM tonight" to the ticker.
- The page fades in from black when it loads, finishing the dip to black the
  agent starts when it leaves the update screen (section 8).
- Development: `npm run dev` proxies `/api` and `/media` to an agent, for
  example a real board through `ssh -L 8080:127.0.0.1:8080 <board>`.
- Releases: `npm run package` builds and writes
  `musallahboard-app-<version>.mbu` signed with `MB_RELEASE_SIGNING_KEY`, plus
  `app-channel.json`.

## 16. Security summary

| Threat | Mitigation |
|---|---|
| Malicious content via any transport | Content must be signed by a pinned content key and bound to this device id. |
| Malicious code (SPA or agent) | Only the release key signs software; the backend never holds it. |
| Backend compromise | Limited to content on screens; cannot push code. Content key rotation via `previous-public-keys` and `trust` CLI. |
| Replay of old packages | Monotonic sequence/version in `state.json`. |
| Bad agent release | Self-update with a crash-safe journal and a probation (stays up; reconnects if it was connected), automatic rollback and a rejected-version list. |
| Daemon compromise escalating to root | Daemon is unprivileged (only `CAP_SYS_TIME`, `CAP_NET_BIND_SERVICE`); the root updater re-verifies from a private copy with root-owned keys. |
| Zip-slip, zip bombs, disk exhaustion | Strict entry names, declared sizes enforced while reading, global caps, one import at a time. |
| Hostile USB stick | Allow-listed filesystems, read-only `nosuid,nodev,noexec` mount, only `*.mbu` regular files copied, no network in the helper, signature checks as for any source. |
| Rogue DHCP on a building network | Service port only after a DHCP client attempt fails; off by default; `service-port off` before connecting. |
| Clock tampering | With NTP, nothing but NTP moves the clock, and a floor ahead of NTP time is lowered. Without NTP, the clock only moves forward to the floor automatically; uploader time only with a verified package and within 90 days past the floor, and never into the floor. Packages signed more than a day ahead of an NTP clock are refused. |
| DNS rebinding against the upload server | `Host` allow-list. |
| Enrollment or backend impersonated on the LAN | `https://` backend and `wss://` socket on the backend's host, enforced at enrollment and at every daemon start; plaintext only with the explicit `insecure_dev` opt-in (section 11). |

## 17. Out of scope

- Weather on boards without internet (online boards get it through the weather
  overlay in section 7).
- Automatic eth0 switching between service port and LAN.
- Release key rotation other than shipping a new agent with an added key.
