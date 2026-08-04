# musallahboard-agent

Go daemon that runs on MusallahBoard kiosk devices. Connects to MusallahBoard backend over an authenticated WebSocket to execute commands, stream logs, and report telemetry.

## Overview

Each kiosk runs this agent as a systemd service. The agent:

1. **Enrolls** on first boot — generates an Ed25519 keypair, registers the device with the backend using a one-time admin token, and stores the resulting device ID and private key on disk.
2. **Connects** to the backend via a persistent, authenticated WebSocket. Identity is proved by signing a server-issued challenge with the device's private key.
3. **Executes commands** dispatched by the backend — browser reload, screenshot capture, kiosk restart, system reboot, log streaming, and config refresh.
4. **Heartbeats** on a server-dictated interval, sending telemetry (CPU, memory, disk, uptime, agent version, which frame is on screen) so the admin dashboard can monitor device health.
5. **Recovers** automatically — exponential backoff with jitter on connection loss, and a crash-counter safe mode that disables command execution after repeated failed starts.

## Commands

| Kind | What it does | Privilege required |
|---|---|---|
| `chrome.reload` | Hard-reloads the kiosk page via Chrome DevTools Protocol | None (CDP on localhost:9222) |
| `chrome.screenshot` | Captures a PNG screenshot via CDP | None |
| `kiosk.restart` | Restarts the `musallahboard-kiosk.service` systemd unit | `sudo systemctl restart musallahboard-kiosk.service` |
| `system.reboot` | Reboots the device after a short delay, so the result frame lands first | `sudo systemctl reboot` |
| `logs.tail` | Streams the last N lines of the agent journal | None |
| `config.refresh` | Re-composes `/etc/musallahboard/kiosk-url`, then asks the page to refetch in place via `window.MusallahBoard.setDeviceId()` + `.refresh()` | None |

Privileged commands use `sudo -n` — the exact invocations are allow-listed in `packaging/sudoers.d/musallahboard-agent`.

## Architecture

```
Backend
  └── WebSocket (/api/agent/ws)
        │
        ▼
   wsclient.Client          ← manages dial / auth / reconnect / heartbeat
        │
        ▼
   commands.Dispatcher      ← dedupes by commandId, applies deadline
        │
        ▼
   commands.Registry        ← routes Kind → Handler
        │
        ├── chrome.reload / chrome.screenshot  (cdp.Client → localhost:9222)
        ├── kiosk.restart                       (sudo systemctl …)
        ├── system.reboot                       (sudo shutdown …)
        ├── logs.tail                           (journalctl)
        └── config.refresh                      (config.Load)
```

## Building

```bash
# Native (host arch)
make build

# Cross-compile for Raspberry Pi (arm64)
make build-arm64
```

Requires Go 1.23+. Binaries land in `build/`.

## Deploying to a Pi

Copy the binary and restart the service in one step:

```bash
make deploy PI=admin@brothers-board.local
```

This cross-compiles for arm64, SCPs the binary to `/tmp/` on the Pi, installs it to `/usr/bin/`, and restarts `musallahboard-agent.service`.

## First-time host setup

One script gets the device fully provisioned and **ready for enrollment**;
the only remaining manual step is the one-time `enroll`. Build the binary
first (`make build-arm64`), then run the wrapper from a full repo checkout as
a user with sudo access:

```bash
bash setup.sh          # Raspberry Pi OS (Debian/trixie), arm64
```

The wrapper hardens the OS, creates the users, **then runs
`packaging/install.sh`** for you (agent binary, cage kiosk unit, systemd
service — everything except enrollment). If no built binary is found it skips
that step and prints the manual `install.sh` command instead. Platform-neutral
logic (config prompts, users, SSH hardening, journal, UFW, …) lives in
`packaging/lib/common.sh`; `setup.sh` only declares the Pi-specific parts —
deb `chromium`, rpi-connect, hardware watchdog, WiFi power-save fix, and a
required SSH key.

> **x86-64 hosts** are no longer provisioned by a script here. The
> `image/` directory builds a Debian 13 `qcow2` with Packer that runs the same
> `packaging/install.sh` and boots straight to the kiosk — use that for VM and
> e2e testing. See `image/README.md`.

Common behaviour:

- **Locked-down kiosk user** — password locked, no sudo, no SSH, no shell.
- **SSH hardening** — admin user only, kiosk user denied.
- **tty1 autologin removed**, persistent size-capped journal, idle/power
  ignored, UFW deny-inbound-except-SSH, unattended security upgrades.
- **Software-render fallback** — `/etc/musallahboard/kiosk.env` is written
  automatically when `systemd-detect-virt` reports a VM (and removed on bare
  metal), so cage falls back to pixman where the virtual GPU can't scan out
  GBM buffers. No manual per-platform toggle.
- **Board URL persisted** to `/etc/musallahboard/board-url`; the agent
  composes `<board-url>?deviceId=<uuid>` → `/etc/musallahboard/kiosk-url`
  after enrollment. Change the board via `/etc/musallahboard/board-url` +
  restart the agent.

`install.sh --kiosk` installs **cage** as `musallahboard-kiosk.service` — a
system unit, no display manager. The browser runs in kiosk mode and the unit
blocks on `/etc/musallahboard/kiosk-url` (a local "waiting" splash shows until
the agent enrolls), so the board never loads without a device id.

Making the box *boot* into it — `set-default multi-user.target` plus disabling
any display manager — is `packaging/appliance-policy.sh`, run separately.
`install.sh` is payload only (files, accounts, unit enablement) so that it can
become the `.deb`'s postinst unchanged: installing a package must not repoint
somebody's boot target. `setup.sh` runs both, in that order.

When setup finishes the device is at the splash, waiting. Finish it with the
one-time enroll (see Enrollment below) — the board then loads automatically,
no reboot needed.

## Clock

`setup.sh` sets the timezone and blocks until `systemd-timesyncd` reports a real
sync, because a Pi has no battery-backed RTC and two things break on a wrong
clock without saying so:

- Prayer times are computed against local midnight.
- The backend rejects a handshake whose signature timestamp is more than five
  minutes out, so the agent logs `auth_failed` forever.

The agent compares its clock against the server's `hello` frame on every connect
and logs an error naming NTP when they disagree by more than 30 seconds. If a
device won't authenticate, check `timedatectl status` first.

## Enrollment

On first boot, enroll the device from the Pi:

```bash
sudo musallahboard-agent enroll \
  --backend https://api.utmmsa.ca \
  --token <one-time-token-from-admin-ui>
```

This generates an Ed25519 keypair, registers with the backend, and writes the config to `/etc/musallahboard/agent.toml`. The private key is stored at `/etc/musallahboard/agent.key` and never leaves the device.

## Configuration

Config file: `/etc/musallahboard/agent.toml`

```toml
device_id     = "<uuid assigned at enrollment>"
backend_url   = "https://api.utmmsa.ca"
websocket_url = "wss://api.utmmsa.ca/api/agent/ws"
key_path      = "/etc/musallahboard/agent.key"
```

`websocket_url` is whatever the backend returned at enrollment (from its
`musallahboard.agent.websocketUrl` property) and is never re-fetched. If the
backend was misconfigured when a device enrolled, that device keeps dialling
the wrong address forever — but this is an ordinary config fix, not a
re-enrollment:

```bash
sudo nano /etc/musallahboard/agent.toml     # correct websocket_url
sudo systemctl restart musallahboard-agent
```

Re-running `enroll` would mint a *new* device row and orphan the existing one's
history, so only do that if the Ed25519 key is also gone.

## Safe mode

If the agent crashes more than 3 times consecutively, it sets a **safe mode** flag: the startup log says so and the counter resets after 5 minutes of stable running.

**Not yet enforced.** The flag is computed but nothing consults it — commands still execute, and it is not reported to the backend, so the admin UI cannot show the device as degraded. Wiring both ends is outstanding.

## Development

```bash
make test   # run all tests
make tidy   # go mod tidy
make clean  # remove build/
```
