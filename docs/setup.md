# iVault Setup Guide
### Intelligent USB Storage Appliance
**Platform:** Radxa Rock 5T | **OS:** Armbian 26.2.6 (Debian Trixie) | **Version:** 0.1.0

---

## Overview

iVault turns a Radxa Rock 5T into a dedicated USB storage appliance. Any device that records to USB — video mixers, cameras, audio recorders, medical equipment — sees iVault as a standard USB drive. Files are automatically ingested and uploaded to Google Drive (or other cloud backends) without any manual intervention.

```
[ Recording Device ]
        │
   USB-C (data)
        │
   [ Rock 5T / iVault ]
     │          │
  NVMe SSD   Ethernet
     │          │
  Storage   Google Drive
```

### Key Features
- Presents as a native USB mass storage device to any host
- Automatic ingest and cloud sync
- Non-destructive — device is only offline for ~2 seconds during maintenance
- Upload happens in background while recording continues
- Web UI for status, control, and configuration (roadmap)
- Single binary Go application, runs as a systemd service

---

## Hardware Requirements

| Component | Specification |
|-----------|--------------|
| SBC | Radxa Rock 5T |
| RAM | 16GB (recommended) |
| OS Storage | eMMC (onboard) |
| Data Storage | NVMe SSD, 2TB–4TB |
| Power | 12V DC, 3A+ regulated |
| Network | Ethernet (recommended) |

---

## Part 1 — Boot Armbian from eMMC

### 1.1 Flash Armbian to microSD
Download Armbian Minimal (CLI) for Rock 5T from [armbian.com/radxa-rock-5t](https://www.armbian.com/radxa-rock-5t/) and flash to a microSD card using Balena Etcher or `dd`.

### 1.2 Boot from microSD and install to eMMC
Insert the microSD, power on, complete first-boot setup, then run:

```bash
sudo armbian-install
```

Select **Option 2 — Boot from eMMC – system on eMMC**.

> **Troubleshooting:** If you see "Partition too small. Needed: 1442 MB Available: MB", the eMMC has stale data. Wipe it first:
> ```bash
> sudo dd if=/dev/zero of=/dev/mmcblk0 bs=1M count=64 status=progress
> sudo dd if=/dev/zero of=/dev/mmcblk0boot0 bs=1M status=progress
> sudo dd if=/dev/zero of=/dev/mmcblk0boot1 bs=1M status=progress
> ```
> Then re-run `sudo armbian-install`.

### 1.3 Verify eMMC boot
Power off, remove the microSD card, power on. You should see the Armbian banner with:
- Root filesystem on eMMC (~113GB)
- Kernel: `6.1.115-vendor-rk35xx`

---

## Part 2 — NVMe Setup

### 2.1 Partition and format NVMe

```bash
sudo fdisk /dev/nvme0n1
# Inside fdisk:
# g  → new GPT partition table
# n  → new partition (accept all defaults)
# w  → write and exit

sudo mkfs.ext4 /dev/nvme0n1p1
```

### 2.2 Mount and configure auto-mount

```bash
sudo mkdir -p /nvme
sudo mount /dev/nvme0n1p1 /nvme

echo "UUID=$(sudo blkid -s UUID -o value /dev/nvme0n1p1) /nvme ext4 defaults 0 2" | sudo tee -a /etc/fstab
```

### 2.3 Set permissions

```bash
sudo chown -R $USER:$USER /nvme
```

### 2.4 Create directory structure

```bash
mkdir -p /nvme/ingest /nvme/upload_queue
```

---

## Part 3 — USB Disk Image

### 3.1 Install exFAT tools

```bash
sudo apt install exfatprogs -y
```

### 3.2 Create and format the disk image

```bash
# Create 200GB sparse image (adjust size as needed)
fallocate -l 200G /nvme/usb_disk.img

# Format as exFAT (required by most recording devices)
mkfs.exfat -L "IVAULT" /nvme/usb_disk.img
```

> **Note:** exFAT is required for most professional recording devices. It supports files larger than 4GB (unlike FAT32) and is widely compatible.

---

## Part 4 — Go Installation

### 4.1 Install Go

```bash
cd ~
wget https://go.dev/dl/go1.25.0.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.25.0.linux-arm64.tar.gz
rm go1.25.0.linux-arm64.tar.gz
```

### 4.2 Configure environment

```bash
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
echo 'export GOPATH=$HOME/go' >> ~/.bashrc
echo 'export CGO_ENABLED=1' >> ~/.bashrc
source ~/.bashrc
```

### 4.3 Install build dependencies

```bash
sudo apt install gcc libc6-dev sqlite3 exfat-fuse -y
```

### 4.4 Verify

```bash
go version
# Expected: go version go1.25.0 linux/arm64
```

---

## Part 5 — iVault Application

### 5.1 Create project structure

```bash
mkdir -p ~/ivault
cd ~/ivault
go mod init github.com/umarsear/ivault
mkdir -p internal/{gadget,ingest,upload,state,device,db,api}
mkdir -p web/{static,templates}
mkdir -p profiles
```

### 5.2 Install dependencies

```bash
go get github.com/mattn/go-sqlite3
go get github.com/gin-gonic/gin
```

### 5.3 Create database layer

**`internal/db/schema.sql`**
```sql
CREATE TABLE IF NOT EXISTS config (
    key        TEXT PRIMARY KEY,
    value      TEXT,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS devices (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    profile     TEXT NOT NULL,
    fs_type     TEXT DEFAULT 'exfat',
    disk_image  TEXT NOT NULL,
    mount_point TEXT NOT NULL,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sessions (
    id         INTEGER PRIMARY KEY,
    device_id  INTEGER REFERENCES devices(id),
    started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    ended_at   DATETIME,
    status     TEXT DEFAULT 'recording',
    bytes      INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS uploads (
    id          INTEGER PRIMARY KEY,
    session_id  INTEGER REFERENCES sessions(id),
    filename    TEXT NOT NULL,
    size        INTEGER DEFAULT 0,
    status      TEXT DEFAULT 'pending',
    attempts    INTEGER DEFAULT 0,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    uploaded_at DATETIME,
    remote_path TEXT
);

CREATE TABLE IF NOT EXISTS logs (
    id        INTEGER PRIMARY KEY,
    ts        DATETIME DEFAULT CURRENT_TIMESTAMP,
    level     TEXT,
    component TEXT,
    message   TEXT
);
```

**`internal/db/db.go`**
```go
package db

import (
	"database/sql"
	"embed"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaFS embed.FS

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}

	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return nil, err
	}

	if _, err := conn.Exec(string(schema)); err != nil {
		return nil, err
	}

	return &DB{conn: conn}, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

func (d *DB) SetConfig(key, value string) error {
	_, err := d.conn.Exec(
		`INSERT INTO config(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`,
		key, value,
	)
	return err
}

func (d *DB) GetConfig(key string) (string, error) {
	var value string
	err := d.conn.QueryRow(`SELECT value FROM config WHERE key=?`, key).Scan(&value)
	return value, err
}

func (d *DB) Log(level, component, message string) error {
	_, err := d.conn.Exec(
		`INSERT INTO logs(level, component, message) VALUES(?, ?, ?)`,
		level, component, message,
	)
	return err
}

func (d *DB) RecentLogs(limit int) ([]LogEntry, error) {
	rows, err := d.conn.Query(
		`SELECT id, ts, level, component, message FROM logs ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		rows.Scan(&e.ID, &e.Ts, &e.Level, &e.Component, &e.Message)
		entries = append(entries, e)
	}
	return entries, nil
}

type LogEntry struct {
	ID        int64
	Ts        string
	Level     string
	Component string
	Message   string
}
```

### 5.4 Create state machine

**`internal/state/state.go`**
```go
package state

import (
	"sync"
)

type State int

const (
	StateRecording   State = iota
	StateDetaching
	StateMaintenance
	StateAttaching
	StateError
)

func (s State) String() string {
	switch s {
	case StateRecording:
		return "recording"
	case StateDetaching:
		return "detaching"
	case StateMaintenance:
		return "maintenance"
	case StateAttaching:
		return "attaching"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

type Machine struct {
	mu       sync.RWMutex
	current  State
	onChange []func(old, new State)
}

func New() *Machine {
	return &Machine{current: StateRecording}
}

func (m *Machine) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *Machine) OnChange(fn func(old, new State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

func (m *Machine) Transition(next State) {
	m.mu.Lock()
	old := m.current
	m.current = next
	handlers := m.onChange
	m.mu.Unlock()

	for _, fn := range handlers {
		fn(old, next)
	}
}
```

### 5.5 Create gadget package

**`internal/gadget/gadget.go`**
```go
package gadget

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	gadgetDir = "/sys/kernel/config/usb_gadget/ivault"
	udcName   = "fc000000.usb"
)

func Attach(imagePath string) error {
	// Always clean up any stale gadget first
	Detach()
	time.Sleep(200 * time.Millisecond)

	if err := os.MkdirAll(gadgetDir, 0755); err != nil {
		return fmt.Errorf("create gadget dir: %w", err)
	}

	writes := map[string]string{
		"idVendor":  "0x1d6b",
		"idProduct": "0x0104",
		"bcdDevice": "0x0100",
		"bcdUSB":    "0x0200",
	}
	for k, v := range writes {
		if err := writeFile(gadgetDir+"/"+k, v); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
	}

	if err := os.MkdirAll(gadgetDir+"/strings/0x409", 0755); err != nil {
		return err
	}
	stringWrites := map[string]string{
		"strings/0x409/serialnumber": "ivault-001",
		"strings/0x409/manufacturer": "iVault",
		"strings/0x409/product":      "iVault Storage",
	}
	for k, v := range stringWrites {
		if err := writeFile(gadgetDir+"/"+k, v); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
	}

	if err := os.MkdirAll(gadgetDir+"/configs/c.1/strings/0x409", 0755); err != nil {
		return err
	}
	if err := writeFile(gadgetDir+"/configs/c.1/strings/0x409/configuration", "Mass Storage"); err != nil {
		return err
	}
	if err := writeFile(gadgetDir+"/configs/c.1/MaxPower", "250"); err != nil {
		return err
	}

	if err := os.MkdirAll(gadgetDir+"/functions/mass_storage.0", 0755); err != nil {
		return err
	}
	funcWrites := map[string]string{
		"functions/mass_storage.0/stall":           "0",
		"functions/mass_storage.0/lun.0/removable": "1",
		"functions/mass_storage.0/lun.0/ro":        "0",
		"functions/mass_storage.0/lun.0/cdrom":     "0",
		"functions/mass_storage.0/lun.0/file":      imagePath,
	}
	for k, v := range funcWrites {
		if err := writeFile(gadgetDir+"/"+k, v); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
	}

	symlink := gadgetDir + "/configs/c.1/mass_storage.0"
	if _, err := os.Lstat(symlink); os.IsNotExist(err) {
		if err := os.Symlink(gadgetDir+"/functions/mass_storage.0", symlink); err != nil {
			return fmt.Errorf("symlink: %w", err)
		}
	}

	if err := writeFile(gadgetDir+"/UDC", udcName); err != nil {
		return fmt.Errorf("enable udc: %w", err)
	}

	return nil
}

func Detach() error {
	if _, err := os.Stat(gadgetDir); os.IsNotExist(err) {
		return nil
	}

	writeFile(gadgetDir+"/UDC", "")
	time.Sleep(500 * time.Millisecond)

	steps := []string{
		gadgetDir + "/configs/c.1/mass_storage.0",
		gadgetDir + "/configs/c.1/strings/0x409",
		gadgetDir + "/configs/c.1",
		gadgetDir + "/functions/mass_storage.0",
		gadgetDir + "/strings/0x409",
		gadgetDir,
	}

	for _, path := range steps {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			os.Remove(path)
		} else {
			exec.Command("rmdir", path).Run()
		}
	}

	return nil
}

func State() string {
	b, err := os.ReadFile("/sys/class/udc/" + udcName + "/state")
	if err != nil {
		return "unknown"
	}
	return string(b[:len(b)-1])
}

func IsAttached() bool {
	_, err := os.Stat(gadgetDir)
	return err == nil
}

func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}
```

### 5.6 Create ingest package

**`internal/ingest/ingest.go`**
```go
package ingest

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	ImagePath   = "/nvme/usb_disk.img"
	MountPoint  = "/nvme/ingest"
	UploadQueue = "/nvme/upload_queue"
)

func Mount() error {
	cmd := exec.Command("mount", "-o", "loop", ImagePath, MountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount failed: %w — %s", err, string(out))
	}
	return nil
}

func Unmount() error {
	cmd := exec.Command("umount", MountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount failed: %w — %s", err, string(out))
	}
	return nil
}

func MoveFiles() ([]string, error) {
	var moved []string

	entries, err := os.ReadDir(MountPoint)
	if err != nil {
		return nil, fmt.Errorf("read mount point: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Skip macOS metadata and system files
		if strings.HasPrefix(name, "._") ||
			name == ".DS_Store" ||
			name == ".Spotlight-V100" ||
			name == ".fseventsd" ||
			strings.HasPrefix(name, ".") {
			continue
		}

		src := filepath.Join(MountPoint, name)
		dst := filepath.Join(UploadQueue, name)

		if err := moveFile(src, dst); err != nil {
			return moved, fmt.Errorf("move %s: %w", name, err)
		}

		moved = append(moved, name)
	}

	return moved, nil
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	if err := out.Sync(); err != nil {
		return err
	}

	return os.Remove(src)
}
```

### 5.7 Create upload package

**`internal/upload/upload.go`**
```go
package upload

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	UploadQueue = "/nvme/upload_queue"
	RemoteName  = "gdrive"
	RemotePath  = "iVault"
)

func UploadAll() ([]string, error) {
	entries, err := os.ReadDir(UploadQueue)
	if err != nil {
		return nil, fmt.Errorf("read upload queue: %w", err)
	}

	var uploaded []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		src := filepath.Join(UploadQueue, name)
		dst := fmt.Sprintf("%s:%s/%s", RemoteName, RemotePath, name)

		if err := uploadFile(src, dst); err != nil {
			return uploaded, fmt.Errorf("upload %s: %w", name, err)
		}

		os.Remove(src)
		uploaded = append(uploaded, name)
	}

	return uploaded, nil
}

func uploadFile(src, dst string) error {
	cmd := exec.Command("rclone", "copyto",
		"--retries", "3",
		"--low-level-retries", "10",
		"--stats", "0",
		src, dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rclone error: %w — %s", err, string(out))
	}
	return nil
}
```

### 5.8 Create main.go

**`main.go`**
```go
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/umarsear/ivault/internal/db"
	"github.com/umarsear/ivault/internal/gadget"
	"github.com/umarsear/ivault/internal/ingest"
	"github.com/umarsear/ivault/internal/state"
	"github.com/umarsear/ivault/internal/upload"
)

func main() {
	database, err := db.Open("/var/lib/ivault/ivault.db")
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}
	defer database.Close()

	sm := state.New()
	sm.OnChange(func(old, new state.State) {
		msg := "state transition: " + old.String() + " → " + new.String()
		log.Println(msg)
		database.Log("info", "state", msg)
	})

	sm.Transition(state.StateAttaching)
	if err := gadget.Attach("/nvme/usb_disk.img"); err != nil {
		log.Fatalf("failed to attach gadget: %v", err)
	}
	sm.Transition(state.StateRecording)
	log.Println("iVault ready — gadget state:", gadget.State())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1)
	log.Println("Send SIGUSR1 to trigger maintenance: kill -USR1", os.Getpid())

	for sig := range sigs {
		log.Println("received signal:", sig)
		runMaintenance(sm, database)
	}
}

func runMaintenance(sm *state.Machine, database *db.DB) {
	log.Println("--- maintenance cycle starting ---")

	sm.Transition(state.StateDetaching)
	if err := gadget.Detach(); err != nil {
		log.Println("detach error:", err)
		sm.Transition(state.StateError)
		return
	}
	time.Sleep(500 * time.Millisecond)

	sm.Transition(state.StateMaintenance)
	if err := ingest.Mount(); err != nil {
		log.Println("mount error:", err)
		sm.Transition(state.StateError)
		return
	}
	log.Println("disk image mounted")

	files, err := ingest.MoveFiles()
	if err != nil {
		log.Println("move error:", err)
	}
	log.Printf("moved %d files to upload queue: %v", len(files), files)
	database.Log("info", "ingest", fmt.Sprintf("moved %d files", len(files)))

	if err := ingest.Unmount(); err != nil {
		log.Println("unmount error:", err)
	}
	log.Println("disk image unmounted")

	sm.Transition(state.StateAttaching)
	if err := gadget.Attach("/nvme/usb_disk.img"); err != nil {
		log.Println("reattach error:", err)
		sm.Transition(state.StateError)
		return
	}
	sm.Transition(state.StateRecording)
	log.Println("gadget reattached — device can record again")

	go func() {
		log.Println("starting upload to Google Drive...")
		uploaded, err := upload.UploadAll()
		if err != nil {
			log.Println("upload error:", err)
			database.Log("error", "upload", err.Error())
			return
		}
		log.Printf("uploaded %d files to Google Drive: %v", len(uploaded), uploaded)
		database.Log("info", "upload", fmt.Sprintf("uploaded %d files", len(uploaded)))
	}()

	log.Println("--- maintenance cycle complete ---")
}
```

### 5.9 Build

```bash
cd ~/ivault
CGO_ENABLED=1 go build -o ivault .
```

### 5.10 Create application data directory

```bash
sudo mkdir -p /var/lib/ivault
sudo chown $USER:$USER /var/lib/ivault
```

---

## Part 6 — rclone / Google Drive

### 6.1 Install rclone

```bash
sudo apt install rclone -y
```

### 6.2 Configure Google Drive remote

```bash
rclone config
```

- `n` — new remote
- Name: `gdrive`
- Storage: `18` (Google Drive)
- client_id, client_secret: blank (Enter)
- scope: `1` (full access)
- All other prompts: blank or default
- Auto config: `n` (headless)
- Run on a machine with a browser: `rclone authorize "drive" "<token>"`
- Shared Drive: `n`
- Confirm: `y`
- Quit: `q`

### 6.3 Copy rclone config to root (required since iVault runs as root)

```bash
sudo mkdir -p /root/.config/rclone
sudo cp ~/.config/rclone/rclone.conf /root/.config/rclone/rclone.conf
```

### 6.4 Verify

```bash
rclone lsd gdrive:
```

---

## Part 7 — Systemd Service

### 7.1 Create service file

```bash
sudo nano /etc/systemd/system/ivault.service
```

```ini
[Unit]
Description=iVault - Intelligent USB Storage Appliance
After=local-fs.target
DefaultDependencies=no

[Service]
Type=simple
ExecStart=/home/umarsear/ivault/ivault
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

### 7.2 Enable and start

```bash
sudo systemctl daemon-reload
sudo systemctl enable ivault.service
sudo systemctl start ivault.service
sudo systemctl status ivault.service
```

---

## Storage Layout

```
eMMC (116GB) — OS
├── /var/lib/ivault/ivault.db    ← SQLite database (always accessible)
└── /home/umarsear/ivault/       ← Application binary and source

NVMe (1.8TB) — Data
├── /nvme/usb_disk.img           ← Virtual USB drive (200GB, exFAT)
├── /nvme/ingest/                ← Temporary mount point
└── /nvme/upload_queue/          ← Files staged for cloud upload
```

---

## State Machine

```
[Recording]
    │  SIGUSR1 / schedule / trigger
    ▼
[Detaching]   ← gadget.Detach()
    │
    ▼
[Maintenance] ← ingest.Mount() → MoveFiles() → Unmount()
    │
    ▼
[Attaching]   ← gadget.Attach()   (device back online in ~2 seconds)
    │
    ▼
[Recording]   ← upload.UploadAll() running in background goroutine
```

---

## Triggering Maintenance

**Manual (current):**
```bash
sudo kill -USR1 $(pgrep ivault)
```

**Scheduled (cron):**
```bash
# Every hour
0 * * * * root kill -USR1 $(pgrep ivault)
```

---

## Useful Commands

```bash
# Check system status
systemctl status ivault.service

# View logs
journalctl -u ivault.service -f

# Check SQLite logs
sqlite3 /var/lib/ivault/ivault.db "SELECT * FROM logs ORDER BY id DESC LIMIT 20;"

# Check upload queue
ls -lh /nvme/upload_queue/

# Check gadget state
cat /sys/class/udc/fc000000.usb/state

# Check NVMe usage
df -h /nvme

# Manually trigger maintenance
sudo kill -USR1 $(pgrep ivault)
```

---

## Roadmap

- [ ] Web UI — dashboard, status, logs, configuration
- [ ] Scheduled maintenance (configurable interval)
- [ ] Device profiles (per-device file filtering, filesystem settings)
- [ ] Multiple cloud backends (S3, Backblaze, Dropbox)
- [ ] Multiple virtual drives (one per connected device)
- [ ] Upload progress tracking in SQLite
- [ ] Retry failed uploads
- [ ] Bandwidth throttling for uploads
- [ ] Setup wizard (first-run configuration)
- [ ] Single-script installer
- [ ] Caddy reverse proxy for web UI
- [ ] Fleet management (multiple iVault units)

---

## Known Issues / Notes

- Upload speed is limited by your internet connection. Large files (video) will take time. The recording device is unaffected as upload runs in the background.
- rclone config must be copied to `/root/.config/rclone/` since iVault currently runs as root.
- The UDC identifier `fc000000.usb` is specific to the Rock 5T (RK3588S). Other boards will have a different UDC name — check with `ls /sys/class/udc/`.
- macOS metadata files (`._*`, `.DS_Store`) are automatically filtered during ingest.

---

*iVault v0.1.0 — Built on Radxa Rock 5T*
