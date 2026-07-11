# MakerUSA Relay

**Intelligent USB Storage Appliance**

Relay turns a small RK35xx single-board computer into a headless USB storage
appliance. Any device that records to USB — video mixers, cameras, audio
recorders, medical equipment — sees Relay as an ordinary USB drive. Files are
automatically ingested, checksum-verified, and uploaded to a configured cloud
destination, with no manual intervention and only a brief (~2 s) interruption
to the recording device.

> The Go module, systemd service, and on-disk paths use the internal name
> `ivault` (e.g. `/etc/ivault`, `ivault.service`); the product is **MakerUSA
> Relay**. The companion cloud portal lives in
> [`makerusa/ivault-portal`](https://github.com/makerusa/ivault-portal).

## How it works

```
[ Recording device ] ──USB-C──▶ [ RK35xx / Relay ] ──▶ [ Google Drive ]
```

1. The recording device writes files to Relay as if it were a USB drive.
2. On a schedule (within an allowed time window), or on manual/portal trigger,
   Relay briefly takes the drive offline (~2 s).
3. New files are copied to an upload queue with SHA-256 verification and
   recorded in SQLite.
4. The virtual drive reattaches; the recorder keeps working.
5. Files upload to the configured destination in the background.

## Features

- Presents as native USB mass storage to any host (exFAT "superfloppy" for
  maximum recorder compatibility).
- Automatic ingest with combined copy-and-verify (SHA-256) and dedup.
- Upload to Google Drive via credentials pushed from the portal (SMB/local
  scaffolding exists; Google Drive is the supported path).
- **Window-aware scheduler** — daily within an allowed hour window (so a
  disconnect never interrupts a recording session), interval, or off.
- **Space-based retention** (opt-in) — deletes only already-uploaded files when
  the drive crosses a threshold.
- **Status LED** — slow pulse = not provisioned/not connected, rapid pulse =
  provisioning, solid = provisioned and connected.
- **Portal heartbeat** — telemetry, device state, and a per-file manifest
  synced efficiently to the cloud portal (delta by content hash).
- Cached credentials **encrypted at rest**; unique per-device USB serial.
- Plug/unplug detection with debounce; graceful gadget teardown on shutdown;
  startup recovery of stuck states.

## Supported hardware

The installer detects the board; the USB OTG controller is auto-detected from
`/sys/class/udc/`, so no per-board value is hardcoded.

| Board | SoC | Notes |
|-------|-----|-------|
| Seeed reComputer RK3576 | RK3576 | SD boot, 1× NVMe, no eMMC. |
| Radxa Rock 5T | RK3588 | eMMC + up to 2× NVMe. |

Other RK35xx boards with a USB-C OTG (peripheral-capable) port should work.

## Requirements

- Armbian minimal (Debian) on microSD (or eMMC).
- Network (Ethernet recommended) for install and portal sync.
- The installer installs the rest (Go 1.25, gcc, rclone, exfatprogs, …).

## Quick install

Flash **Armbian minimal**, boot, get on the network, then:

```bash
sudo apt update && sudo apt install -y git
git clone https://github.com/makerusa/ivault.git
cd ivault
sudo ./scripts/install.sh
```

The installer:

- **Detects your disks** (boot device, eMMC, NVMe) and never touches the disk
  you booted from.
- **Chooses the external-drive backing** automatically — a whole dedicated NVMe
  (exFAT superfloppy) when one is free, or an exFAT image on a shared single
  NVMe. Either way the host sees a superfloppy.
- **Prompts only for real choices** (which NVMe, how much space) and confirms
  once before wiping anything.
- **Auto-detects the USB OTG controller** and **neutralizes the vendor USB
  gadget service** (`usbdevice`) that otherwise steals the controller.
- **Builds, configures, and starts** the systemd service.

Flags: `--yes`, `--otg-disk=`, `--internal-disk=`, `--external-size=`,
`--udc=`, `--label=`, `--no-samba`, `--no-build`, `--migrate-os-to-emmc`
(see `--help`).

> **Boot device:** the installer leaves your OS where it is and uses the NVMe
> for storage. On a board with onboard **eMMC**, it offers to move the OS there
> first (interactive prompt, or `--migrate-os-to-emmc`) so the appliance boots
> card-free and every NVMe is freed for storage — see below. On boards without
> eMMC it prints the `armbian-install` steps for moving to NVMe instead.

### Move the OS to eMMC (optional)

Boards with onboard eMMC (e.g. Rock 5T) run best card-free: the microSD comes
out, boot is faster and more reliable, and both NVMe slots are freed for
storage. When the installer detects eMMC and you booted from microSD/NVMe, it
offers to migrate:

```bash
sudo ./scripts/install.sh --migrate-os-to-emmc   # or just answer "y" at the prompt
```

The migration itself is handed to Armbian's `armbian-install` (choose **Boot
from eMMC — system on eMMC**), which writes the Rockchip bootloader at the
correct offsets for your board — the installer never copies the root filesystem
or writes a bootloader by hand, since getting that wrong can brick a headless
device. When it finishes: power off, remove the microSD, boot from eMMC, and
re-run `sudo ./scripts/install.sh` so storage is laid out around the new OS.

### USB troubleshooting (rare)

The installer neutralizes the vendor `usbdevice` service, which is what
otherwise stops the drive from appearing — no other USB step is normally
needed. If a host still can't see the drive, check the gadget state:

```bash
cat /sys/class/udc/*/state     # "configured" = a host is connected; "not attached" = no host seen
```

**`not attached` that never changes when you plug in a host** means the USB-C
port isn't detecting the cable. Work through, in order:

1. **Cable/port.** Use a known data-capable cable into the board's USB-C **OTG**
   port (not a USB-A port, not a charge-only cable). Confirm the board is powered
   from its own supply, not through the OTG-C port.
2. **Kernel/image.** On RK3588 (Rock 5T) the USB-C role/VBUS detection is
   kernel-dependent — some Armbian builds regressed it. If plugging in produces
   **no `dmesg` activity at all** and `/sys/class/typec/port0/power_role` is
   stuck at `source`, try a known-good image; e.g. `26.8.0-trunk.326` works on
   the Rock 5T where an earlier 26.8.0 did not.
3. **Force peripheral (RK3588 fallback).** If you can't change the image, force
   the OTG controller to device-only mode so it enumerates via the PHY's own
   VBUS detection instead of the Type-C role path:
   ```bash
   sudo armbian-add-overlay scripts/overlays/rk3588-usb-peripheral.dts
   sudo reboot
   ```
4. **Flaky SuperSpeed link (RK3576).** As a last resort, if a host can mount the
   drive but `dmesg` shows a persistent `dwc3 … device reset` loop, pin the OTG
   controller to USB 2.0:
   ```bash
   sudo armbian-add-overlay scripts/overlays/rk3576-usb-highspeed.dts
   sudo reboot
   ```

## Status LED

Relay drives a board LED (default `user-led`, configurable) to reflect
provisioning/connection status:

| LED | Meaning |
|-----|---------|
| Slow pulse (~3 s) | Not provisioned, or not connected to the portal |
| Rapid pulse (~400 ms) | Provisioning in progress |
| Solid | Provisioned and connected |

## Provisioning

Provision a device from the portal: it generates an `ivault.provision` file you
copy onto the Relay drive. Because the drive is single-writer exFAT, Relay reads
it the next time it takes the drive (ejecting the drive triggers this
immediately; a scheduled cycle also picks it up). The LED shows the rapid
"provisioning" pulse while it applies, then solid once connected. You can also
set `user_id`/`device_id`/`device_api_key`/`cloud_endpoint` in the config
directly.

## Configuration

`/etc/ivault/config.json` (written by the installer). Key fields:

| Field | Default | Purpose |
|-------|---------|---------|
| `image_path` | `/nvme/usb_disk.img` | External-drive backing (image or `/dev/…`) |
| `mount_point`, `upload_queue`, `db_path` | under `/nvme` | Internal storage |
| `udc_name` | auto-detected | USB Device Controller |
| `schedule_mode` | `daily` | `daily` \| `interval` \| `off` |
| `schedule_window_start_hour` / `_end_hour` | `2` / `5` | Allowed window for `daily` |
| `schedule_interval_minutes` | `60` | For `interval` mode |
| `retention_enabled` / `retention_threshold_percent` | `false` / `80` | Space-based cleanup |
| `led_enabled` / `led_name` | `true` / `user-led` | Status LED |

## Storage layout

```
microSD / eMMC — OS
NVMe (internal, ext4, mounted /nvme)
├── usb_disk.img       ← external USB drive (exFAT image; single-NVMe boards)
├── ingest/            ← temporary mount point during maintenance
├── upload_queue/      ← files staged for upload
├── relay/ivault.db    ← SQLite (file tracking, config, logs)
└── relay/secret.key   ← per-device key for at-rest credential encryption
```
On dual-NVMe boards, a whole NVMe is dedicated as the external drive instead of
an image file.

## Hardening

After install, `scripts/harden.sh` (run deliberately, supports `--dry-run`)
reduces attack surface and background load: masks unused vendor services
(Bluetooth, the camera-ISP daemon, auto-updates), caps logs, applies sysctl
hardening, and offers opt-in `--ssh-lockdown`, `--firewall`, and
`--samba=keep|lockdown|remove`.

## Manual build (development)

```bash
sudo apt install -y gcc libc6-dev sqlite3 exfatprogs rclone
CGO_ENABLED=1 go build -o ivault .
```
Run against a config file: `sudo ./ivault --config /etc/ivault/config.json`.
Trigger a maintenance cycle manually: `sudo kill -USR1 $(pgrep ivault)`.

## Roadmap

- [x] Automatic scheduler (window-aware)
- [x] Retention policy (space-based)
- [x] Cloud management portal + provisioning
- [x] Metrics / telemetry
- [x] Installer script
- [x] Status LED
- [ ] Google Drive app-created-folder flow (least-privilege uploads)
- [ ] SMB/NAS destination (finish + test)
- [ ] OTA firmware updates

## License

MIT
