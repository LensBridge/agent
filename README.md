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
chmod +x ./setup.sh
sudo ./setup.sh
```

Follow the prompts to set the device up, then reboot. Upon reboot, the device will be at a splash screen waiting for enrollment. To enroll the device, run:

```bash
sudo musallahboard-agent enroll \
  --backend <your backend instance>\
  --token <one-time-token-from-admin-ui
```

The device will then be enrolled and the MusallahBoard frontend will load automatically. The device is now ready to be used as a kiosk!

## Building

A little unorthodox, but this go project uses a Makefile to build the agent binary. The Makefile has targets for both native and cross-compilation.

```bash
# Native (host arch)
make build

# Cross-compile for Raspberry Pi (arm64)
make build-arm64
```

Requires Go 1.23+. Binaries land in `build/`.

## Roadmap

Eventually I would like to create a premade Pi image (or an image builder) with the agent preinstalled, so that users can just flash an SD card and have a ready-to-go kiosk. Unfortunately, everything has been testing my patience lately and I gave up on that for now.
