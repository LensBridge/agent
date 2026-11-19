# MusallahBoard Agent

The device agent for MusallahBoard kiosk Pis. It serves the board from the Pi's own disk, installs signed update packages from USB sticks, laptops and the network, keeps the clock and the kiosk healthy, and, optionally, connects the board to LensBridge for telemetry and remote commands. A board works fully without a network.

## Overview

> [!Note]
> Documentation will lag behind development for now, until the agent is at a point where I'm happy with it. If you have a question, please open an issue

While working on MusallahBoard, it was quickly discovered that a basic websocket to a chromium instance would not suffice for a production kiosk. Thus, this agent was created
to manage the kiosk lifecycle, including:

1. The board's identity, from a signed setup file
2. Signed content, board app and agent updates, by USB stick or network
3. Deep(er) Chromium and Wayland control
4. Optionally: telemetry and remote commands (reboot, restart the kiosk, tail logs, etc.)

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

### 1. Install

On a fresh Raspberry Pi OS Lite (trixie, arm64), as a user with sudo (not as root), while the Pi has internet:

```bash
curl -fsSL https://raw.githubusercontent.com/lensbridge/agent/main/setup.sh | bash
```

It asks for a hostname, an admin username and that admin's SSH public key (the board has no password login; SSH is the maintainer's way in), the time zone, and **LensBridge's content signing public key** (base64, shown in the console; the backend also publishes it at `/api/agent/signing-keys`). The board accepts content and setup files signed by that key only. For an unattended install pass the answers in front of `bash`: `MB_ADMIN_USER=... MB_ADMIN_SSH_KEY="ssh-ed25519 ..." MB_CONTENT_KEY="<base64>"`. Re-running it is safe. Then reboot.

The board now shows **"This board is not set up yet. Plug in its setup USB stick."**, with its address for SSH.

### 2. Plug in the setup stick

In the LensBridge console, add the board (Devices, Add board), then export its setup file or a USB kit (one stick for several boards: setup files, content, the board app and the agent). Copy the files to a USB stick, at its top level or in a `MusallahBoard/` folder, and plug it in. The board takes its identity from its setup file and then everything else on the stick that is for it, and shows "You can remove the USB stick". It needs no network, now or later.

If the setup file was exported for an online board, the board also connects to LensBridge whenever it has internet: it binds its own device key to the board in the console (a one-time claim) and from then on syncs content and takes remote commands. A maintainer can instead enroll with a token from the console:

```bash
sudo musallahboard-agent enroll --token=<token> --backend=<backend URL>
```

The backend URL must be `https://`, and the WebSocket address it hands back must be `wss://` on the same host; the agent refuses anything else.

### Development boards on plain http

For a backend running on your own machine without TLS, add `--insecure-dev` to the enroll command. It is saved in `/etc/musallahboard/agent.toml` as `insecure_dev = true`, and the agent logs a warning at every start while it is on: anyone on that board's network can impersonate the backend. Never use it on a deployed board. A board whose config is not safe to use keeps showing the content it has but does not talk to the backend; `musallahboard-agent status` and the journal say why.

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

## A test board without a Pi

`vm/mbvm` runs a board in a QEMU virtual machine on macOS or Linux. The VM is
provisioned by `setup.sh`, persists between runs, and can be reset to any
agent release (or a build of this checkout):

```bash
MBVM_CONTENT_KEY=<base64> vm/mbvm create
vm/mbvm usb setup-xxxxxxxx.mbu content-xxxxxxxx.mbu
vm/mbvm reset --agent=0.2.1
```

See [vm/README.md](vm/README.md).

## How a board gets updates

Every board renders from its own disk: the kiosk always loads the agent's local server at `http://127.0.0.1:8080/`. Its identity (setup file), content (payloads and posters), the board app and the agent itself all arrive as signed `.mbu` packages, which the board checks against keys it has pinned before installing anything. The network only changes how fresh the board is. [docs/architecture.md](docs/architecture.md) is the full design and the contract with the backend, the board app and the Android app.

- **USB stick.** Copy `.mbu` files from the console (setup file, content, or a whole USB kit) to a stick and plug it in. One stick can carry packages for many boards: each takes only its own, and only what is newer. Content covers up to 92 days.
- **Release channels.** Any board with internet follows the GitHub release channels for new board app and agent versions (no enrollment needed) and installs them overnight. A new agent is installed by a small root updater that checks it again and rolls it back if it does not stay up.
- **Online boards** (claimed or enrolled) also sync their content from LensBridge every 30 minutes and soon after an admin changes something.
- **Laptop or phone** on the ethernet service port (`sudo musallahboard-agent service-port on`): open `http://10.77.0.1/`, or `mbpush <file.mbu>...`. An upload also sets the board's clock from the laptop's. Turn it off before plugging the board into a real network. A DS3231 clock module: `bash setup.sh --service-port --rtc`.
- **On the board:** `sudo musallahboard-agent import <file.mbu>...` and `musallahboard-agent status`.

### Release signing

Software (board app and agent) is signed with a release key that the backend never holds; content is signed by the backend's own key. To set up release signing once:

```bash
make mbpack && build/mbpack keygen    # prints a private seed, a public key and its key id
```

- The **private seed** goes in the repository secret `MB_RELEASE_SIGNING_KEY` (and the frontend repo's, for app packages). Keep an offline copy; never commit it.
- The **public key** goes in the repository variable `MB_RELEASE_PUBLIC_KEYS` (comma-separated, to allow a second key during a rotation). CI compiles it into the agent (`RELEASE_KEYS`), so every board trusts it without any configuration.

Pushing a tag (`0.3.0`) runs `.github/workflows/release.yml`: tests, builds, signs `musallahboard-agent-<version>-<arch>.mbu`, and publishes them with `agent-channel-<arch>.json`, the raw agent binaries setup.sh installs, and the `mbpush` binaries. Locally: `MB_RELEASE_SIGNING_KEY=<seed> make package-mbu VERSION=0.3.0 RELEASE_KEYS=<public key>`.

### Trust management

Keys live in `/etc/musallahboard/trust.json` (root-owned). The content key is pinned by `setup.sh`; `sudo musallahboard-agent trust fetch` re-reads the backend's (for example after a key rotation). Release keys are compiled in and can be extended there.

```bash
sudo musallahboard-agent trust show
sudo musallahboard-agent trust add content|release <base64 public key>
sudo musallahboard-agent trust remove <key id>
sudo musallahboard-agent trust fetch
```

## Roadmap

Eventually I would like to create a premade Pi image (or an image builder) with the agent preinstalled, so that users can just flash an SD card and have a ready-to-go kiosk. Unfortunately, everything has been testing my patience lately and I gave up on that for now.
