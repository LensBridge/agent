# MusallahBoard Agent

Go daemon-orchestrator for kiosk devices. Manages Chromium, telemetry, remote commands, and crash recovery.

## Overview

> [!Note]
> Documentation will lag behind development for now, until the agent is at a point where I'm happy with it. If you have a question, please open an issue

While working on MusallahBoard, it was quickly discovered that a basic websocket to a chromium instance would not suffice for a production kiosk. Thus, this agent was created
to manage the kiosk lifecycle, including:

1. Device Enrollment and Identity
2. Deep(er) Chromium and Wayland Control
3. Telemetry and Health Monitoring
4. Remote Command Execution, beyond what the browser can do (reboot, restart kiosk service, tail logs, etc.)

By running a daemon as a systemd service, we offload all complexity from the React frontend and keep the kiosk page as simple as possible. This allows the frontend to
be a memory-efficient React SPA rather than a full Electron app (Under 1Gb total memory usage, incl. OS) while the agent handles heavy lifting in the background.

## Quickstart

> [!Note]
> The agent is **not** a standalone application! It requires the [LensBridgeBackend](https://github.com/lensbridge/lensbridgebackend) and the [MusallahBoard Frontend](https://github.com/lensbridge/MusallahBoard) to function!

> [!CAUTION]
> Installing the agent on a device **will** make changes to the system, including creating users, hardening SSH, and installing a kiosk service. 
> Do **NOT** install the agent on a device you'd like to keep as-is!

> [!WARNING]
> This is your final warning to NOT install the agent on a device that will not act as a 24/7 kiosk! The agent, and related infrastructure, is NOT designed to run on a personal computer!

Installing the agent and setting up related MusallahBoard infrastructure is as simple as 

```bash
curl -fsSL https://raw.githubusercontent.com/lensbridge/agent/main/setup.sh | bash
```

Or clone the repo and run the script manually:

```bash
git clone https://github.com/LensBridge/agent.git
cd agent
./setup.sh        # as a user with sudo, not as root
```

Follow the prompts to set the device up (you need an admin username and your SSH public key; the board has no password login), then reboot. For unattended installs, pass them as `MB_ADMIN_USER=... MB_ADMIN_SSH_KEY="ssh-ed25519 ..."` in front of `bash`. Upon reboot, the device will be at a splash screen waiting for enrollment. Once the device has a network connection, that splash also prints its IP address on screen — use it to SSH in without hunting through DHCP leases.

To enroll the device, run:

```bash
sudo musallahboard-agent enroll --token=<token> --backend=<backend URL>
# Easiest: copy the whole command from the admin portal when you issue the token.
```

The device will then be enrolled and the board will load automatically once its first content and board app packages have synced. The device is now ready to be used as a kiosk!

## Building

A little unorthodox, but this go project uses a Makefile to build the agent binary. The Makefile has targets for both native and cross-compilation.

```bash
# Native (host arch)
make build

# Cross-compile for Raspberry Pi (arm64)
make build-arm64

# Laptop tool (Windows, macOS, Linux) and the package signer
make mbpush
make mbpack
```

Requires Go 1.24+. Binaries land in `build/`. A plain build is a dev build: it trusts no release key of its own (see Release signing below).

## How a board gets updates (v2)

Every board renders from its own disk: the kiosk always loads the agent's local server at `http://127.0.0.1:8080/`. Content (payloads and posters), the board app and the agent itself all arrive as signed `.mbu` packages, which the board checks against keys it has pinned before installing anything. The network only changes how fresh the board is. [docs/architecture.md](docs/architecture.md) is the full design and the contract with the backend, the frontend and the Android app.

### Online boards

Nothing to do. An enrolled board syncs its content from LensBridge every 30 minutes (and straight after an admin changes something), and follows the release channels on GitHub for new board app and agent versions. A new agent is installed by a small root updater that checks it again and rolls it back automatically if it does not start. If the internet drops, the board keeps showing the 7 days of content it already has.

### Boards without internet

Set the board up and enroll it online as usual, then turn on the ethernet service port:

```bash
sudo musallahboard-agent service-port on
```

If a DS3231 clock module is fitted, also re-run the setup once with `--rtc`, while the board has internet:

```bash
curl -fsSL https://raw.githubusercontent.com/lensbridge/agent/main/setup.sh | bash -s -- --service-port --rtc
```

The service port turns the ethernet port into a service port: a laptop or phone plugged straight into it gets an address from the board. Turn it off before plugging the board into a real network: `sudo musallahboard-agent service-port off`.

To update the board, get the packages first, while you have internet:

- content: LensBridge admin, Devices, the board, Download offline bundle (`musallahboard-content-<id>-<date>.mbu`, 14 days)
- board app and agent: `mbpush fetch` (saves the latest `musallahboard-app-*.mbu` and `musallahboard-agent-*-arm64.mbu`)

Then use any of:

1. **USB stick.** Copy the `.mbu` files to the stick (its root, or a `MusallahBoard/` folder) and plug it into the board. The screen shows progress, and says "You can remove the USB stick" as soon as it has copied what it needs, usually within seconds. One stick can carry content for several boards: each board takes only its own.
2. **Laptop.** Plug into the board's ethernet port and open `http://10.77.0.1/` in a browser, or run `mbpush <file.mbu>...`. `mbpush status` shows what is installed and how far the board's clock is off; an upload also sets the clock from the laptop's. For a board reachable only over SSH: `mbpush --ssh admin@host <file.mbu>...`.
3. **Android app.** Downloads the packages while it has signal, then uploads them over a USB ethernet adapter.

On the board itself: `sudo musallahboard-agent import <file.mbu>...` and `sudo musallahboard-agent status`.

### Release signing

Software (board app and agent) is signed with a release key that the backend never holds; content is signed by the backend's own key. To set up release signing once:

```bash
make mbpack && build/mbpack keygen    # prints a private seed, a public key and its key id
```

- The **private seed** goes in the repository secret `MB_RELEASE_SIGNING_KEY` (and the frontend repo's, for app packages). Keep an offline copy; never commit it.
- The **public key** goes in the repository variable `MB_RELEASE_PUBLIC_KEYS` (comma-separated, to allow a second key during a rotation). CI compiles it into the agent (`RELEASE_KEYS`), so every board trusts it without any configuration.

Pushing a tag (`0.3.0`) runs `.github/workflows/release.yml`: tests, builds, signs `musallahboard-agent-<version>-<arch>.mbu`, and publishes them with `agent-channel-<arch>.json`, the release tarballs and the `mbpush` binaries. Locally: `MB_RELEASE_SIGNING_KEY=<seed> make package-mbu VERSION=0.3.0 RELEASE_KEYS=<public key>`.

### Trust management

Keys live in `/etc/musallahboard/trust.json` (root-owned). Content keys are pinned at enrollment; `sudo musallahboard-agent trust fetch` re-reads them from the backend (for example after a key rotation). Release keys are compiled in and can be extended there.

```bash
sudo musallahboard-agent trust show
sudo musallahboard-agent trust add content|release <base64 public key>
sudo musallahboard-agent trust remove <key id>
sudo musallahboard-agent trust fetch
```

## Roadmap

Eventually I would like to create a premade Pi image (or an image builder) with the agent preinstalled, so that users can just flash an SD card and have a ready-to-go kiosk. Unfortunately, everything has been testing my patience lately and I gave up on that for now.
