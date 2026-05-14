# musallahboard-agent

Go daemon that runs on the MusallahBoard Raspberry Pi kiosk devices. It connects to the MusallahBoard backend over an authenticated WebSocket and executes remote commands issued from the admin UI.

## Overview

Each kiosk Pi runs this agent as a systemd service. The agent:

1. **Enrolls** on first boot — generates an Ed25519 keypair, registers the device with the backend using a one-time admin token, and stores the resulting device ID and private key on disk.
2. **Connects** to the backend via a persistent, authenticated WebSocket. Identity is proved by signing a server-issued challenge with the device's private key.
3. **Executes commands** dispatched by the backend — browser reload, screenshot capture, kiosk restart, system reboot, log streaming, and config refresh.
4. **Heartbeats** on a configurable interval, sending telemetry (CPU, memory, disk, uptime) so the admin dashboard can monitor device health.
5. **Recovers** automatically — exponential backoff with jitter on connection loss, and a crash-counter safe mode that disables command execution after repeated failed starts.

## Commands

| Kind | What it does | Privilege required |
|---|---|---|
| `chrome.reload` | Hard-reloads the kiosk page via Chrome DevTools Protocol | None (CDP on localhost:9222) |
| `chrome.screenshot` | Captures a JPEG screenshot via CDP | None |
| `kiosk.restart` | Restarts the `musallahboard-kiosk.service` systemd unit | `sudo systemctl restart musallahboard-kiosk.service` |
| `system.reboot` | Schedules a system reboot (`shutdown -r +1`) | `sudo /sbin/shutdown -r +1` |
| `logs.tail` | Streams the last N lines of the agent journal | None |
| `config.refresh` | Re-reads the agent config file without restarting | None |

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
make install-local PI=admin@brothers-board.local
```

This cross-compiles for arm64, SCPs the binary to `/tmp/` on the Pi, installs it to `/usr/local/bin/`, and restarts `musallahboard-agent.service`.

## First-time Pi setup

`setup.sh` bootstraps a fresh Pi OS (Debian/trixie) installation into a locked-down kiosk. Run it as a user with sudo access:

```bash
bash setup.sh
```

It configures:

- **labwc** Wayland compositor with cursor hiding on boot
- **Chromium** in kiosk mode (reads URL from `~/url.txt`)
- **lightdm** auto-login into the kiosk user's labwc session
- **SSH hardening** — key-based auth only, kiosk user denied
- **Raspberry Pi Connect** for remote screen and shell access
- **Hardware watchdog**, persistent journal, idle prevention
- **WiFi power save disabled** (prevents overnight DHCP lease drops)
- **UFW firewall** and unattended security upgrades

After the script completes, run `agent enroll` (see below) to register the device with the backend.

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

## Safe mode

If the agent crashes more than 3 times consecutively, it enters **safe mode**: it still connects and sends heartbeats (so the admin UI shows the device as degraded), but refuses to execute commands. The crash counter resets after the agent has been running stably for 5 minutes.

## Development

```bash
make test   # run all tests
make tidy   # go mod tidy
make clean  # remove build/
```
