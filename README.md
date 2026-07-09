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
| Seeed reComputer RK3576 | RK3576 | SD boot, 1× NVMe, no eMMC. Needs the USB high-speed overlay (see below). |
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
`--udc=`, `--label=`, `--no-samba`, `--no-build` (see `--help`).

> **Boot device:** the installer leaves your OS where it is and uses the NVMe
> for storage. To run card-free from eMMC/NVMe, it prints the `armbian-install`
> steps — it won't migrate a live root filesystem for you.

### RK3576 USB overlay

The RK3576 OTG controller defaults to attempting an unstable SuperSpeed link.
If a connected host doesn't see the drive and `dmesg` shows repeated
`dwc3 … device reset`, apply the high-speed overlay and reboot:

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
├── ivault/ivault.db   ← SQLite (file tracking, config, logs)
└── ivault/secret.key  ← per-device key for at-rest credential encryption
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
