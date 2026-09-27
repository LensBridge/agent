# mbvm: a test board in a virtual machine

`vm/mbvm` runs a MusallahBoard in QEMU on your computer, so you can test with
just a backend and no Raspberry Pi. It is a Debian trixie VM set up by this
checkout's `setup.sh`, the same way a Pi is. It runs a released agent (or one
built from this checkout), and a window shows the board's screen.

The board keeps its state between runs. `reset` takes it back to a freshly
provisioned board with whichever agent version you want.

## Requirements

- **macOS (Apple Silicon or Intel):** `brew install qemu mtools xorriso`
- **Linux:** QEMU with its GUI and tools, plus mtools and xorriso. On
  Debian/Ubuntu: `sudo apt install qemu-system qemu-system-gui qemu-utils mtools xorriso`.
  For an arm64 VM you also need `qemu-efi-aarch64`. Your user needs access to
  `/dev/kvm`, for example via the `kvm` group.
- On both: `curl`, `ssh` and `python3`. Go and make are needed only for
  `agent local`.

The VM uses your computer's architecture: arm64 on Apple Silicon, amd64 on
most PCs. That way it runs with hardware virtualisation. Any other
architecture is emulated, which works but is very slow.

## Getting started

```sh
vm/mbvm create        # first time: downloads Debian, runs setup.sh (a few minutes)
vm/mbvm enroll --token=<token from the portal> --backend=http://localhost:8080
```

`localhost` in `--backend` means your computer: mbvm rewrites it to the
address the VM reaches your computer at (10.0.2.2). A deployed backend URL is
passed through unchanged.

Once enrolled, the board installs the latest board app from its release
channel, syncs content and shows the board, exactly like a Pi.

## Commands

| Command | What it does |
|---|---|
| `create [--agent=V]` | Provision a new VM and start it |
| `start [--headless]` / `stop` | Boot or shut down; the board keeps its state |
| `reset [--agent=V]` | Back to a freshly provisioned, unenrolled board |
| `agent V` | Install agent `V` on the running board, keeping its state |
| `enroll --token=T --backend=URL` | Enroll the board |
| `usb FILE.mbu...` / `usb --remove` | Plug in, or pull out, a USB stick holding these files |
| `status` | VM state and `musallahboard-agent status` |
| `logs` | Follow the agent's log |
| `ssh [command]` | A shell on the board (you are `mbadmin`, with sudo) |
| `tunnel` | Open the board (http://127.0.0.1:18080/) and Chromium DevTools (http://127.0.0.1:19222/) on your computer |
| `screenshot [FILE.png]` | Save what the screen shows |
| `snapshot save\|restore\|delete NAME`, `snapshot list` | Named copies of the board. They survive `reset` |
| `rebuild` | Provision a new base, after changing `setup.sh` |
| `destroy` | Delete the VM (downloaded images are kept) |

Agent versions (`V`) can be:

- `latest`
- a release, such as `0.2.1`
- `local`, which builds this checkout
- a path to an agent binary

## Recipes

**Test an update from an older agent.** Start from an old agent and let the
board update itself:

```sh
vm/mbvm reset --agent=0.2.1
vm/mbvm enroll --token=... --backend=...
vm/mbvm ssh sudo musallahboard-agent update now
```

**Test a USB update.**

```sh
vm/mbvm usb musallahboard-app-2.1.1.mbu content.mbu
# watch the window, then
vm/mbvm usb --remove
```

**Keep an enrolled board with content, to come back to.**

```sh
vm/mbvm stop
vm/mbvm snapshot save enrolled
# ... later, after resets and experiments
vm/mbvm stop && vm/mbvm snapshot restore enrolled && vm/mbvm start
```

**Try agent changes from this checkout.**

```sh
MB_RELEASE_PUBLIC_KEYS=<the repository variable> vm/mbvm agent local
```

A local build reports version `dev`, so it does not follow the agent release
channel and is not replaced overnight. Without `MB_RELEASE_PUBLIC_KEYS` it
trusts no release key, and so it refuses the signed board app. Set
`MBVM_LOCAL_VERSION=0.3.1` (for example) to build with a real version
instead.

**Several boards.** Give each its own name and SSH port:

```sh
MBVM_NAME=second MBVM_SSH_PORT=2223 vm/mbvm create
```

## Settings

All are environment variables:

| Variable | Default | |
|---|---|---|
| `MBVM_NAME` | `board` | Which VM |
| `MBVM_SSH_PORT` | `2222` | Host port for SSH to the board |
| `MBVM_ARCH` | this computer's | `amd64` or `arm64`, chosen at `create` |
| `MBVM_MEMORY`, `MBVM_CPUS` | `2048`, `2` | |
| `MBVM_RESOLUTION` | `1920x1080` | The board is designed for 1080p. The window scales to fit |
| `MBVM_TIMEZONE` | `America/Toronto` | Passed to `setup.sh` |
| `MBVM_DISPLAY` | cocoa or gtk | A raw QEMU `-display` value, such as `vnc=127.0.0.1:1` |
| `MBVM_QEMU_ARGS` | | Extra QEMU arguments |
| `MBVM_IMAGE_URL` | Debian 13 generic cloud image | |
| `MBVM_HOME` | `~/.local/share/musallahboard-vm` | Where VMs and images live |

## How it works

- **The base image.** `create` boots the Debian cloud image with a cloud-init
  seed that adds the `mbadmin` user and the VM's own SSH key. It then runs
  `setup.sh` over SSH and shuts down. The result, `base.qcow2`, is never booted
  again.
- **The board's disk.** `disk.qcow2` is a copy-on-write layer on top of the
  base. `reset` deletes that layer and makes a new one, which is why a reset
  takes seconds. Snapshots are copies of it.
- **The agent.** `setup.sh` installs the latest agent release. `agent V`
  replaces the binary directly and restarts the agent. That is how a test
  board can go back to an older version, which a real board, updated only
  through signed packages, never does.
- **VM detection.** `setup.sh` notices it is in a VM. It then accepts amd64
  as well as arm64, and skips the Pi-only steps (Raspberry Pi Connect and the
  hardware watchdog). The kiosk uses software rendering.
- **What a VM cannot do.** There is no ethernet service port, no RTC, and no
  real display hardware. Test those on a Pi.
