# MusallahBoard Offline Mode

Plan and interface contract for running a board with no internet connection.
Every component below is built against this document. If code and this file
disagree, fix one of them deliberately — do not let them drift.

## Why

Some musallahs cannot give the board's Pi (a Raspberry Pi 4B) an internet
connection. Instead, an admin downloads a content bundle from LensBridge while
online, walks to the board, plugs a laptop into the Pi's ethernet port, and
pushes the bundle over SSH. Prayer times and the Hijri date are computed on the
board, so they stay correct indefinitely; only posters, events, weekly content
and the ticker need refreshing.

When a board later gets a network connection, one command switches it back to
online mode. Nothing is reinstalled and the device does not re-enroll.

## Design summary

| Concern | Decision |
|---|---|
| Transport format | A zip: a manifest, one fully-assembled payload per day, poster images. |
| Window | 14 days by default (1–31 allowed). Visit roughly every 7–10 days. |
| Payload assembly | The backend's existing `BoardPayloadAssembler`, run once per day with the clock set to that day. No frame logic is reimplemented anywhere else. |
| Local server | The existing Go agent grows an `offline` mode that serves the SPA, payloads and media on `127.0.0.1:8080`. There is no separate daemon. |
| Frontend | One build for both modes. Prayer times via `adhan`, Hijri date via `Intl`, fonts bundled — in **both** modes. `?mode=offline` in the kiosk URL only disables the refresh socket and enables the stale-content indicator. No service worker / PWA. |
| Updating | `mbpush` on a laptop, or the Android app, over SSH as the restricted `mbpush` account, whose only command is `musallahboard-agent gate` (see "Push account"). The bundle is streamed on stdin. No upload endpoint exists on the Pi. |
| Laptop ↔ Pi link | In offline mode eth0 falls back to a NetworkManager "shared" connection at `10.77.0.1/24`, which hands the laptop an address by DHCP. It is only tried after a plain DHCP-client profile has failed, so a board plugged into a real network stays a client there. |
| Clock | A DS3231 RTC module per board. Every `mbpush` also corrects the Pi's clock from the laptop's. |
| Identity | Every board enrolls online first, exactly as today. Offline mode keeps the enrolled identity. |

## The update visit, end to end

1. **Before the visit (online, anywhere).** Enter the next two weeks of content
   in LensBridge. Then Devices → the board → **Download offline bundle**, which
   saves `musallahboard-<id8>-<firstDay>.zip`.
2. **At the board.** Ethernet cable from the laptop to the Pi, then:
   ```
   mbpush musallahboard-3f2a1b4c-2026-09-24.zip
   ```
   `mbpush`, over `ssh mbpush@10.77.0.1`, sends gate requests:
   1. `clock` reads the Pi's clock; if it is more than 5 s off the laptop's,
      `clock-set <epoch>` sets it and saves it to the RTC, and the drift is
      reported;
   2. `bundle-install` with the zip on stdin validates and installs it;
   3. `status` prints the result.
3. Unplug. The new content is on screen within seconds (the install reloads
   Chromium over CDP).

Failure at any step leaves the previously installed bundle serving. Nothing on
screen changes until a bundle passes every check.

---

## Contract 1 — the bundle zip

Produced by the backend, consumed by `musallahboard-agent bundle install`.

```
manifest.json
payloads/2026-09-24.json
payloads/2026-09-25.json
...
payloads/2026-10-07.json
media/<sha256>.<ext>
...
```

No other entries are allowed. Entry names are exactly as above: no leading
`/`, no `..`, no directories other than `payloads/` and `media/`.

### `manifest.json`

```json
{
  "formatVersion": 1,
  "deviceId": "3f2a1b4c-....",
  "timezone": "America/Toronto",
  "generatedAt": "2026-09-24T14:02:11Z",
  "firstDay": "2026-09-24",
  "lastDay": "2026-10-07",
  "media": [
    { "path": "media/9b1c...e4.jpg", "sha256": "9b1c...e4", "bytes": 482113, "contentType": "image/jpeg" }
  ]
}
```

- `formatVersion` — integer. Consumers reject any value they do not support.
  Only `1` exists.
- `timezone` — the device's resolved zone (`BoardContext.zone()`), IANA id.
  "Today" on the board is always evaluated in this zone.
- `firstDay` / `lastDay` — ISO dates, inclusive. There is exactly one payload
  file for every date in this range and no others.
- `media` — every file under `media/`, with its SHA-256 (lowercase hex, which is
  also the file's base name), size and content type.

### `payloads/<date>.json`

Exactly the JSON that `GET /api/musallah/payload?deviceId=<id>` would return
(`MusallahBoardPayload`) if requested at the **start of that day** in the
device's timezone, with these differences:

- `weather` is `null` (the frontend already hides the weather chip).
- Posters are those active **at any point during that day** in the device's
  timezone (`startTime < dayEnd && endTime > dayStart`), not those active at the
  instant of assembly.
- Every poster frame's `frameConfig.posterUrl` is rewritten to
  `/media/<sha256>.<ext>` and that file is in the zip. Identical images are
  stored once.

### Backend endpoint

```
GET /api/admin/board/devices/{deviceId}/offline-bundle?days=14
Authority: BOARD_DEVICE_READ
200 application/zip
Content-Disposition: attachment; filename="musallahboard-<first 8 chars of deviceId>-<firstDay>.zip"
```

- `days`: default 14, allowed 1–31, else `400`.
- Unknown device: `404`. Revoked device: `409`.
- `firstDay` is today in the device's timezone.
- If any poster image cannot be fetched, the export fails with `502` naming the
  poster. A bundle is never silently missing content.

---

## Contract 2 — the agent in offline mode

### Config

`/etc/musallahboard/agent.toml` gains one key:

```toml
mode = "offline"   # or "online"; absent means "online"
```

Everything else about enrollment and identity is unchanged.

### Filesystem

| Path | Contents |
|---|---|
| `/var/lib/musallahboard/offline/bundles/<generatedAt, compact>/` | an extracted, validated bundle |
| `/var/lib/musallahboard/offline/current` | symlink to the bundle being served |
| `/usr/share/musallahboard/board/` | the built frontend SPA (`index.html`, `assets/`, …) |

The install keeps the current and previous bundle, and deletes older ones.

### Behaviour by mode

| | `online` | `offline` |
|---|---|---|
| Backend WebSocket, telemetry, remote commands | started, as today | **not started**; the agent makes no network calls |
| Local HTTP server | none | `127.0.0.1:8080` |
| kiosk-url | `<board-url>?deviceId=<id>`, as today | `http://127.0.0.1:8080/?deviceId=<id>&mode=offline` |
| eth0 | whatever the OS does (`musallahboard-lan`, a DHCP client, if provisioned) | `musallahboard-lan` first; `musallahboard-service-port` when that fails (see `mode` below) |

Watchdog, safe mode and sd_notify are shared by both modes.

### Local HTTP server (offline mode only)

Bound to `127.0.0.1:8080` only. Read-only.

| Method | Path | Behaviour |
|---|---|---|
| GET | `/api/musallah/payload?deviceId=<id>` | Today's payload file, verbatim. `404` if `deviceId` does not match the bundle's. `503` `{"message":"no bundle installed"}` if there is none. `Cache-Control: no-store`. |
| GET | `/api/local/status` | Status JSON, below. `Cache-Control: no-store`. |
| GET | `/media/<file>` | From the current bundle. `Cache-Control: public, max-age=31536000, immutable`. |
| GET | `/api/*` (anything else) | `404` JSON. Never the SPA fallback: the page parses these as JSON. |
| GET | anything else | Static file from the SPA directory; unknown paths fall back to `index.html`. `index.html` is `no-cache`; `/assets/*` is immutable. A missing `/assets/*` file is a `404`, not `index.html` (it is a stale hashed URL). `503` if no SPA is installed. |

**Picking today's payload.** `today` = the current date in the manifest's
`timezone`. If `today < firstDay`, serve `firstDay`. If `today > lastDay`, serve
`lastDay`. The current bundle is re-read (symlink resolved) at least once a
minute or on every request, so a newly installed bundle is picked up without a
restart.

**`/api/local/status`:**

```json
{
  "mode": "offline",
  "deviceId": "3f2a1b4c-....",
  "bundle": {
    "firstDay": "2026-09-24",
    "lastDay": "2026-10-07",
    "generatedAt": "2026-09-24T14:02:11Z",
    "timezone": "America/Toronto"
  },
  "today": "2026-09-30",
  "servingDay": "2026-09-30",
  "daysRemaining": 7,
  "staleDays": 0
}
```

`bundle` is `null` when nothing is installed; `servingDay`, `daysRemaining` and
`staleDays` are then `null` too, and `today` is in the system timezone.
`daysRemaining` = `max(0, lastDay − today)` (0 on the last day).
`staleDays` = `max(0, today − lastDay)`. If a bundle is installed but cannot be
read, `bundle` is `null` and an extra `error` string says why.

### CLI subcommands

All of these print plain English for a human at a terminal. Any that change
the system require root and say so if run without it.

**`musallahboard-agent bundle install <zip>`**

1. Reject if the zip is over 200 MB.
2. Validate, without writing anything outside a staging directory:
   - every entry name is allowed (see Contract 1), so zip-slip is impossible;
   - `manifest.json` parses, `formatVersion == 1`;
   - `deviceId` equals this device's enrolled id;
   - `timezone` loads;
   - a payload exists for every date `firstDay..lastDay` and nothing else, and
     each parses as a JSON object;
   - every manifest media entry exists, matches its size and SHA-256, and every
     `/media/...` `posterUrl` in every payload refers to one of them;
   - no non-empty `posterUrl` points anywhere other than `/media/` (Contract 1
     has the exporter rewrite them all; one that still points at the internet
     would be a blank tile on the board);
   - the range is at most 31 days, and the media at most 512 MB uncompressed.
3. Extract to `bundles/.staging-<random>`, fsync, rename to
   `bundles/<generatedAt compact>`.
4. Atomically replace `current` (create `current.tmp` symlink, rename over).
5. Prune to current + previous.
6. If the mode is offline and Chromium's CDP answers on `127.0.0.1:9222`,
   reload the page. Failure to reload is a warning, not an error: the board
   polls every 10 minutes anyway.
7. Print the resulting status.

Installing while in online mode is allowed (pre-staging before a switch) and
says so.

**`musallahboard-agent status [--json]`** — mode, device, bundle range, days
remaining / stale days, the system clock with its timezone, whether an RTC is
present (`/dev/rtc0`). `--json` prints the same object as `/api/local/status`
plus `clock` and `rtc`.

**`musallahboard-agent mode online|offline [--force]`**

- Writes `mode` to `agent.toml` (atomically, like `config.Save`).
- `offline`: sets `musallahboard-service-port` to autoconnect (at priority
  −999, the lowest NetworkManager has), gives `musallahboard-lan` a single
  autoconnect retry, and brings the service port up — **unless eth0 is in use**:
  another NetworkManager connection is active on it, or the IPv4 default route
  goes through it. Then it still writes the mode and enables autoconnect, but
  does not start the service port (that would kill the board's connection and
  run a DHCP server on that network), says so, and exits 0. `--force` skips
  the check. Warns if no bundle is installed.
- `online`: disables the service port's autoconnect, restores
  `musallahboard-lan`'s default retries, and brings the service port down.
  Switch to online **before** plugging the board into a real network: in
  offline mode the protection below applies, but it is not absolute.
- Boot-time protection. eth0 has two profiles, tried in priority order:
  `musallahboard-lan` (DHCP client, priority 0, `ipv4.dhcp-timeout 30`), then
  `musallahboard-service-port` (shared, priority −999). On a real network the
  first gets a lease and the board is only a client there. With a laptop
  plugged in (no DHCP server), it fails after 30 s and, with one retry,
  NetworkManager falls back to the service port — so allow up to a minute
  after plugging in. The remaining exposure is a network whose DHCP server
  does not answer within 30 s; then the board would start serving DHCP there.
  If the board has its own saved wired profile instead of `musallahboard-lan`,
  that profile is left alone and tried first, with NetworkManager's default
  retries (so fallback to the service port can take a few minutes).
- Restarts `musallahboard-agent.service`, which rewrites kiosk-url; the
  existing `.path` watcher bounces the kiosk.
- If run over the ethernet SSH session, switching to online drops the session.
  Print a warning before doing it.

**`musallahboard-agent app install <dir-or-tar.gz>`** — installs a new SPA
build into `/usr/share/musallahboard/board/` with the same validate / stage /
atomic-swap approach (the directory must contain `index.html`), then reloads
Chromium.

### Provisioning — `setup.sh --offline`

Run **while the Pi still has internet**, after a normal online setup and
enrollment. It:

- installs `dnsmasq-base` (used by NetworkManager's shared mode) and
  `util-linux-extra` (`hwclock`);
- creates `/var/lib/musallahboard/offline` (`root:musallahdaemon`, 0750: root
  installs bundles, the agent reads them);
- creates the NetworkManager connection `musallahboard-lan` (eth0, DHCP client,
  priority 0, `ipv4.dhcp-timeout 30`), unless the board already has a saved
  wired profile of its own;
- creates the NetworkManager connection `musallahboard-service-port`: eth0,
  `ipv4.method shared`, `ipv4.addresses 10.77.0.1/24`, IPv6 disabled,
  priority −999, autoconnect off (the `mode` command owns autoconnect);
- allows DHCP (67/udp) and DNS (53) in on eth0 in ufw, alongside the existing
  SSH rule;
- with `--rtc`: adds `dtoverlay=i2c-rtc,ds3231` to `/boot/firmware/config.txt`,
  removes `fake-hwclock`, and installs a udev rule + `musallahboard-rtc.service`
  that sets the system clock from `/dev/rtc0` when it appears at boot (the
  ds3231 driver is a module, so the kernel's own boot-time RTC read has
  already happened) — only once an RTC is fitted;
- installs the SPA from `--board-dist=<dir-or-tar.gz>` into
  `/usr/share/musallahboard/board/`;
- sets up the `mbpush` push account (next section), allowing each
  `--push-key="ssh-ed25519 AAAA... name"` given (repeatable);
- runs `musallahboard-agent mode offline`. If the board is on the network
  through eth0 at the time (the usual case when converting a board), that
  leaves the service port stopped and says so; it starts by itself when a
  laptop is plugged in later.

It is idempotent: re-running it is safe, and is how you add push keys later.
Packages are only fetched if missing, so a re-run works on a board that has no
internet any more.

### Push account

`mbpush` and the Android app log in as `mbpush`, not the admin. A leaked push
key can install a bundle (which must still pass every check, including the
device id), read status and set the clock within 2026–2100. It cannot open a
shell, read files, forward ports or run anything else as root. The admin
account is unchanged and stays the way to administer the board.

What `setup.sh --offline` sets up:

| Piece | Content |
|---|---|
| user | `mbpush`, system account, shell `/bin/sh` (sshd runs the forced command through it), password `*` (no password matches; not "locked", which would make sshd refuse its keys) |
| `/home/mbpush/.ssh/authorized_keys` | owned by root, one line per `--push-key`: `restrict,command="sudo -n /usr/bin/musallahboard-agent gate" <key>` |
| `/etc/sudoers.d/musallahboard-push` | `Defaults:mbpush env_keep += "SSH_ORIGINAL_COMMAND"` and `mbpush ALL=(root) NOPASSWD: /usr/bin/musallahboard-agent gate` |
| `/etc/ssh/sshd_config.d/99-musallahboard-push.conf` | `Match User mbpush`: `ForceCommand` the same gate command, no TTY, no forwarding of any kind, no password auth |
| `kiosk-hardening.conf` | `mbpush` appended to the existing `AllowUsers` line |

The forced command is set twice, per key and per user, so a key added to
`authorized_keys` by hand without the option is still confined. Before sshd is
reloaded, setup compares the admin account's effective sshd settings
(`sshd -T -C user=<admin>,...`) before and after the change, and checks that
`mbpush` gets the ForceCommand. Any difference, or a config `sshd -t` rejects,
restores the previous files and stops. A mistake here cannot lock the admin
out.

To revoke a key, delete its line from `/home/mbpush/.ssh/authorized_keys`.

**The gate** (`musallahboard-agent gate`, `cmd/agent/gate.go`) reads the request
from its arguments or, when run as the forced command, from
`SSH_ORIGINAL_COMMAND`. It splits it on whitespace, strips an optional
`sudo -n musallahboard-agent gate` prefix, and matches token for token. Nothing
reaches a shell.

| Request | Does |
|---|---|
| `status`, `status --json` | as `musallahboard-agent status` |
| `clock` | prints the board's clock as Unix seconds |
| `clock-set <seconds>` | plain decimal only, 2026–2100; sets the clock and saves it to the RTC (prints a `Warning:` line if there is none) |
| `bundle-install` | the zip on stdin (≤ 200 MB) → a root-only temp file → the normal `bundle install`, then the temp file is removed |
| `app-install` | a `.tar.gz` of the build on stdin (≤ 200 MB) → the normal `app install` |

Anything else is refused with exit status 126. Clients always send
`sudo -n musallahboard-agent gate <request>`: for `mbpush` the forced command
takes over and reads it back, for the admin it simply runs, so one client
works against either account.

### Converting an existing online board

1. Fit the DS3231 RTC (power off first).
2. Update the agent binary to one with offline mode:
   `make deploy PI=<admin>@<board-ip>` (which runs `packaging/update.sh`).
3. While the board is still online, on the board:
   `bash setup.sh --offline --rtc --board-dist=<dist-dir-or-tar.gz> --push-key="<your public key>"`
   (copy the frontend build over first; `setup.sh` is in the release tarball).
   Because eth0 is on the network, the service port is left stopped.
4. Install the first bundle over the current network:
   `mbpush --host <current ip> musallahboard-<id8>-<date>.zip`.
   This also sets the clock and saves it to the RTC.
5. Reboot. On the same network the board comes back as a DHCP client
   (`musallahboard-lan`), still offline mode.
6. Verify: `sudo musallahboard-agent status` shows mode offline, the bundle and
   `RTC: present`; `ls /dev/rtc0` exists.
7. Move the board to the musallah. At the next visit, plug the laptop into its
   ethernet port and use `mbpush` with the default host.

### `mbpush` (on the admin's laptop)

A Go binary in the agent repo (`cmd/mbpush`), cross-compiled for Windows,
macOS and Linux. It shells out to the system `ssh` so the user's existing keys
and agent just work (Windows 10+ ships it), and speaks only gate requests, with
files streamed on stdin. The Android app (`MusallahBoard/mbpush-android`) sends
the same requests.

```
mbpush <bundle.zip>                    # clock sync, send + install, status
mbpush --app <dist-dir-or-tar.gz>      # update the SPA
mbpush status                          # clock check + status, changes nothing
Flags: --host (default 10.77.0.1)  --user (default mbpush; an admin account
       works too)  -i <identity file>  --check-host-key
```

Every board answers on 10.77.0.1 with its own host key, so on that address
`mbpush` does not record or check host keys (otherwise the second board visited
fails with "REMOTE HOST IDENTIFICATION HAS CHANGED"). The link is a cable from
the laptop into the board. Any other `--host`, or `--check-host-key`, keeps
ssh's normal checking. Requests use `sudo -n`, so an account without the
sudo rule fails immediately rather than hanging on a password prompt.

Clock sync: compare the gate's `clock` with the laptop's; if more than 5 s
apart, `clock-set <laptop epoch>` (no RTC fitted is a warning, not a failure).
Always print the measured drift.

On any failure it says what failed in plain English, and, when the install
step was reached and failed, that the board is still showing its previous
content.

---

## Contract 3 — the frontend

One build serves both modes.

**Both modes:**

- Prayer times from the `adhan` package, computed for the board's date in the
  board's timezone, replacing the Aladhan API. Method parameters must match
  Aladhan's definitions for each `CalculationMethod` value the backend can send
  (`ALADHAN_METHOD` keys), including the Hanafi Asr alongside the standard one.
  Verified against Aladhan for a real location over at least one month.
- Hijri date from `Intl.DateTimeFormat` with the `islamic-umalqura` calendar,
  for the board's date in the board's timezone, mapped to the existing
  `{ day, monthEn, monthAr, year }` shape.
- Fonts (Cinzel, Montserrat incl. italic, Amiri) bundled via `@fontsource`
  packages. No request to Google Fonts.
- After this, the only network requests are to the payload API origin.

**When the page URL has `mode=offline`:**

- No refresh WebSocket.
- Payload polled every 10 minutes from same-origin `/api/musallah/payload`, as
  today, plus `/api/local/status`.
- If `staleDays > 0`, a small, unobtrusive "Content last updated <date>"
  indicator. The diagnostics panel (Alt+Shift+F) shows mode, bundle range and
  days remaining.

## Out of scope for v1

- Updating the agent binary through `mbpush` (use the existing update path
  over SSH by hand).
- Any automatic online/offline fallback. Mode is set explicitly.
- Weather offline.
- A web upload page on the Pi.
