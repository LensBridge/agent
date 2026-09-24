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
   agent that does not come up healthy is rolled back automatically.

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
| `createdAt` | RFC 3339 UTC, second precision. Used as a signed lower bound for the clock (section 10). |
| `files` | Array as above. `sha256` lowercase hex of the file bytes; `bytes` its exact size. Paths unique and valid per the entry-name rules. |

Per type:

**`content`** (signed by a *content* key)

- `deviceId`: required, the enrolled device id. Content is always device-bound.
- `sequence`: required, positive integer, strictly increasing per device. The
  backend uses the build time in epoch milliseconds (not truncated to seconds,
  so two builds in the same second still order).
- `content.timezone`: IANA zone, not empty, not `Local`, loadable on the board.
- `content.firstDay`, `content.lastDay`: `YYYY-MM-DD`, inclusive, first <= last,
  at most 31 days.
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
  github.com/LensBridge/agent/internal/trust.BuiltinReleaseKeys=<b64>[,<b64>]`)
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
  "update": { "active": false }
}
```

`app` and `content` are `null` when nothing is installed; `servingDay`,
`daysRemaining` and `staleDays` are then `null` and `today` is in the system
zone. `source` is one of `sync`, `usb`, `upload`, `cli`. `sync.lastError` is a
short human string or `null`. If installed content cannot be read, `content` is
`null` and an `error` string is added.

**`/api/local/events`** (SSE, `text/event-stream`, `no-store`):

- `event: content` with `data: {"sequence": n}` after new content is installed
  whose payloads differ from what was being served. The page re-fetches the
  payload in place.
- `event: app` with `data: {"version": "…"}` after a new app release is
  installed. The page reloads itself.
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
   older -> `rejected` ("older than what is installed"). An agent version in
   `agentVersionsRejected` is `rejected` ("failed to start last time").
3. **Install**:
   - content: move media into the store (verified hash, fsync), write the
     bundle directory, fsync, rename into `bundles/`, swap `current`, record the
     sequence, prune, emit `content` if payloads changed.
   - app: extract into `releases/.staging-*`, fsync, rename, swap `current`,
     record the version, prune, emit `app`.
   - agent: write the verified package to `agent/staged/package.mbu` and create
     `agent/staged/ready`. The root updater does the rest (section 12).
4. **Clock**: every verified package raises `clockFloor` to its `createdAt`
   (section 10).

A batch processes `agent` packages first, then `app`, then `content`. When an
agent package is staged, the rest of the batch stays in the inbox and is
processed by the new agent after the restart.

**Update screen.** For `usb`, `upload` and `cli` batches containing at least one
package that is not skipped, and for any app or agent install from any source,
the daemon navigates the kiosk (CDP) to `/_mb/updating` and drives it through
`window.mbUpdate` (`caption(text)`, `complete({seconds, headline, message, caption})`),
then navigates back to `/` after the countdown. Background content sync from
the backend is silent: an admin editing a poster must not put "Working on
updates" on every screen. On failure the screen shows
`complete({headline: "Update not installed", message: "Returning to MusallahBoard", caption: <reason>})`.

Results are JSON objects per package:

```json
{ "file": "musallahboard-app-2.1.0.mbu", "type": "app", "version": "2.1.0",
  "action": "installed", "message": "Installed board app 2.1.0" }
```

`action` is one of `installed`, `unchanged`, `staged` (agent, pending restart),
`skipped`, `rejected`.

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
start) two channel URLs, each returning:

```json
{ "version": "2.1.0", "url": "https://…/musallahboard-app-2.1.0.mbu", "sha256": "…", "bytes": 1234567 }
```

If `version` is newer than what is installed (and not rejected), it downloads
`url` (size-capped, sha256-checked) and imports it with source `sync`. The
channel file is only a pointer; the package's own signature and the monotonic
rules are what make it safe. Defaults:

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
| `POST /api/import` | `multipart/form-data`, one or more `package` parts (each at most 512 MiB, total at most 1 GiB). Optional header `X-MB-Client-Time: <unix seconds>`. Each part is streamed to the inbox, then processed as one batch (source `upload`). Response `200 {"results": [...], "clock": {"driftSeconds": n, "adjusted": bool, "note": "…"}}`. Only one import runs at a time; a second gets `409`. |

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
- The daemon shows the update screen while it works; the stick can be removed
  once the screen says "Update complete".
- `usb_import = false` in `agent.toml` disables it (the helper exits at once).

### 9.7 Admin CLI

`sudo musallahboard-agent import <file.mbu>... | -` copies into the inbox (`-`
reads one package from stdin) and waits for and prints the results.

## 10. Clock

Correct time decides which day's content is shown and the prayer times.

- `clockFloor` in `state.json` is the maximum of every verified package's
  `createdAt` (also kept as `signedClockFloor`) and the wall clock the daemon
  persists every 10 minutes. The persisted wall clock may not raise the floor
  more than 400 days past `signedClockFloor`, so a clock once set far into the
  future cannot lock the board there.
- At daemon start and every 10 minutes: if the clock is more than 60 s behind
  `clockFloor`, set it to `clockFloor` (the agent has `CAP_SYS_TIME`) and write
  the RTC when one is present. A signed `createdAt` in the future of the board
  is proof its clock is behind.
- An upload's `X-MB-Client-Time` (the uploader's clock when it started
  sending; the board measures the offset when the request arrives) is applied only after at least one package in
  that request verified, only when it differs by more than 5 s, and only within
  `[clockFloor, clockFloor + 90 days]`. The response reports the drift.
- A DS3231 RTC is still recommended (`setup.sh --rtc`), and online boards keep
  `systemd-timesyncd`.

## 11. Configuration (`/etc/musallahboard/agent.toml`)

Existing keys (`device_id`, `backend_url`, `websocket_url`, `key_path`) are
unchanged. New keys, all optional:

| Key | Default | Meaning |
|---|---|---|
| `service_port` | `false` | Run the eth0 service port and the upload server. Set by `musallahboard-agent service-port on|off`. |
| `usb_import` | `true` | Accept USB sticks. |
| `content_sync` | `true` | Sync content from the backend. |
| `content_days` | `7` | Days per synced content package (1-31). |
| `auto_update` | `true` | Follow the release channels. |
| `app_channel_url`, `agent_channel_url` | section 9.4 | Channel pointers. Empty disables that channel. |

An offline board needs no configuration change: sync attempts fail fast with no
route and cost nothing, and the moment it gets a network it starts syncing.

## 12. Agent self-update

1. The daemon verifies an agent package, writes it to
   `agent/staged/package.mbu` and creates `agent/staged/ready`.
2. `musallahboard-agent-update.path` (`PathExists=` on `ready`) starts
   `musallahboard-agent-update.service` (root, oneshot):
   `musallahboard-agent selfupdate apply`.
3. As root, the updater: removes `ready`; copies `package.mbu` into a
   root-only temporary directory (so the daemon cannot swap it mid-check);
   verifies it from scratch against root-trusted keys only; checks arch and
   that the version is newer than its own; extracts the binary to
   `/usr/bin/.musallahboard-agent.new`; runs `.new version` and requires the
   expected version; copies the current binary to
   `/usr/lib/musallahboard/agent.prev`; renames the new binary into place;
   restarts `musallahboard-agent.service`.
4. Health check: within 120 s, `GET http://127.0.0.1:8080/api/local/status`
   must report the new `agentVersion`. Otherwise the updater restores
   `agent.prev`, restarts the service, and adds the version to
   `agentVersionsRejected`, so a broken release cannot loop.
5. The outcome is written to `agent/last-update.json`. The new daemon, finding
   the kiosk on the update screen at startup, completes it and returns to the
   board.

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
  existing `waiting.html` splash shows.
- `musallahboard-kiosk.service` is ordered `After=` and `Wants=`
  `musallahboard-agent.service`, and the agent reports `READY=1` only after the
  local server is listening, so Chromium never starts before the server.
- The daemon still watches for Chromium's error page and navigates back to the
  board, as a backstop.

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
| Bad agent release | Health-checked self-update with automatic rollback and a rejected-version list. |
| Daemon compromise escalating to root | Daemon is unprivileged (only `CAP_SYS_TIME`, `CAP_NET_BIND_SERVICE`); the root updater re-verifies from a private copy with root-owned keys. |
| Zip-slip, zip bombs, disk exhaustion | Strict entry names, declared sizes enforced while reading, global caps, one import at a time. |
| Hostile USB stick | Allow-listed filesystems, read-only `nosuid,nodev,noexec` mount, only `*.mbu` regular files copied, no network in the helper, signature checks as for any source. |
| Rogue DHCP on a building network | Service port only after a DHCP client attempt fails; off by default; `service-port off` before connecting. |
| Clock tampering | Clock only moves forward to signed times automatically; uploader time only with a verified package and within 90 days of the floor. |
| DNS rebinding against the upload server | `Host` allow-list. |

## 17. Out of scope

- Weather on boards without internet (online boards get it through the weather
  overlay in section 7).
- Automatic eth0 switching between service port and LAN.
- Release key rotation other than shipping a new agent with an added key.
