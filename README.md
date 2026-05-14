# iVault

**Intelligent USB Storage Appliance**

iVault turns a Radxa Rock 5T (or compatible RK3588 SBC) into a headless USB storage appliance. Any device that records to USB — video mixers, cameras, audio recorders, medical equipment — sees iVault as a standard USB drive. Files are automatically ingested and uploaded to configured destinations (NAS, Google Drive, etc.) without manual intervention.

## How It Works
[ Recording Device ] ──USB-C──▶ [ Rock 5T / iVault ] ──▶ [ NAS / Google Drive ]

1. Recording device writes files to iVault as if it were a USB drive
2. On a schedule (or manually triggered), iVault detaches the virtual drive
3. Files are copied to an upload queue with SHA-256 verification
4. Virtual drive reattaches (~2 seconds offline)
5. Files upload to configured destination in the background

## Features

- Presents as native USB mass storage to any host device
- Automatic ingest with checksum verification
- Upload to Google Drive, SMB/NAS, or local path
- File lifecycle tracking in SQLite
- Plug/unplug detection — interrupts upload cleanly if device reconnects
- Graceful shutdown — clean gadget teardown on SIGTERM
- Startup recovery — resets stuck states after crash or power loss
- State machine: booting → recording → maintenance → uploading → recording

## Hardware

Developed and tested on:
- **Radxa Rock 5T** (RK3588S, 16GB RAM)
- **NVMe SSD** (2TB) for data storage
- **eMMC** for OS

Other RK3588-based boards with USB-C OTG support may work. The UDC name
(`fc000000.usb`) may differ — check `ls /sys/class/udc/`.

## Requirements

- Armbian 26.2.6+ (Debian Trixie) on eMMC
- Go 1.25+
- gcc, libc6-dev (for CGO/SQLite)
- exfatprogs, exfat-fuse
- rclone (for cloud destinations)

## Setup

### 1. Prepare storage
```bash
# Partition and format NVMe
sudo fdisk /dev/nvme0n1   # g, n, w
sudo mkfs.ext4 /dev/nvme0n1p1
sudo mkdir -p /nvme
sudo mount /dev/nvme0n1p1 /nvme
echo "UUID=$(sudo blkid -s UUID -o value /dev/nvme0n1p1) /nvme ext4 defaults 0 2" | sudo tee -a /etc/fstab
sudo chown -R $USER:$USER /nvme

# Create virtual drive (200GB, exFAT)
sudo apt install exfatprogs exfat-fuse -y
mkdir -p /nvme/ingest /nvme/upload_queue
fallocate -l 200G /nvme/usb_disk.img
mkfs.exfat -L "IVAULT" /nvme/usb_disk.img
```

### 2. Build iVault
```bash
sudo apt install gcc libc6-dev sqlite3 -y
git clone https://github.com/makerusa/ivault.git
cd ivault
CGO_ENABLED=1 go build -o ivault .
```

### 3. Create data directory
```bash
sudo mkdir -p /var/lib/ivault
sudo chown $USER:$USER /var/lib/ivault
```

### 4. Configure rclone (Google Drive)
```bash
sudo apt install rclone -y
rclone config   # follow prompts, name remote "gdrive"
sudo mkdir -p /root/.config/rclone
sudo cp ~/.config/rclone/rclone.conf /root/.config/rclone/rclone.conf
```

### 5. Run
```bash
sudo ./ivault
```

### 6. Trigger maintenance manually
```bash
sudo kill -USR1 $(pgrep ivault)
```

## Storage Layout
eMMC
└── /var/lib/ivault/ivault.db    ← SQLite (always accessible)
NVMe
├── /nvme/usb_disk.img           ← Virtual USB drive (exFAT)
├── /nvme/ingest/                ← Temporary mount point
└── /nvme/upload_queue/          ← Files staged for upload

## State Machine
booting → attaching → recording
│
(trigger)
▼
detaching → maintenance → attaching → recording
│
uploading (background)
│
recording

## Reference Hardware

| Component | Model | Approx Cost |
|-----------|-------|-------------|
| SBC | Radxa Rock 5T 16GB | ~$140 |
| NVMe | Samsung 990 Pro 2TB | ~$90 |
| Power | 12V 3A DC | ~$12 |

## Roadmap

- [ ] Automatic scheduler (interval-based maintenance)
- [ ] SMB/NAS destination
- [ ] Retention policy
- [ ] Cloud management portal (app.ivault.net)
- [ ] Provisioning via config file on drive
- [ ] Metrics (CPU, temp, network) 
- [ ] Installer script
- [ ] OTA firmware updates

## License

MIT
