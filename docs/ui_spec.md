# iVault Cloud Portal — UI Specification
**Version:** 0.1.0 | **Status:** Draft

---

## Overview

The iVault portal is a cloud-hosted web application at `app.ivault.net`. It provides device management, status monitoring, storage destination configuration, and system health for all iVault devices registered to a user's account. The device is fully headless — all configuration happens here.

---

## User Flows

### New User Flow
```
Landing Page → Sign Up → Email Verification → Dashboard (empty) → Add Device → Provision → Device Online
```

### Returning User Flow
```
Sign In → Dashboard → Select Device → Monitor / Configure
```

### Device Addition Flow
```
Dashboard → Add Device → Name Device → Network Config → Download Provision File → 
Copy to Drive → Eject → Device Phones Home → Device Online ✓
```

---

## Screen Inventory

| # | Screen | Route |
|---|--------|-------|
| 1 | Landing Page | `/` |
| 2 | Sign Up | `/signup` |
| 3 | Sign In | `/signin` |
| 4 | Dashboard (Device List) | `/dashboard` |
| 5 | Device Overview | `/devices/{id}` |
| 6 | Device — Storage | `/devices/{id}/storage` |
| 7 | Device — Destinations | `/devices/{id}/destinations` |
| 8 | Device — Schedule | `/devices/{id}/schedule` |
| 9 | Device — System | `/devices/{id}/system` |
| 10 | Device — Logs | `/devices/{id}/logs` |
| 11 | Add Device Wizard | `/devices/new` |
| 12 | Account Settings | `/account` |

---

## Screen Specifications

---

### 1. Landing Page `/`

**Purpose:** Marketing + entry point

**Sections:**
- Hero: tagline, "Get Started Free" CTA, "Sign In" link
- How it works: 3-step visual (Plug in → Configure → Records to Cloud)
- Use cases: Video production, Houses of worship, Audio recording, Security, Industrial
- Supported destinations: Google Drive, SMB/NAS, FTP, S3 (roadmap)
- Sign Up / Sign In buttons in nav

---

### 2. Sign Up `/signup`

**Fields:**
- Full name
- Email address
- Password (min 12 chars)
- Confirm password
- [ ] I agree to Terms of Service

**Actions:**
- Create Account → sends verification email
- Already have an account? Sign In

**Post-signup:** Email verification required before dashboard access.

---

### 3. Sign In `/signin`

**Fields:**
- Email
- Password
- [ ] Remember me

**Actions:**
- Sign In
- Forgot password? → email reset flow
- Don't have an account? Sign Up

---

### 4. Dashboard `/dashboard`

**Purpose:** All devices at a glance.

**Header:**
- "My Devices" title
- "+ Add Device" button (primary CTA)

**Device Cards (grid, 2-3 col):**

Each card shows:
```
┌─────────────────────────────────┐
│ 🟢 Online          [Studio A]   │
│                                 │
│ State:     Recording            │
│ Storage:   [████░░░░] 42% used  │
│            84GB / 200GB         │
│ Queue:     0 files pending      │
│ Last sync: 2 minutes ago        │
│                                 │
│ CPU: 12%  Temp: 48°C  RAM: 8%  │
│                                 │
│         [Manage →]              │
└─────────────────────────────────┘
```

**Status indicators:**
- 🟢 Online — connected, polling active
- 🟡 Busy — maintenance/upload in progress
- 🔴 Offline — not seen in >2 minutes
- ⚙️ Provisioning — config file downloaded, not yet connected
- ⚠️ Error — device reported error state

**Empty state (no devices):**
```
No devices yet.
[+ Add your first device]
```

---

### 5. Device Overview `/devices/{id}`

**Purpose:** Primary device screen. Status at a glance + quick actions.

**Layout:** Left sidebar nav + main content area

**Sidebar nav items:**
- Overview (active)
- Storage
- Destinations
- Schedule
- System
- Logs

---

**Main Content — Overview**

#### Status Banner
```
┌─────────────────────────────────────────────────────┐
│  🟢 Studio A                          [Trigger Sync] │
│  State: Recording                     [•••] More     │
│  Last seen: just now                                 │
└─────────────────────────────────────────────────────┘
```

#### System Health Cards (row of 4)
```
┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
│   CPU    │ │  Memory  │ │   Temp   │ │ Network  │
│          │ │          │ │          │ │          │
│   12%    │ │  8%      │ │  48°C    │ │ 94 Mbps  │
│          │ │ 1.2/15GB │ │          │ │ ↑ 82     │
│ [▁▂▃▂▁] │ │ [▁▁▂▁▁] │ │  ● Cool  │ │ ↓ 12     │
└──────────┘ └──────────┘ └──────────┘ └──────────┘
```

Temperature thresholds:
- ● Cool: < 55°C (green)
- ● Warm: 55–70°C (yellow)
- ● Hot: > 70°C (red)

#### Storage Overview
```
┌─────────────────────────────────────────────────────┐
│ Virtual Drive (usb_disk.img)                        │
│ [████████░░░░░░░░░░░░] 42%                         │
│ 84.0 GB used of 200.0 GB                           │
│                                                     │
│ NVMe (System)                                       │
│ [██░░░░░░░░░░░░░░░░░░] 12%                         │
│ 201 GB used of 1.8 TB                              │
└─────────────────────────────────────────────────────┘
```

#### Upload Queue
```
┌─────────────────────────────────────────────────────┐
│ Upload Queue                              [0 files] │
│                                                     │
│ ✓ All files synced                                  │
│ Last sync: Today at 8:32 PM → NAS (Studio-NAS)     │
└─────────────────────────────────────────────────────┘
```

When files are queued:
```
│ 3 files pending upload (2.4 GB)                     │
│ ↑ Uploading: RecordingSession_001.mp4  [████░░] 67% │
```

#### Recent Activity (last 5 events)
```
┌─────────────────────────────────────────────────────┐
│ Recent Activity                          [View All] │
├─────────────────────────────────────────────────────┤
│ 20:32  ✓ Sync complete — 3 files → Studio-NAS      │
│ 20:31  ↩ Gadget reattached                         │
│ 20:31  ↑ Maintenance cycle complete                 │
│ 20:30  ⏏ Gadget detached                           │
│ 20:30  ⚡ Maintenance triggered (schedule)          │
└─────────────────────────────────────────────────────┘
```

#### Quick Actions
```
[Trigger Sync Now]  [Restart iVault]  [Download Logs]
```

---

### 6. Device — Storage `/devices/{id}/storage`

**Purpose:** Configure the virtual USB drive.

#### Virtual Drive Settings
```
┌─────────────────────────────────────────────────────┐
│ Virtual Drive                                       │
├─────────────────────────────────────────────────────┤
│ Label:       [IVAULT              ]                 │
│ Size:        [200        ] GB                       │
│ Filesystem:  [exFAT ▾]                             │
│                                                     │
│ ⚠ Resizing requires a maintenance cycle.           │
│   Files will be preserved.                          │
│                                                     │
│                              [Save Changes]         │
└─────────────────────────────────────────────────────┘
```

#### File Filtering
```
┌─────────────────────────────────────────────────────┐
│ File Filters                                        │
├─────────────────────────────────────────────────────┤
│ Skip system files (._*, .DS_Store, .Spotlight)  [✓] │
│                                                     │
│ Only ingest these extensions (leave blank for all): │
│ [.mp4, .mov, .mxf, .wav, .mp3            ] [+ Add] │
│                                                     │
│ Skip files smaller than: [1    ] MB                │
│ (useful to ignore thumbnails and metadata files)    │
│                                                     │
│                              [Save Changes]         │
└─────────────────────────────────────────────────────┘
```

#### Storage Health
```
┌─────────────────────────────────────────────────────┐
│ NVMe Health                                         │
├─────────────────────────────────────────────────────┤
│ Device:      Samsung 990 Pro 2TB                    │
│ Temperature: 42°C                                   │
│ Total written: 1.2 TB                               │
│ Estimated life remaining: ████████████░ 94%        │
└─────────────────────────────────────────────────────┘
```

---

### 7. Device — Destinations `/devices/{id}/destinations`

**Purpose:** Configure where files are sent after ingest. This is the most important configuration screen.

#### Destination List
```
┌─────────────────────────────────────────────────────┐
│ Destinations                        [+ Add Destination] │
├─────────────────────────────────────────────────────┤
│                                                     │
│ ┌─────────────────────────────────────────────────┐ │
│ │ 🖥 Studio-NAS                    PRIMARY  [•••] │ │
│ │ SMB / Network Share                             │ │
│ │ \\192.168.1.50\Recordings                      │ │
│ │ 🟢 Reachable · Last used: 2 min ago            │ │
│ └─────────────────────────────────────────────────┘ │
│                                                     │
│ ┌─────────────────────────────────────────────────┐ │
│ │ ☁ Google Drive               FALLBACK  [•••]  │ │
│ │ Google Drive                                    │ │
│ │ /iVault/Studio A                               │ │
│ │ 🟢 Connected · umarsear@gmail.com              │ │
│ └─────────────────────────────────────────────────┘ │
│                                                     │
└─────────────────────────────────────────────────────┘
```

**Destination priority:** Drag to reorder. Primary is tried first, fallback if unreachable.

#### Add Destination — SMB/NAS
```
┌─────────────────────────────────────────────────────┐
│ Add SMB / Network Share                        [✕]  │
├─────────────────────────────────────────────────────┤
│ Name:         [Studio-NAS                    ]      │
│ Host:         [192.168.1.50                  ]      │
│ Share:        [Recordings                    ]      │
│ Subfolder:    [/Studio A                     ]      │
│ Username:     [ivault                        ]      │
│ Password:     [••••••••                      ]      │
│ Domain:       [WORKGROUP                     ]      │
│                                                     │
│ SMB Version:  [Auto ▾]                             │
│                                                     │
│               [Test Connection]  [Save]             │
└─────────────────────────────────────────────────────┘
```

#### Add Destination — Google Drive
```
┌─────────────────────────────────────────────────────┐
│ Add Google Drive                               [✕]  │
├─────────────────────────────────────────────────────┤
│ Name:         [My Google Drive               ]      │
│ Folder path:  [/iVault/Studio A              ]      │
│                                                     │
│               [Connect Google Drive →]              │
│                                                     │
│ Opens Google OAuth in this browser.                 │
│ Credentials are stored securely on your device.     │
└─────────────────────────────────────────────────────┘
```

#### Add Destination — FTP/SFTP (roadmap)
#### Add Destination — S3 / Backblaze B2 (roadmap)
#### Add Destination — Local Path (advanced)
```
┌─────────────────────────────────────────────────────┐
│ Add Local Path                                 [✕]  │
├─────────────────────────────────────────────────────┤
│ Name:         [External USB                  ]      │
│ Path:         [/media/backup                 ]      │
│                                                     │
│ ⚠ Path must be accessible on the iVault device.    │
│                                                     │
│               [Test Path]  [Save]                   │
└─────────────────────────────────────────────────────┘
```

#### Destination Card — Context Menu `[•••]`
- Set as Primary
- Set as Fallback
- Edit
- Test Connection
- Remove

---

### 8. Device — Schedule `/devices/{id}/schedule`

**Purpose:** Configure when maintenance cycles run automatically.

#### Maintenance Schedule
```
┌─────────────────────────────────────────────────────┐
│ Maintenance Schedule                                │
├─────────────────────────────────────────────────────┤
│ Mode:                                               │
│ ○ Manual only (trigger via dashboard)               │
│ ● Scheduled                                         │
│ ○ After idle (no writes for X minutes)             │
│                                                     │
│ Run every:  [1 ▾] [Hours ▾]                        │
│                                                     │
│ Next scheduled run: Today at 9:32 PM               │
│                                                     │
│ ─────────────────────────────────────────────────  │
│ Blackout Window (never run during):                 │
│ [ ] Enable blackout window                         │
│ From: [20:00] To: [23:00]                          │
│ (useful during live events)                         │
│                                                     │
│                              [Save Changes]         │
└─────────────────────────────────────────────────────┘
```

#### Upload Settings
```
┌─────────────────────────────────────────────────────┐
│ Upload Settings                                     │
├─────────────────────────────────────────────────────┤
│ Bandwidth limit:                                    │
│ ○ Unlimited                                         │
│ ● Limit to [50    ] Mbps                           │
│                                                     │
│ Retry failed uploads:  [3 ▾] attempts              │
│ Retry delay:           [5 ▾] minutes               │
│                                                     │
│ Delete from queue after successful upload:  [✓]    │
│                                                     │
│                              [Save Changes]         │
└─────────────────────────────────────────────────────┘
```

---

### 9. Device — System `/devices/{id}/system`

**Purpose:** Device identity, network, firmware.

#### Device Info
```
┌─────────────────────────────────────────────────────┐
│ Device Information                                  │
├─────────────────────────────────────────────────────┤
│ Name:         [Studio A                      ] ✎   │
│ Device ID:    a3f2c891-4d12-4b7e-9c3a-...  [Copy] │
│ Hardware:     Radxa Rock 5T                        │
│ Firmware:     iVault v0.1.0                        │
│ Armbian:      26.2.6 (Debian Trixie)               │
│ Kernel:       6.1.115-vendor-rk35xx                │
│ Uptime:       3 days, 14 hours                     │
│ Last seen:    Just now                             │
└─────────────────────────────────────────────────────┘
```

#### Network
```
┌─────────────────────────────────────────────────────┐
│ Network                                             │
├─────────────────────────────────────────────────────┤
│ Interface:    Ethernet (enP3p49s0)                  │
│ IP Address:   10.0.0.111                           │
│ MAC:          e8:4e:06:xx:xx:xx                    │
│ Link speed:   1 Gbps                               │
│                                                     │
│ DNS:          192.168.1.1                          │
│ Gateway:      10.0.0.1                             │
│                                                     │
│ Cloud connection: 🟢 Connected to api.ivault.net   │
│ Poll latency: 12ms                                 │
│                                                     │
│              [Reconfigure Network →]               │
└─────────────────────────────────────────────────────┘
```

#### Firmware Updates
```
┌─────────────────────────────────────────────────────┐
│ Firmware                                            │
├─────────────────────────────────────────────────────┤
│ Current:  v0.1.0                                   │
│ Latest:   v0.2.1  ← Update available               │
│                                                     │
│ Release notes:                                      │
│ • SMB destination support                          │
│ • Improved upload retry logic                      │
│ • Dashboard health metrics                         │
│                                                     │
│ Update will take ~2 minutes. Recording will        │
│ pause briefly during restart.                      │
│                                                     │
│              [Install Update]                      │
└─────────────────────────────────────────────────────┘
```

When up to date:
```
│ ✓ Firmware is up to date (v0.1.0)                  │
│ Auto-update:  [✓] Install updates automatically    │
```

#### Danger Zone
```
┌─────────────────────────────────────────────────────┐
│ Danger Zone                                         │
├─────────────────────────────────────────────────────┤
│ Restart iVault service                              │
│ Restarts the iVault process without rebooting.     │
│                              [Restart Service]     │
│                                                     │
│ Reboot Device                                       │
│ Fully reboots the Rock 5T.                         │
│                              [Reboot Device]       │
│                                                     │
│ Factory Reset                                       │
│ Removes all configuration. Device will need        │
│ re-provisioning.                                   │
│                              [Factory Reset]       │
│                                                     │
│ Remove Device                                       │
│ Removes device from your account permanently.      │
│                              [Remove Device]       │
└─────────────────────────────────────────────────────┘
```

---

### 10. Device — Logs `/devices/{id}/logs`

**Purpose:** Searchable, filterable log viewer.

#### Controls
```
[🔍 Search logs...        ] [Level: All ▾] [Component: All ▾] [Last 24h ▾] [⬇ Download]
```

#### Log Table
```
┌──────────────────┬───────┬───────────┬────────────────────────────────────────────┐
│ Timestamp        │ Level │ Component │ Message                                    │
├──────────────────┼───────┼───────────┼────────────────────────────────────────────┤
│ 20:32:14         │ INFO  │ upload    │ Uploaded 3 files to Studio-NAS             │
│ 20:31:58         │ INFO  │ gadget    │ Gadget reattached — state: configured      │
│ 20:31:44         │ INFO  │ ingest    │ Moved 3 files to upload queue              │
│ 20:31:44         │ INFO  │ ingest    │ Disk image unmounted                       │
│ 20:31:12         │ INFO  │ state     │ recording → detaching                      │
│ 20:30:00         │ INFO  │ state     │ Maintenance triggered (schedule)           │
│ 19:30:00         │ INFO  │ state     │ Maintenance triggered (schedule)           │
│ 08:14:33         │ WARN  │ upload    │ Retry 1/3: timeout connecting to NAS       │
│ 08:14:28         │ ERROR │ upload    │ Upload failed: connection refused           │
└──────────────────┴───────┴───────────┴────────────────────────────────────────────┘
```

Level badges: INFO (gray), WARN (yellow), ERROR (red)

**Live tail toggle:** [ ] Live (auto-refresh every 5s)

---

### 11. Add Device Wizard `/devices/new`

**Purpose:** Guided flow to provision a new device. 4 steps.

---

**Step 1 of 4 — Name Your Device**
```
┌─────────────────────────────────────────────────────┐
│ ● ─ ─ ─                           Add New Device   │
├─────────────────────────────────────────────────────┤
│                                                     │
│         What would you like to call it?             │
│                                                     │
│         [Studio A                          ]        │
│                                                     │
│         Use a name that describes its location      │
│         or purpose (e.g. "Main Stage", "Booth B")   │
│                                                     │
│                              [Next →]               │
└─────────────────────────────────────────────────────┘
```

---

**Step 2 of 4 — Network**
```
┌─────────────────────────────────────────────────────┐
│ ─ ● ─ ─                           Add New Device   │
├─────────────────────────────────────────────────────┤
│                                                     │
│  Connection type:                                   │
│  ● Wired (Ethernet)    ○ WiFi                      │
│                                                     │
│  IP Configuration:                                  │
│  ● DHCP (automatic)    ○ Static IP                 │
│                                                     │
│  ── If Static ──────────────────────────────────── │
│  IP Address:  [192.168.1.100        ]               │
│  Subnet mask: [255.255.255.0        ]               │
│  Gateway:     [192.168.1.1          ]               │
│  DNS:         [1.1.1.1              ]               │
│                                                     │
│  ── If WiFi ────────────────────────────────────── │
│  SSID:        [MyNetwork            ]               │
│  Password:    [••••••••••••         ]               │
│  Band:        [Auto ▾]                             │
│                                                     │
│              [← Back]          [Next →]            │
└─────────────────────────────────────────────────────┘
```

---

**Step 3 of 4 — Storage Destination**
```
┌─────────────────────────────────────────────────────┐
│ ─ ─ ● ─                           Add New Device   │
├─────────────────────────────────────────────────────┤
│                                                     │
│  Where should recorded files be sent?               │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │ 🖥  SMB / NAS                               │   │
│  │    Network share, Synology, QNAP, etc       │   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │ ☁  Google Drive                            │   │
│  │    Upload to Google Drive folder            │   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │ ⏭  Skip for now                            │   │
│  │    Configure later in settings              │   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  (Selecting SMB or Google Drive opens              │
│   configuration inline before continuing)          │
│                                                     │
│              [← Back]          [Next →]            │
└─────────────────────────────────────────────────────┘
```

---

**Step 4 of 4 — Download Provision File**
```
┌─────────────────────────────────────────────────────┐
│ ─ ─ ─ ●                           Add New Device   │
├─────────────────────────────────────────────────────┤
│                                                     │
│  ✓ Configuration ready for "Studio A"              │
│                                                     │
│  Follow these steps:                               │
│                                                     │
│  1  Connect iVault to your computer via USB-C      │
│     The drive will appear as "IVAULT"              │
│                                                     │
│  2  [⬇ Download ivault-provision.json]             │
│     Copy this file to the root of the IVAULT drive │
│                                                     │
│  3  Eject the IVAULT drive safely                  │
│     The device will read the file automatically    │
│                                                     │
│  4  Wait ~30 seconds                               │
│     The device will configure itself and           │
│     appear as Online in your dashboard             │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │ ⏳ Waiting for device to come online...     │   │
│  │    This page will update automatically.     │   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  Provision file expires in: 28:44                  │
│                                                     │
│              [← Back]    [Go to Dashboard]         │
└─────────────────────────────────────────────────────┘
```

After device connects:
```
│  ┌─────────────────────────────────────────────┐   │
│  │ 🟢 Studio A is online!                      │   │
│  │                      [Go to Device →]       │   │
│  └─────────────────────────────────────────────┘   │
```

---

### 12. Account Settings `/account`

**Sections:**

**Profile**
- Name, email, change password

**Billing** (future)
- Plan: Free / Pro / Business
- Device limit, storage, retention

**Notifications**
```
Notify me when:
[✓] A device goes offline
[✓] An upload fails after all retries
[✓] Storage is above 80%
[ ] Maintenance cycle completes
[✓] Firmware update available

Notification method:
● Email   ○ SMS (Pro)   ○ Webhook (Business)
```

**API Access** (advanced)
- Generate API key for custom integrations
- Webhook URL for events

**Danger Zone**
- Delete account

---

## Global UI Elements

### Top Navigation
```
[iVault logo]   Dashboard   ──────────────   [🔔 2]  [Umar ▾]
```

Notification bell shows count of unread alerts (offline devices, failed uploads).

User menu: Account Settings, Documentation, Sign Out.

### Device Sidebar (when inside a device)
```
← All Devices

📱 Studio A
   🟢 Online

─────────────
  Overview
  Storage
  Destinations
  Schedule
  System
  Logs
─────────────
  + Add Device
```

### Empty States
Every list/table has a helpful empty state:
- No devices → "Add your first device"
- No logs → "No log entries yet"
- No uploads → "All caught up — nothing in the queue"
- Destination unreachable → "Unable to reach Studio-NAS. Check network."

### Confirmation Dialogs
Destructive actions (Reboot, Factory Reset, Remove Device) require:
```
┌─────────────────────────────────────┐
│ Reboot Studio A?                    │
│                                     │
│ The device will be offline for      │
│ approximately 30 seconds. Any       │
│ active recording will be paused.    │
│                                     │
│        [Cancel]  [Reboot Device]    │
└─────────────────────────────────────┘
```

---

## Destination Types

| Type | Status | Notes |
|------|--------|-------|
| SMB / CIFS | ✅ v0.1 | Synology, QNAP, Windows shares, TrueNAS |
| Google Drive | ✅ v0.1 | OAuth via portal |
| Local path | ✅ v0.1 | Any path mounted on device |
| FTP / SFTP | 🗓 Roadmap | |
| Amazon S3 | 🗓 Roadmap | |
| Backblaze B2 | 🗓 Roadmap | |
| Dropbox | 🗓 Roadmap | |
| OneDrive | 🗓 Roadmap | |

---

## Provisioning File Format

The downloaded `ivault-provision.json` is AES-256-GCM encrypted. Plaintext schema:

```json
{
  "version": 1,
  "device_id": "a3f2c891-4d12-4b7e-9c3a-1234567890ab",
  "device_name": "Studio A",
  "token": "eyJ...",
  "token_expires": "2026-05-11T21:30:00Z",
  "cloud_endpoint": "https://api.ivault.net",
  "network": {
    "mode": "dhcp",
    "interface": "eth0"
  }
}
```

WiFi variant:
```json
{
  "network": {
    "mode": "dhcp",
    "interface": "wlan0",
    "wifi_ssid": "MyNetwork",
    "wifi_password": "••••••••"
  }
}
```

Static IP variant:
```json
{
  "network": {
    "mode": "static",
    "interface": "eth0",
    "ip": "192.168.1.100",
    "subnet": "255.255.255.0",
    "gateway": "192.168.1.1",
    "dns": ["1.1.1.1", "8.8.8.8"]
  }
}
```

**Security:**
- File is encrypted with AES-256-GCM
- Encryption key derived from one-time token (HKDF)
- Token expires 30 minutes after generation
- Token is single-use — invalidated on first device connection
- File is deleted from the drive by the device after reading

---

## Device Status — State Reference

| State | Badge | Description |
|-------|-------|-------------|
| `recording` | 🟢 Recording | Gadget attached, device can write |
| `detaching` | 🟡 Syncing | Gracefully detaching gadget |
| `maintenance` | 🟡 Syncing | Mounting, moving files |
| `attaching` | 🟡 Syncing | Re-attaching gadget |
| `uploading` | 🟡 Uploading | Background upload in progress |
| `error` | 🔴 Error | Error state, check logs |
| `offline` | ⚫ Offline | Not seen in >2 minutes |
| `provisioning` | ⚙️ Provisioning | Config downloaded, not yet connected |

---

## Responsiveness

| Breakpoint | Layout |
|------------|--------|
| Mobile (<768px) | Single column, bottom nav |
| Tablet (768–1024px) | Sidebar collapsed to icons |
| Desktop (>1024px) | Full sidebar + content |

Mobile priority screens: Dashboard, Overview, Logs (field use — check status on phone).

---

*iVault Portal UI Spec v0.1.0*
