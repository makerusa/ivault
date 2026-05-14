# iVault — Complete Technical Specification
**Version:** 0.2.0 | **Status:** Draft | **Date:** May 2026

---

## 1. Product Overview

iVault is a headless USB storage appliance built on the Radxa Rock 5T. It presents itself as a standard USB mass storage device to any connected recording device — video mixers, cameras, audio recorders, medical equipment, industrial systems — and automatically ingests recorded files to one or more configured destinations (NAS, Google Drive, S3, etc.) without any manual intervention.

The device is managed entirely through a cloud portal at `app.ivault.net`. No keyboard, monitor, or direct network access is required after initial provisioning.

### 1.1 Core Principles

- **Transparency** — The connected device always sees a normal USB drive. iVault's operation is invisible to it.
- **Reliability** — Core recording function (USB gadget) must never depend on cloud connectivity, network availability, or upload success.
- **Non-destructive** — Original files are never deleted until successful upload is confirmed and retention policy permits.
- **Resilience** — Every operation is recoverable. Crashes, power loss, and network failures leave the system in a consistent state.
- **Zero-touch** — After provisioning, the device requires no human interaction.

---

## 2. System Architecture

### 2.1 Physical Architecture

```
[ Recording Device ]
        │
   USB-C (data only)
        │
   [ Rock 5T ]
   ┌────────────────────────────────┐
   │  iVault Process                │
   │  ┌──────────┐  ┌────────────┐ │
   │  │  Gadget  │  │   State    │ │
   │  │ Manager  │  │  Machine   │ │
   │  └──────────┘  └────────────┘ │
   │  ┌──────────┐  ┌────────────┐ │
   │  │  Ingest  │  │  Upload    │ │
   │  │ Manager  │  │  Manager   │ │
   │  └──────────┘  └────────────┘ │
   │  ┌──────────┐  ┌────────────┐ │
   │  │  Cloud   │  │  SQLite    │ │
   │  │  Agent   │  │    DB      │ │
   │  └──────────┘  └────────────┘ │
   └────────────────────────────────┘
        │                │
     NVMe SSD         Ethernet
        │                │
   /nvme/            api.ivault.net
   usb_disk.img      (long-poll HTTPS)
   upload_queue/
   ingest/
```

### 2.2 Storage Layout

```
eMMC (116GB) — OS + Application
├── /var/lib/ivault/
│   ├── ivault.db              ← SQLite database (always accessible)
│   └── config/
│       └── rclone.conf        ← rclone configuration
└── /home/ivault/
    └── ivault                 ← Application binary

NVMe (1.8TB+) — Data
├── /nvme/
│   ├── usb_disk.img           ← Virtual USB drive (exFAT, configurable size)
│   ├── ingest/                ← Temporary mount point (always empty at rest)
│   ├── upload_queue/          ← Files staged for upload (retained until confirmed)
│   └── originals/             ← (optional) mirror of uploaded files for retention
```

### 2.3 Cloud Architecture

```
┌─────────────────────────────────────────────────────┐
│                  api.ivault.net                     │
│                                                     │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │  Device API  │  │   User API   │  │  Auth     │ │
│  │  (Go/HTTP)   │  │  (Go/HTTP)   │  │  (JWT)    │ │
│  └─────────────┘  └──────────────┘  └───────────┘ │
│  ┌─────────────────────────────────────────────┐   │
│  │              SQLite (per customer)           │   │
│  └─────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────┘
         ▲
         │ HTTPS (outbound only from device)
         │
┌────────────────┐
│  iVault Device │
│  Long-poll     │
│  every 30s     │
└────────────────┘
```

### 2.4 Device-Cloud Communication

All communication is **outbound from the device**. The device initiates all connections. No inbound ports are required. Works behind any NAT or firewall.

**Heartbeat / Command Poll (every 30s):**
```
GET https://api.ivault.net/v1/devices/{id}/poll
Authorization: Bearer {device_token}

Response (command pending):
{"command": "maintenance", "payload": {}, "command_id": "abc123"}

Response (no commands, after 30s timeout):
{"command": null}
```

**Status Push (every 60s):**
```
POST https://api.ivault.net/v1/devices/{id}/status
{
  "state": "recording",
  "gadget_state": "configured",
  "storage_used_bytes": 90194313216,
  "storage_total_bytes": 214748364800,
  "queue_depth": 0,
  "queue_bytes": 0,
  "cpu_percent": 12,
  "memory_used_bytes": 1288490188,
  "memory_total_bytes": 16777216000,
  "cpu_temp_celsius": 48,
  "nvme_temp_celsius": 42,
  "uptime_seconds": 345600,
  "network_rx_mbps": 0.2,
  "network_tx_mbps": 82.4,
  "firmware_version": "0.2.0",
  "timestamp": "2026-05-12T03:32:00Z"
}
```

**Event Push (immediate, on state change):**
```
POST https://api.ivault.net/v1/devices/{id}/events
{
  "event": "state_transition",
  "from": "recording",
  "to": "maintenance",
  "timestamp": "2026-05-12T03:32:00Z"
}
```

---

## 3. State Machine

### 3.1 States

| State | Description |
|-------|-------------|
| `booting` | Process starting, initializing DB and gadget |
| `provisioning` | Watching for provision file on drive |
| `recording` | Gadget attached, device can write freely |
| `detaching` | Gracefully detaching gadget before maintenance |
| `maintenance` | Gadget detached, mounting and copying files |
| `attaching` | Re-attaching gadget after maintenance |
| `uploading` | Background upload in progress (gadget is attached) |
| `error` | Unrecoverable error, check logs |
| `shutting_down` | SIGTERM received, cleaning up gracefully |

### 3.2 State Transitions

```
[booting]
    │
    ├─ provision file found ──▶ [provisioning]
    │                               │
    │                               └─ configured ──▶ [recording]
    │
    └─ already configured ──▶ [attaching] ──▶ [recording]
                                                    │
                              ┌─────────────────────┤
                              │  trigger: schedule,  │
                              │  signal, threshold,  │
                              │  cloud command       │
                              ▼                      │
                          [detaching]                │
                              │                      │
                              ▼                      │
                          [maintenance]              │
                              │                      │
                              ▼                      │
                          [attaching] ───────────────┘
                              │
                              └─ upload in background ──▶ [uploading]
                                                              │
                                                              └─▶ [recording]

Any state ──▶ [shutting_down] on SIGTERM
Any state ──▶ [error] on unrecoverable failure
[error] ──▶ [recording] on recovery / restart
```

### 3.3 USB Plug/Unplug Events

The gadget manager continuously watches `/sys/class/udc/fc000000.usb/state`.

| UDC State | iVault State | Action |
|-----------|-------------|--------|
| `configured` | `recording` | Normal — device connected |
| `not attached` | `recording` | Device unplugged — log, stay ready |
| `configured` | `maintenance` / `uploading` | Device plugged in during sync — abort upload, unmount, reattach immediately |
| `not attached` | `maintenance` | Device unplugged during sync — continue, will reattach when done |

**Plug-in interrupt flow:**
```
[uploading / maintenance]
    │
    USB plug detected (UDC → configured)
    │
    ▼
Cancel active upload goroutine (context cancel)
    │
    ▼
If image is mounted → unmount
    │
    ▼
[attaching] → [recording]
    │
    ▼
Log: "Upload interrupted by device reconnect — will resume at next cycle"
    │
    ▼
Files remain in upload_queue — picked up at next maintenance cycle
```

---

## 4. File Lifecycle

### 4.1 File States

```
[on_device]         File exists on virtual drive (usb_disk.img)
    │
    ▼
[discovered]        Found during maintenance mount, recorded in DB
    │
    ▼
[copied]            Copied to upload_queue/, checksum verified
    │
    ▼ (still on virtual drive until retention policy permits deletion)
[queued]            Waiting for upload
    │
    ▼
[uploading]         Active upload in progress
    │
    ├─ success ──▶ [uploaded]     Confirmed at destination
    │                  │
    │                  ▼
    │             [eligible]      Retention policy check passed
    │                  │
    │                  ▼
    │             [deleted]       Removed from virtual drive
    │
    └─ failure ──▶ [failed]       Will retry per retry policy
                       │
                       ├─ retry ──▶ [queued]
                       └─ max retries ──▶ [abandoned] (alert sent)
```

### 4.2 Ingest Process (Maintenance Cycle)

```
1. Detach gadget
2. Mount usb_disk.img at /nvme/ingest (loop mount, exFAT)
3. Scan /nvme/ingest for files
4. For each eligible file:
   a. Skip if already in DB with state >= copied
   b. Skip system/metadata files (._*, .DS_Store, .Spotlight-V100, .fseventsd)
   c. Apply extension filter (if configured)
   d. Apply minimum size filter (if configured)
   e. Compute SHA-256 checksum of source file
   f. Copy to /nvme/upload_queue/{filename}
   g. Verify checksum of copy matches source
   h. Record in DB: files table, state = copied
5. Unmount /nvme/ingest
6. Reattach gadget  ← device back online
7. Trigger upload (background goroutine)
```

### 4.3 Copy vs Move

Files are **copied, not moved** during ingest. The original remains on the virtual drive until:
1. Upload is confirmed successful to at least one destination
2. Retention policy permits deletion

This means if the device is replugged during upload, or upload fails, the file is safe on the virtual drive and will be picked up again at the next maintenance cycle (deduplicated by checksum in DB).

### 4.4 Retention Policy

Configurable per device in portal. Applied to files with state = `uploaded`.

| Policy | Description |
|--------|-------------|
| `delete_after_upload` | Delete from virtual drive immediately after confirmed upload |
| `keep_n_days` | Keep for N days after upload, then delete |
| `keep_n_gb` | Keep until virtual drive exceeds N GB, then delete oldest first |
| `keep_forever` | Never delete from virtual drive (manual management) |

Default: `delete_after_upload`

Retention enforcement runs at the end of each maintenance cycle, after upload confirmation.

---

## 5. Upload Manager

### 5.1 Destination Priority

Each device has an ordered list of destinations. Upload attempts primary first, falls back to next if unreachable.

```
Destination 1 (Primary):    SMB/NAS
Destination 2 (Fallback):   Google Drive
```

Before each maintenance cycle, all destinations are health-checked. If no destinations are reachable, ingest still runs (files copied to queue) but upload is skipped — queued files will be retried at next cycle.

### 5.2 Upload Flow

```
For each file in upload_queue/ with state = queued:
    1. Check destination reachability
    2. If unreachable, try next destination
    3. If no destinations reachable, skip (retry next cycle)
    4. Mark file state = uploading in DB
    5. Upload file to destination
    6. On success:
       - Mark file state = uploaded in DB
       - Record destination, timestamp, remote path
       - Apply retention policy (delete from virtual drive if eligible)
    7. On failure:
       - Increment attempts counter
       - If attempts < max_retries: mark state = queued (retry next cycle)
       - If attempts >= max_retries: mark state = abandoned, send alert
```

### 5.3 Upload Parallelism

Configurable per destination:
- SMB/NAS: default 4 parallel uploads (fast local network)
- Google Drive: default 2 parallel uploads (API rate limits)
- Bandwidth limit: configurable in Mbps per destination

### 5.4 Context Cancellation

Every upload operation runs with a Go `context.Context`. The upload manager holds a `context.CancelFunc`. When a USB plug event interrupts maintenance:
```go
uploadManager.Cancel() // cancels all in-flight uploads
```
Cancelled uploads are returned to `queued` state in DB.

### 5.5 Supported Destinations

| Type | Backend | Status |
|------|---------|--------|
| SMB / CIFS | rclone smb | v0.2 |
| Google Drive | rclone drive | v0.2 |
| Local path | os.Copy | v0.2 |
| FTP / SFTP | rclone ftp/sftp | Roadmap |
| Amazon S3 | rclone s3 | Roadmap |
| Backblaze B2 | rclone b2 | Roadmap |
| Dropbox | rclone dropbox | Roadmap |
| OneDrive | rclone onedrive | Roadmap |

---

## 6. USB Gadget Manager

### 6.1 Responsibilities

- Configure USB mass storage gadget via Linux configfs
- Monitor UDC state continuously
- Emit plug/unplug events to state machine
- Clean teardown on process exit

### 6.2 UDC Monitor

A dedicated goroutine polls `/sys/class/udc/fc000000.usb/state` every 500ms.

```go
type UDCEvent int

const (
    UDCPlugged   UDCEvent = iota // not attached → configured
    UDCUnplugged                 // configured → not attached
)
```

State change debounce: 1 second (ignore transient flickers during enumeration).

### 6.3 Gadget Configuration

| Parameter | Value |
|-----------|-------|
| idVendor | 0x1d6b (Linux Foundation) |
| idProduct | 0x0104 (Mass Storage) |
| Manufacturer | iVault |
| Product | iVault Storage |
| Serial | {device_id[:8]} |
| UDC | fc000000.usb (Rock 5T) |
| LUN filesystem | exFAT |
| Read-only | No |
| Removable | Yes |

### 6.4 Graceful Shutdown

On SIGTERM:
1. Transition to `shutting_down`
2. Cancel any active upload context
3. If image is mounted → unmount
4. Detach gadget (clean configfs teardown)
5. Close DB
6. Exit 0

---

## 7. Provisioning

### 7.1 Provision File Detection

On startup, and continuously during `recording` state, iVault watches for `ivault-provision.json` in the root of the virtual drive.

Detection method: poll mount + directory scan every 5 seconds during recording state, or on USB plug event.

### 7.2 Provision Flow

```
1. USB plug event received
2. Mount usb_disk.img read-only
3. Check for ivault-provision.json in root
4. If found:
   a. Read and decrypt file (AES-256-GCM)
   b. Validate token expiry
   c. Validate device_id matches (or is new provisioning)
   d. Unmount
   e. Reattach gadget
   f. Apply network configuration
   g. Write cloud credentials to /var/lib/ivault/config/
   h. Delete provision file (via loop mount + delete + unmount)
   i. Connect to cloud API
   j. POST /v1/devices/{id}/provision-complete
   k. Transition to recording state
5. If not found: unmount, normal operation
```

### 7.3 Provision File Format

Encrypted with AES-256-GCM. Key derived via HKDF from one-time token.

```json
{
  "version": 1,
  "device_id": "a3f2c891-4d12-4b7e-9c3a-1234567890ab",
  "device_name": "Studio A",
  "token": "eyJhbGciOiJIUzI1NiJ9...",
  "token_expires": "2026-05-12T04:00:00Z",
  "cloud_endpoint": "https://api.ivault.net",
  "network": {
    "mode": "dhcp",
    "interface": "eth0",
    "wifi_ssid": null,
    "wifi_password": null,
    "static_ip": null,
    "static_subnet": null,
    "static_gateway": null,
    "static_dns": null
  }
}
```

Security properties:
- AES-256-GCM encryption
- Token expires 30 minutes after generation
- Token is single-use — invalidated on first successful device connection
- File is deleted from virtual drive immediately after processing
- Device generates keypair on first boot; public key registered via provision token

---

## 8. SQLite Schema

Database location: `/var/lib/ivault/ivault.db`
WAL mode enabled. Foreign keys enforced.

```sql
-- Device identity and configuration
CREATE TABLE IF NOT EXISTS config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Recording sessions (one per maintenance cycle)
CREATE TABLE IF NOT EXISTS sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    ended_at     DATETIME,
    status       TEXT DEFAULT 'active',  -- active, complete, interrupted
    files_found  INTEGER DEFAULT 0,
    files_copied INTEGER DEFAULT 0,
    bytes_copied INTEGER DEFAULT 0
);

-- Individual file tracking
CREATE TABLE IF NOT EXISTS files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      INTEGER REFERENCES sessions(id),
    filename        TEXT NOT NULL,
    size_bytes      INTEGER NOT NULL,
    checksum_sha256 TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'discovered',
    -- discovered, copied, queued, uploading, uploaded, failed, abandoned, deleted
    discovered_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    copied_at       DATETIME,
    queued_at       DATETIME,
    uploaded_at     DATETIME,
    deleted_at      DATETIME,
    upload_attempts INTEGER DEFAULT 0,
    destination_id  INTEGER REFERENCES destinations(id),
    remote_path     TEXT,
    error_message   TEXT
);

-- Configured upload destinations
CREATE TABLE IF NOT EXISTS destinations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    type         TEXT NOT NULL,  -- smb, gdrive, local, ftp, s3
    priority     INTEGER NOT NULL DEFAULT 0,  -- lower = higher priority
    config_json  TEXT NOT NULL,  -- encrypted destination-specific config
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_used_at DATETIME,
    last_ok_at   DATETIME,
    last_error   TEXT
);

-- System event log
CREATE TABLE IF NOT EXISTS logs (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        DATETIME DEFAULT CURRENT_TIMESTAMP,
    level     TEXT NOT NULL,     -- debug, info, warn, error
    component TEXT NOT NULL,     -- gadget, ingest, upload, state, cloud, system
    message   TEXT NOT NULL,
    data_json TEXT               -- optional structured context
);

-- Upload queue (denormalized for fast queue operations)
CREATE TABLE IF NOT EXISTS upload_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id     INTEGER REFERENCES files(id),
    destination_id INTEGER REFERENCES destinations(id),
    enqueued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    attempts    INTEGER DEFAULT 0,
    last_attempt DATETIME,
    status      TEXT DEFAULT 'pending'  -- pending, uploading, done, failed
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_files_state ON files(state);
CREATE INDEX IF NOT EXISTS idx_files_checksum ON files(checksum_sha256);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts);
CREATE INDEX IF NOT EXISTS idx_upload_queue_status ON upload_queue(status);
```

---

## 9. Go Package Structure

```
ivault/
├── main.go                      ← Entry point, wiring
├── go.mod
├── go.sum
│
├── internal/
│   ├── config/
│   │   └── config.go            ← App config (loaded from DB + env)
│   │
│   ├── db/
│   │   ├── db.go                ← Connection, migrations
│   │   ├── schema.sql
│   │   ├── files.go             ← File CRUD
│   │   ├── sessions.go          ← Session CRUD
│   │   ├── destinations.go      ← Destination CRUD
│   │   ├── logs.go              ← Log writes + queries
│   │   └── queue.go             ← Upload queue operations
│   │
│   ├── state/
│   │   └── state.go             ← State machine + transitions
│   │
│   ├── gadget/
│   │   ├── gadget.go            ← configfs attach/detach
│   │   └── monitor.go           ← UDC state watcher + events
│   │
│   ├── ingest/
│   │   ├── ingest.go            ← Mount, scan, copy, unmount
│   │   ├── filter.go            ← File filtering rules
│   │   └── checksum.go          ← SHA-256 verification
│   │
│   ├── upload/
│   │   ├── manager.go           ← Upload orchestration, parallelism, cancellation
│   │   ├── destination.go       ← Destination interface
│   │   ├── smb.go               ← SMB/CIFS via rclone
│   │   ├── gdrive.go            ← Google Drive via rclone
│   │   └── local.go             ← Local path copy
│   │
│   ├── retention/
│   │   └── retention.go         ← Retention policy enforcement
│   │
│   ├── cloud/
│   │   ├── agent.go             ← Long-poll, heartbeat, event push
│   │   └── provision.go         ← Provision file detection + processing
│   │
│   ├── metrics/
│   │   └── metrics.go           ← CPU, memory, temp, network stats
│   │
│   └── cycle/
│       └── cycle.go             ← Maintenance cycle orchestration
│                                   (owns the full recording→maintenance→recording flow)
│
├── profiles/                    ← Device profile JSON files
│   ├── generic.json
│   ├── gostream.json
│   └── zoom_h6.json
│
└── web/                         ← (future local fallback UI)
```

### 9.1 Key Interfaces

```go
// Destination interface — all upload backends implement this
type Destination interface {
    Name() string
    Type() string
    Test() error                                    // connectivity check
    Upload(ctx context.Context, src, dst string) error
    Delete(ctx context.Context, path string) error
}

// UDC event channel
type GadgetMonitor interface {
    Events() <-chan UDCEvent
    State() string
    Start() error
    Stop()
}

// Maintenance cycle
type Cycle interface {
    Run(ctx context.Context) error
    IsRunning() bool
    Cancel()
}
```

---

## 10. Maintenance Cycle — Full Sequence

```
Trigger received (schedule / cloud command / SIGUSR1 / storage threshold)
    │
    ├─ IsRunning() == true? → skip, log "cycle already in progress"
    │
    ▼
Acquire cycle mutex
    │
    ▼
Check destination reachability
    ├─ At least one reachable? → continue
    └─ None reachable? → log warn, still run ingest (queue for later), continue
    │
    ▼
[state: detaching]
gadget.Detach()
sleep 500ms
    │
    ▼
[state: maintenance]
ingest.Mount()
    ├─ error? → gadget.Attach(), [state: error], return
    │
    ▼
ingest.Scan() → []FileInfo
    │
    ▼
For each file:
    ├─ Already in DB (by checksum)? → skip
    ├─ Passes filters? → continue
    ├─ SHA-256 source checksum
    ├─ Copy to /nvme/upload_queue/
    ├─ SHA-256 verify copy
    └─ Record in DB (state: copied → queued)
    │
    ▼
ingest.Unmount()
    │
    ▼
[state: attaching]
gadget.Attach()                    ← DEVICE BACK ONLINE
[state: recording]
    │
    ▼
Release cycle mutex
    │
    ▼
upload.Manager.RunAsync(ctx)       ← background goroutine, gadget already attached
    │
    ├─ For each queued file:
    │   ├─ Select reachable destination (priority order)
    │   ├─ Mark uploading in DB
    │   ├─ Upload with context (cancellable)
    │   ├─ On success: mark uploaded, apply retention
    │   └─ On failure: increment attempts, requeue or abandon
    │
    └─ On context cancel (USB plug interrupt):
        ├─ Return in-flight files to queued state
        └─ Exit goroutine cleanly
    │
    ▼
retention.Enforce()                ← delete eligible files from virtual drive
    │
    ▼
[state: recording]
```

---

## 11. Triggers

Maintenance cycle can be triggered by:

| Trigger | Source | Notes |
|---------|--------|-------|
| Schedule | Internal cron | Configurable interval, blackout windows |
| Cloud command | Long-poll response | User clicks "Trigger Sync" in portal |
| SIGUSR1 | OS signal | Manual / scripting |
| Storage threshold | Storage monitor | Virtual drive > X% full |
| USB unplug event | Gadget monitor | Optional — configurable |

---

## 12. Metrics Collection

Collected every 60 seconds for status push to cloud:

| Metric | Source |
|--------|--------|
| CPU % | `/proc/stat` |
| Memory used/total | `/proc/meminfo` |
| CPU temperature | `/sys/class/thermal/thermal_zone*/temp` |
| NVMe temperature | `smartctl` or `/sys/class/nvme/nvme0/device/hwmon*/temp1_input` |
| Network rx/tx Mbps | `/proc/net/dev` (delta between samples) |
| Virtual drive used/total | `statfs` on mounted image |
| NVMe used/total | `statfs(/nvme)` |
| Queue depth / bytes | SQLite query |
| Uptime | `/proc/uptime` |

---

## 13. Cloud API — Device Endpoints

All device requests authenticated with `Authorization: Bearer {device_token}`.

```
GET    /v1/devices/{id}/poll              Long-poll for commands (30s timeout)
POST   /v1/devices/{id}/status            Push status/metrics
POST   /v1/devices/{id}/events            Push state change events
POST   /v1/devices/{id}/logs              Push log batch
POST   /v1/devices/{id}/provision-complete Confirm provisioning complete
GET    /v1/devices/{id}/config            Pull latest device configuration
POST   /v1/devices/{id}/command-ack       Acknowledge command execution
```

### 13.1 Commands (cloud → device via poll response)

```json
{"command": "maintenance"}
{"command": "reboot"}
{"command": "restart_service"}
{"command": "update_firmware", "payload": {"version": "0.2.1", "url": "...", "sha256": "..."}}
{"command": "update_config", "payload": { ...config... }}
{"command": "factory_reset"}
```

---

## 14. Cloud API — User Endpoints

All user requests authenticated with `Authorization: Bearer {user_jwt}`.

```
POST   /v1/auth/signup
POST   /v1/auth/signin
POST   /v1/auth/refresh
POST   /v1/auth/signout

GET    /v1/devices                        List user's devices
POST   /v1/devices                        Register new device (returns provision file)
GET    /v1/devices/{id}                   Device details + current status
PATCH  /v1/devices/{id}                   Update device name/config
DELETE /v1/devices/{id}                   Remove device

POST   /v1/devices/{id}/commands          Send command to device
GET    /v1/devices/{id}/status/history    Status history (for graphs)
GET    /v1/devices/{id}/logs              Paginated log query
GET    /v1/devices/{id}/files             File history
GET    /v1/devices/{id}/sessions          Session history

GET    /v1/devices/{id}/destinations      List destinations
POST   /v1/devices/{id}/destinations      Add destination
PATCH  /v1/devices/{id}/destinations/{did} Update destination
DELETE /v1/devices/{id}/destinations/{did} Remove destination
POST   /v1/devices/{id}/destinations/{did}/test  Test connectivity

GET    /v1/devices/{id}/schedule          Get schedule config
PATCH  /v1/devices/{id}/schedule          Update schedule

POST   /v1/oauth/gdrive/start             Start Google Drive OAuth flow
POST   /v1/oauth/gdrive/callback          OAuth callback, stores tokens
```

---

## 15. Configuration Reference

Stored in SQLite `config` table on device. Pushed from cloud via `update_config` command.

```json
{
  "device_id": "a3f2c891-4d12-4b7e-9c3a-1234567890ab",
  "device_name": "Studio A",
  "cloud_endpoint": "https://api.ivault.net",
  "cloud_token": "eyJ...",
  "poll_interval_seconds": 30,
  "status_interval_seconds": 60,

  "disk_image_path": "/nvme/usb_disk.img",
  "disk_image_size_gb": 200,
  "disk_label": "IVAULT",
  "disk_filesystem": "exfat",
  "ingest_mount_point": "/nvme/ingest",
  "upload_queue_path": "/nvme/upload_queue",

  "file_filters": {
    "skip_system_files": true,
    "allowed_extensions": [],
    "min_size_bytes": 0
  },

  "schedule": {
    "mode": "interval",
    "interval_minutes": 60,
    "blackout_enabled": false,
    "blackout_start": "20:00",
    "blackout_end": "23:00"
  },

  "storage_threshold": {
    "enabled": true,
    "trigger_percent": 80
  },

  "retention": {
    "policy": "delete_after_upload"
  },

  "upload": {
    "parallel_workers": 2,
    "bandwidth_limit_mbps": 0,
    "max_retries": 3,
    "retry_delay_minutes": 5
  },

  "udc_name": "fc000000.usb"
}
```

---

## 16. Device Profiles

Stored as JSON in `/profiles/`. Selected during provisioning or manually in portal.

```json
{
  "id": "gostream-deck",
  "name": "Osee GoStream Deck/Duet",
  "description": "Osee GoStream Deck and Duet video mixers",
  "filesystem": "exfat",
  "allowed_extensions": [".mp4", ".mov"],
  "min_file_size_bytes": 1048576,
  "usb_label": "GOSTREAM",
  "notes": "Requires exFAT. Records .mp4 files."
}
```

```json
{
  "id": "generic",
  "name": "Generic Device",
  "description": "Any USB recording device",
  "filesystem": "exfat",
  "allowed_extensions": [],
  "min_file_size_bytes": 0,
  "usb_label": "IVAULT",
  "notes": "No file type filtering."
}
```

---

## 17. Error Handling & Recovery

| Scenario | Behavior |
|----------|----------|
| Gadget attach fails on startup | Retry 3x with backoff, then error state + alert |
| Mount fails during maintenance | Re-attach gadget, error state, log |
| Checksum mismatch on copy | Delete bad copy, retry file, log error |
| Upload destination unreachable | Try next destination, queue for retry if all fail |
| Upload interrupted by USB plug | Cancel context, return files to queued, reattach gadget |
| Process crash mid-maintenance | On restart: check if mounted (unmount), check queue (resume), reattach gadget |
| Power loss mid-copy | On restart: files with state=uploading reset to queued, re-verify checksums |
| Cloud unreachable | Continue all local operations, queue status pushes, retry connection |
| NVMe full | Alert, pause ingest, do not delete until confirmed upload |
| Virtual drive full | Alert, trigger maintenance immediately |

### 17.1 Startup Recovery

On every startup, before normal operation:
1. Check if `/nvme/ingest` is mounted → unmount if so
2. Reset any files with state `uploading` → `queued`
3. Reset any files with state `copying` → `discovered`
4. Verify queue files exist on disk (clean DB if not)
5. Proceed with normal startup

---

## 18. Security

| Concern | Mitigation |
|---------|------------|
| Provision file interception | AES-256-GCM encryption, 30min expiry, single-use token |
| Device impersonation | Keypair generated at first boot, public key bound to device_id |
| Token theft | Short-lived JWTs (15min) + refresh tokens, device tokens are long-lived but device-scoped |
| Destination credentials | Stored encrypted in SQLite on eMMC, never sent to cloud |
| Cloud communication | TLS 1.3 only, certificate pinning (roadmap) |
| Physical access | eMMC not easily removable, provision file self-destructs |

---

## 19. Systemd Service

`/etc/systemd/system/ivault.service`

```ini
[Unit]
Description=iVault - Intelligent USB Storage Appliance
After=local-fs.target network.target nvme-mount.service
Wants=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/ivault
Restart=on-failure
RestartSec=10
TimeoutStopSec=30

# Give SIGTERM first, then SIGKILL after 30s
KillMode=mixed
KillSignal=SIGTERM

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=ivault

[Install]
WantedBy=multi-user.target
```

---

## 20. Installer Script (Planned)

Single script to go from bare Armbian to fully running iVault:

```bash
curl -fsSL https://get.ivault.net/install.sh | sudo bash
```

Steps performed:
1. Detect hardware (Rock 5T, UDC name, eMMC, NVMe)
2. Install dependencies (Go, gcc, exfatprogs, exfat-fuse, rclone, sqlite3)
3. Download latest iVault binary from GitHub releases (pre-built ARM64)
4. Create directory structure (/nvme, /var/lib/ivault, etc.)
5. Partition and format NVMe if unformatted
6. Create usb_disk.img (prompt for size)
7. Format usb_disk.img as exFAT
8. Write /etc/fstab entry for NVMe
9. Write systemd service
10. Enable and start service
11. Print provisioning instructions

---

## 21. Roadmap

### v0.2.0 (current)
- [x] eMMC boot
- [x] NVMe setup
- [x] USB gadget (Go-native)
- [x] State machine
- [x] SQLite database
- [x] Ingest (copy + checksum)
- [x] Upload (rclone)
- [x] Google Drive destination
- [ ] SMB/NAS destination
- [ ] USB plug/unplug detection
- [ ] Retention policy
- [ ] Graceful shutdown
- [ ] Startup recovery
- [ ] Cloud agent (long-poll)
- [ ] Provisioning file flow

### v0.3.0
- [ ] Cloud portal (app.ivault.net)
- [ ] User accounts + device registration
- [ ] Remote configuration push
- [ ] Remote command execution
- [ ] Metrics dashboard
- [ ] Log viewer
- [ ] Firmware OTA updates

### v0.4.0
- [ ] Installer script
- [ ] Device profiles (GoStream, Zoom H6, generic)
- [ ] File filtering UI
- [ ] Blackout windows
- [ ] Storage threshold alerts
- [ ] Email notifications

### v1.0.0
- [ ] S3 / Backblaze destination
- [ ] SFTP destination
- [ ] Multi-device dashboard
- [ ] Fleet management
- [ ] Custom hardware (purpose-built PCB)

---

*iVault Technical Specification v0.2.0*
*Radxa Rock 5T | Armbian 26.2.6 | Go 1.25*
