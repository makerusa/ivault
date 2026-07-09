#!/usr/bin/env bash
# ==============================================================================
# MakerUSA Relay — Universal Installer
# ------------------------------------------------------------------------------
# One script for every supported board. It detects the hardware, chooses safe
# defaults wherever there is no real choice, and prompts only when a decision
# is genuinely yours (which NVMe to use, how much space to allocate).
#
# Ideal flow:
#   flash Armbian minimal -> clone repo -> sudo ./scripts/install.sh
#
# Targets any RK35xx board with a USB-C OTG (device-mode) port, e.g.:
#   - Radxa Rock 5T (RK3588, eMMC + 2x NVMe)
#   - Seeed reComputer / other RK3576 boards (SD boot, 1x NVMe, no eMMC)
#
# It NEVER touches the disk you booted from, and it does NOT migrate your OS
# root on its own (that can brick a headless board). Instead it detects the
# best boot target and prints the exact steps for you to run later.
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# ---- Tunables / flags --------------------------------------------------------
ASSUME_YES=0          # --yes            skip all confirmations (uses defaults)
OTG_DISK=""           # --otg-disk=DEV   force the external-drive NVMe
INTERNAL_DISK=""      # --internal-disk=DEV
EXTERNAL_SIZE=""      # --external-size=NNNG   external image size (single-disk)
UDC_OVERRIDE=""       # --udc=NAME       force the USB Device Controller name
DRIVE_LABEL="RELAY"   # --label=NAME     volume label shown on the host
INSTALL_SAMBA=1       # --no-samba       skip the local NAS share
DO_BUILD=1            # --no-build       skip compiling (binary already in place)
GO_VERSION="1.25.0"

MOUNT_ROOT="/nvme"    # internal storage mount point
CONFIG_DIR="/etc/ivault"
CONFIG_FILE="${CONFIG_DIR}/config.json"
BIN_PATH="/usr/local/bin/ivault"

# ---- Pretty output -----------------------------------------------------------
c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
c_grn=$'\033[32m'; c_ylw=$'\033[33m'; c_red=$'\033[31m'; c_cyn=$'\033[36m'
info() { echo "${c_cyn}▸${c_reset} $*"; }
ok()   { echo "${c_grn}✓${c_reset} $*"; }
warn() { echo "${c_ylw}⚠${c_reset}  $*" >&2; }
die()  { echo "${c_red}✗ $*${c_reset}" >&2; exit 1; }
hr()   { echo "${c_dim}────────────────────────────────────────────────────────${c_reset}"; }

# ask VAR "Prompt" "default"  -> reads into VAR, honours --yes (uses default)
ask() {
    local __var="$1" __prompt="$2" __default="${3:-}" __reply=""
    if [ "$ASSUME_YES" = "1" ]; then printf -v "$__var" '%s' "$__default"; return; fi
    if [ -n "$__default" ]; then
        read -r -p "$__prompt [$__default]: " __reply || true
    else
        read -r -p "$__prompt: " __reply || true
    fi
    printf -v "$__var" '%s' "${__reply:-$__default}"
}

# confirm "Prompt"  -> returns 0 for yes. Honours --yes (auto-yes).
confirm() {
    if [ "$ASSUME_YES" = "1" ]; then return 0; fi
    local __reply=""
    read -r -p "$1 [y/N]: " __reply || true
    [[ "$__reply" =~ ^[Yy]$ ]]
}

# ---- Arg parsing -------------------------------------------------------------
for arg in "$@"; do
    case "$arg" in
        --yes|-y)            ASSUME_YES=1 ;;
        --otg-disk=*)        OTG_DISK="${arg#*=}" ;;
        --internal-disk=*)   INTERNAL_DISK="${arg#*=}" ;;
        --external-size=*)   EXTERNAL_SIZE="${arg#*=}" ;;
        --udc=*)             UDC_OVERRIDE="${arg#*=}" ;;
        --label=*)           DRIVE_LABEL="${arg#*=}" ;;
        --no-samba)          INSTALL_SAMBA=0 ;;
        --no-build)          DO_BUILD=0 ;;
        -h|--help)
            grep '^# ' "$0" | sed 's/^# \{0,1\}//' | head -30
            exit 0 ;;
        *) die "unknown argument: $arg (try --help)" ;;
    esac
done

[ "$EUID" -eq 0 ] || die "please run as root (sudo $0)"

echo
echo "${c_bold}MakerUSA Relay Installer${c_reset}"
hr

# ==============================================================================
# 1. HARDWARE DETECTION
# ==============================================================================
info "Detecting hardware..."

# --- Boot / root disk (never to be touched) ---
ROOT_SRC="$(findmnt -n -o SOURCE / 2>/dev/null || echo '')"
# strip partition suffix to get the parent disk (/dev/nvme0n1p2 -> /dev/nvme0n1,
# /dev/mmcblk1p1 -> /dev/mmcblk1, /dev/sda2 -> /dev/sda)
disk_of() {
    local part="$1"
    if [[ "$part" =~ ^/dev/(nvme[0-9]+n[0-9]+|mmcblk[0-9]+)p[0-9]+$ ]]; then
        echo "/dev/${BASH_REMATCH[1]}"
    else
        echo "$part" | sed -E 's/[0-9]+$//'
    fi
}
ROOT_DISK="$(disk_of "$ROOT_SRC")"

# --- eMMC: the mmcblk device that owns bootN hardware partitions ---
EMMC_DISK=""
for b in /dev/mmcblk*boot0; do
    [ -e "$b" ] || continue
    EMMC_DISK="/dev/$(basename "$b" | sed 's/boot0$//')"
    break
done

# --- NVMe whole disks ---
mapfile -t NVME_DISKS < <(lsblk -dpno NAME,TYPE 2>/dev/null | awk '$2=="disk" && $1 ~ /nvme/ {print $1}')

# --- USB Device Controller (OTG port in peripheral mode) ---
UDC_NAME="$UDC_OVERRIDE"
if [ -z "$UDC_NAME" ]; then
    UDC_NAME="$(ls /sys/class/udc/ 2>/dev/null | head -1 || true)"
fi

size_of() { lsblk -dno SIZE "$1" 2>/dev/null | tr -d ' '; }

echo
echo "  ${c_bold}Boot / root:${c_reset} ${ROOT_SRC:-unknown}  (disk: ${ROOT_DISK:-unknown})"
echo "  ${c_bold}eMMC:${c_reset}        ${EMMC_DISK:-none detected}"
if [ "${#NVME_DISKS[@]}" -eq 0 ]; then
    echo "  ${c_bold}NVMe:${c_reset}        none detected"
else
    for d in "${NVME_DISKS[@]}"; do
        tag=""; [ "$d" = "$ROOT_DISK" ] && tag=" ${c_ylw}(boot/root — off limits)${c_reset}"
        echo "  ${c_bold}NVMe:${c_reset}        $d  ($(size_of "$d"))$tag"
    done
fi
echo "  ${c_bold}USB OTG UDC:${c_reset} ${UDC_NAME:-${c_red}NONE${c_reset}}"
echo

# --- USB gadget capability gate ---
if [ -z "$UDC_NAME" ]; then
    warn "No USB Device Controller found under /sys/class/udc/."
    warn "The OTG-C port is not in peripheral/device mode, so Relay cannot"
    warn "present itself as a USB drive yet. This is usually a device-tree"
    warn "issue: the dwc3/usb node needs  dr_mode = \"peripheral\"  (or a working"
    warn "OTG role switch) for this board. Fix that, then re-run the installer."
    echo
    if ! confirm "Continue with storage/service setup anyway?"; then
        die "aborted — resolve OTG peripheral mode first"
    fi
    UDC_NAME="fc000000.usb"   # placeholder; edit ${CONFIG_FILE} once known
    warn "Writing placeholder udc_name='${UDC_NAME}' — correct it in ${CONFIG_FILE}."
fi

# ==============================================================================
# 2. BOOT TARGET ADVICE  (detect, recommend, DO NOT perform)
# ==============================================================================
booted_from="unknown"
case "$ROOT_DISK" in
    "$EMMC_DISK") booted_from="eMMC" ;;
    /dev/nvme*)   booted_from="NVMe" ;;
    /dev/mmcblk*) booted_from="microSD" ;;
esac

if [ "$booted_from" = "microSD" ]; then
    hr
    info "You are booting from microSD."
    if [ -n "$EMMC_DISK" ]; then
        echo "  This board has eMMC (${EMMC_DISK}). For a card-free, more reliable"
        echo "  deployment you can migrate the OS to eMMC later with:"
        echo "      ${c_dim}sudo armbian-install   # choose: system on eMMC${c_reset}"
    elif [ "${#NVME_DISKS[@]}" -gt 0 ]; then
        echo "  No eMMC on this board. You can optionally move the OS to NVMe later"
        echo "  with  ${c_dim}sudo armbian-install${c_reset}  (choose the NVMe target). If you do,"
        echo "  re-run this installer afterward so storage is laid out around the OS."
    fi
    echo "  ${c_dim}(The installer does not migrate the root filesystem itself — that"
    echo "   step is interactive and best done deliberately.)${c_reset}"
    echo "  Leaving the OS on microSD is fully supported; NVMe will be used for storage."
    echo
fi

# ==============================================================================
# 3. STORAGE PLAN
# ==============================================================================
hr
info "Planning storage layout..."

# Candidate disks for storage = NVMe disks that are NOT the boot/root disk.
STORAGE_CANDIDATES=()
for d in "${NVME_DISKS[@]}"; do
    [ "$d" = "$ROOT_DISK" ] && continue
    STORAGE_CANDIDATES+=("$d")
done

# Backing modes:
#   BACKING_MODE=whole  -> OTG_DISK is a whole NVMe formatted exFAT (superfloppy)
#   BACKING_MODE=image  -> IMAGE_PATH is an exFAT image file on the internal ext4
BACKING_MODE=""
IMAGE_PATH=""
INTERNAL_TARGET=""     # whole NVMe disk to become /nvme (ext4), or "" if reusing root

pick_disk() {   # pick_disk VAR "prompt" disk1 disk2 ...
    local __var="$1"; shift
    local __prompt="$1"; shift
    local -a opts=("$@")
    if [ "${#opts[@]}" -eq 1 ]; then printf -v "$__var" '%s' "${opts[0]}"; return; fi
    echo "$__prompt"
    local i=1
    for o in "${opts[@]}"; do echo "    $i) $o  ($(size_of "$o"))"; i=$((i+1)); done
    local sel=""
    ask sel "  choose 1-${#opts[@]}" "1"
    [[ "$sel" =~ ^[0-9]+$ ]] && [ "$sel" -ge 1 ] && [ "$sel" -le "${#opts[@]}" ] || sel=1
    printf -v "$__var" '%s' "${opts[$((sel-1))]}"
}

if [ "${#STORAGE_CANDIDATES[@]}" -ge 2 ]; then
    # --- Dual (or more) NVMe: dedicate one whole disk to OTG, one to internal ---
    if [ -n "$OTG_DISK" ]; then :; else
        pick_disk OTG_DISK "  Which NVMe should be the EXTERNAL USB drive (dedicated, whole disk)?" "${STORAGE_CANDIDATES[@]}"
    fi
    remaining=()
    for d in "${STORAGE_CANDIDATES[@]}"; do [ "$d" = "$OTG_DISK" ] || remaining+=("$d"); done
    if [ -n "$INTERNAL_DISK" ]; then :; else
        pick_disk INTERNAL_DISK "  Which NVMe should hold INTERNAL storage (ingest/queue/DB, ext4)?" "${remaining[@]}"
    fi
    BACKING_MODE="whole"
    IMAGE_PATH="$OTG_DISK"
    INTERNAL_TARGET="$INTERNAL_DISK"

elif [ "${#STORAGE_CANDIDATES[@]}" -eq 1 ]; then
    # --- Single storage NVMe: one ext4 for internal + an exFAT image for external ---
    INTERNAL_TARGET="${STORAGE_CANDIDATES[0]}"
    BACKING_MODE="image"
    IMAGE_PATH="${MOUNT_ROOT}/usb_disk.img"
    total_gib="$(lsblk -bdno SIZE "$INTERNAL_TARGET" | awk '{printf "%d", $1/1024/1024/1024}')"
    default_ext="$(( total_gib * 65 / 100 ))G"
    [ -n "$EXTERNAL_SIZE" ] || ask EXTERNAL_SIZE \
        "  ${INTERNAL_TARGET} is ${total_gib}G. How big should the EXTERNAL drive be? (rest is internal buffer)" \
        "$default_ext"

else
    # --- No spare NVMe. Fall back to an image file on the existing filesystem. ---
    warn "No NVMe available for storage (only the boot disk was found)."
    BACKING_MODE="image"
    INTERNAL_TARGET=""                       # reuse current root filesystem
    MOUNT_ROOT="/var/lib/ivault-storage"
    IMAGE_PATH="${MOUNT_ROOT}/usb_disk.img"
    avail_g="$(df -BG --output=avail / | tail -1 | tr -dc '0-9')"
    default_ext="$(( avail_g * 50 / 100 ))G"
    warn "External drive will be an image file on the boot device (slower, less durable)."
    [ -n "$EXTERNAL_SIZE" ] || ask EXTERNAL_SIZE \
        "  ~${avail_g}G free on /. How big should the EXTERNAL drive image be?" "$default_ext"
fi

INGEST_DIR="${MOUNT_ROOT}/ingest"
QUEUE_DIR="${MOUNT_ROOT}/upload_queue"
DB_DIR="${MOUNT_ROOT}/ivault"          # DB on storage, not the SD card, for durability
DB_PATH="${DB_DIR}/ivault.db"

# --- Show the plan and get one explicit confirmation before destroying data ---
echo
echo "${c_bold}Storage plan${c_reset}"
echo "  External drive backing : ${IMAGE_PATH}   ${c_dim}(${BACKING_MODE})${c_reset}"
[ "$BACKING_MODE" = "image" ] && echo "  External drive size    : ${EXTERNAL_SIZE}"
if [ -n "$INTERNAL_TARGET" ]; then
    echo "  Internal storage       : ${INTERNAL_TARGET} -> ext4 -> ${MOUNT_ROOT}"
else
    echo "  Internal storage       : ${MOUNT_ROOT} (on the existing root filesystem)"
fi
echo "  Ingest / queue / DB    : ${INGEST_DIR}, ${QUEUE_DIR}, ${DB_PATH}"
echo "  USB gadget UDC         : ${UDC_NAME}"
echo

destroys=()
[ "$BACKING_MODE" = "whole" ] && destroys+=("$OTG_DISK (whole disk -> exFAT)")
[ -n "$INTERNAL_TARGET" ] && destroys+=("$INTERNAL_TARGET (whole disk -> ext4)")
if [ "${#destroys[@]}" -gt 0 ]; then
    warn "The following disks will be WIPED:"
    for d in "${destroys[@]}"; do echo "      - $d"; done
    # Safety: never allow the root/boot disk into the wipe set.
    for d in "$OTG_DISK" "$INTERNAL_TARGET"; do
        [ -n "$d" ] && [ "$d" = "$ROOT_DISK" ] && die "refusing to format the boot/root disk ($d)"
    done
    confirm "Proceed and ERASE the disks listed above?" || die "aborted by user"
fi

# ==============================================================================
# 4. APT DEPENDENCIES
# ==============================================================================
hr
info "Installing system packages..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
pkgs=(exfatprogs exfat-fuse rclone sqlite3 gcc libc6-dev wget tar parted)
[ "$INSTALL_SAMBA" = "1" ] && pkgs+=(samba avahi-daemon)
apt-get install -y "${pkgs[@]}"
ok "packages installed"

# ==============================================================================
# 5. APPLY STORAGE LAYOUT
# ==============================================================================
hr
info "Applying storage layout..."

# If a previous install is running, stop it first so it releases the internal
# volume (SQLite DB + upload queue are open) and the USB gadget before we
# unmount/reformat. Without this a re-install fails with "target is busy".
# Safe (no-op) on a first install.
if systemctl list-unit-files 2>/dev/null | grep -q '^ivault\.service'; then
    info "Stopping running Relay service before reformatting storage..."
    systemctl stop ivault.service 2>/dev/null || true
fi

format_ext4_and_mount() {   # format_ext4_and_mount <disk> <mountpoint>
    local disk="$1" mnt="$2" part="${1}p1"
    # ALWAYS start from a clean, full-disk GPT partition. The storage-plan
    # confirmation already warned this disk will be wiped, and the format below
    # is unconditional anyway — so there is no data to preserve here.
    #
    # We must NOT reuse a pre-existing partition table: a disk that arrives with
    # a foreign layout (e.g. a Windows GPT with a ~16 MB "Microsoft reserved"
    # partition as p1) would otherwise get that tiny first partition formatted as
    # the internal volume, leaving almost no space and breaking ingest with
    # "no space left on device".
    info "Partitioning ${disk} (single full-disk GPT partition)..."
    umount "${disk}"* 2>/dev/null || true
    wipefs -a -f "$disk" >/dev/null
    parted -s "$disk" mklabel gpt mkpart primary ext4 1MiB 100%
    partprobe "$disk"; udevadm settle
    info "Formatting ${part} as ext4..."
    mkfs.ext4 -F "$part" >/dev/null
    mkdir -p "$mnt"
    local uuid; uuid="$(blkid -s UUID -o value "$part")"
    # Drop any stale fstab entry for this mountpoint (an old UUID from a previous
    # install would otherwise linger), then add the fresh one.
    sed -i "\| ${mnt} |d" /etc/fstab
    echo "UUID=$uuid $mnt ext4 defaults,nofail 0 2" >> /etc/fstab
    mountpoint -q "$mnt" && umount "$mnt" 2>/dev/null || true
    mount "$mnt"
    ok "internal storage ready at ${mnt} ($(size_of "$disk"))"
}

if [ -n "$INTERNAL_TARGET" ]; then
    format_ext4_and_mount "$INTERNAL_TARGET" "$MOUNT_ROOT"
else
    mkdir -p "$MOUNT_ROOT"
fi

mkdir -p "$INGEST_DIR" "$QUEUE_DIR" "$DB_DIR" "$CONFIG_DIR"

if [ "$BACKING_MODE" = "whole" ]; then
    info "Formatting ${OTG_DISK} as a whole-device exFAT superfloppy (label ${DRIVE_LABEL})..."
    umount "$INGEST_DIR" 2>/dev/null || true
    wipefs -a -f "$OTG_DISK" >/dev/null
    mkfs.exfat -L "$DRIVE_LABEL" "$OTG_DISK" >/dev/null
    ok "external drive ready: ${OTG_DISK}"
else
    if [ ! -f "$IMAGE_PATH" ]; then
        info "Creating ${EXTERNAL_SIZE} exFAT image at ${IMAGE_PATH}..."
        fallocate -l "$EXTERNAL_SIZE" "$IMAGE_PATH" \
            || die "fallocate failed — not enough space for ${EXTERNAL_SIZE}?"
        mkfs.exfat -L "$DRIVE_LABEL" "$IMAGE_PATH" >/dev/null
        ok "external drive image ready: ${IMAGE_PATH}"
    else
        warn "${IMAGE_PATH} already exists — leaving it as-is."
    fi
fi

# ==============================================================================
# 6. USB GADGET KERNEL MODULE
# ==============================================================================
hr
info "Enabling USB gadget support (libcomposite)..."
modprobe libcomposite 2>/dev/null || warn "could not modprobe libcomposite now (will load at boot)"
echo "libcomposite" > /etc/modules-load.d/ivault.conf
mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config 2>/dev/null || true
ok "libcomposite configured to load at boot"

# Rockchip/Seeed vendor images ship usbdevice.service, which builds its own
# USB gadget at boot and claims the single UDC before iVault can — leaving
# iVault's gadget unbound and invisible to the host. Disable it and tear down
# any gadget it already created so iVault owns the controller.
# `disable` is not enough here: these units can be pulled back in by a target
# or udev rule on the next boot. `mask` makes them impossible to start. The
# ExecStartPre reclaim guard on ivault.service (below) is the real backstop —
# it works even if some image spawns a gadget by a mechanism we don't know.
for svc in usbdevice usb-gadget; do
    if systemctl list-unit-files 2>/dev/null | grep -q "^${svc}\.service"; then
        info "Neutralizing competing USB gadget service: ${svc}.service"
        systemctl stop "${svc}.service" >/dev/null 2>&1 || true
        systemctl disable "${svc}.service" >/dev/null 2>&1 || true
        # Vendor images ship this as a REAL unit file in /etc (not a /lib
        # symlink), so `systemctl mask` refuses to overwrite it. Move the real
        # file (and any drop-ins) aside, then mask so it can never start again.
        unit="/etc/systemd/system/${svc}.service"
        if [ -f "$unit" ] && [ ! -L "$unit" ]; then
            mv "$unit" "${unit}.disabled-by-ivault"
        fi
        rm -rf "/etc/systemd/system/${svc}.service.d"
        systemctl daemon-reload >/dev/null 2>&1 || true
        systemctl mask "${svc}.service" >/dev/null 2>&1 || true
    fi
done
for g in /sys/kernel/config/usb_gadget/*/; do
    [ -e "$g" ] || continue
    case "$g" in
        */ivault/) continue ;;
    esac
    if [ -s "${g}UDC" ]; then
        info "Releasing UDC held by stale gadget: $(basename "$g")"
        echo "" > "${g}UDC" 2>/dev/null || true
    fi
done

# ==============================================================================
# 7. BUILD THE BINARY
# ==============================================================================
if [ "$DO_BUILD" = "1" ]; then
    hr
    info "Building the Relay agent..."
    if ! command -v go >/dev/null 2>&1 && [ ! -x /usr/local/go/bin/go ]; then
        arch="$(uname -m)"; goarch="arm64"
        [ "$arch" = "x86_64" ] && goarch="amd64"
        info "Installing Go ${GO_VERSION} (${goarch})..."
        tarball="go${GO_VERSION}.linux-${goarch}.tar.gz"
        wget -q "https://go.dev/dl/${tarball}" -O "/tmp/${tarball}"
        rm -rf /usr/local/go
        tar -C /usr/local -xzf "/tmp/${tarball}"
        rm -f "/tmp/${tarball}"
        echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
    fi
    GO_BIN="$(command -v go || echo /usr/local/go/bin/go)"
    ( cd "$REPO_ROOT" && CGO_ENABLED=1 "$GO_BIN" build -o "$BIN_PATH" . )
    ok "binary built: ${BIN_PATH}"
else
    [ -x "$BIN_PATH" ] || warn "--no-build set but ${BIN_PATH} is missing"
fi

# ==============================================================================
# 8. WRITE CONFIG
# ==============================================================================
hr
info "Writing ${CONFIG_FILE}..."
# Preserve provisioning identity if this is a re-install.
uid=""; did=""; dkey=""; endp=""
if [ -f "$CONFIG_FILE" ] && command -v sqlite3 >/dev/null; then :; fi
if [ -f "$CONFIG_FILE" ]; then
    uid="$(grep -o '"user_id"[^,]*' "$CONFIG_FILE" | sed 's/.*: *"\(.*\)".*/\1/' || true)"
    did="$(grep -o '"device_id"[^,]*' "$CONFIG_FILE" | sed 's/.*: *"\(.*\)".*/\1/' || true)"
    dkey="$(grep -o '"device_api_key"[^,]*' "$CONFIG_FILE" | sed 's/.*: *"\(.*\)".*/\1/' || true)"
    endp="$(grep -o '"cloud_endpoint"[^,]*' "$CONFIG_FILE" | sed 's/.*: *"\(.*\)".*/\1/' || true)"
fi
cat > "$CONFIG_FILE" <<EOF
{
  "user_id": "${uid}",
  "device_id": "${did}",
  "device_api_key": "${dkey}",
  "cloud_endpoint": "${endp}",
  "db_path": "${DB_PATH}",
  "image_path": "${IMAGE_PATH}",
  "mount_point": "${INGEST_DIR}",
  "upload_queue": "${QUEUE_DIR}",
  "udc_name": "${UDC_NAME}",
  "rclone_remote": "gdrive",
  "rclone_path": "Relay",
  "upload_workers": 2,
  "schedule_mode": "daily",
  "schedule_interval_minutes": 60,
  "schedule_window_start_hour": 2,
  "schedule_window_end_hour": 5,
  "retention_enabled": false,
  "retention_threshold_percent": 80,
  "led_enabled": true,
  "led_name": "user-led"
}
EOF
ok "config written"

# ==============================================================================
# 9. SAMBA (optional local NAS view of internal storage)
# ==============================================================================
if [ "$INSTALL_SAMBA" = "1" ]; then
    hr
    info "Configuring local NAS share (ivault-storage -> ${MOUNT_ROOT})..."
    if ! grep -q "\[ivault-storage\]" /etc/samba/smb.conf 2>/dev/null; then
        cat >> /etc/samba/smb.conf <<EOT

[ivault-storage]
   comment = MakerUSA Relay local storage
   path = ${MOUNT_ROOT}
   browseable = yes
   read only = no
   guest ok = yes
   create mask = 0664
   directory mask = 0775
EOT
    fi
    systemctl enable --now smbd >/dev/null 2>&1 || true
    systemctl enable --now avahi-daemon >/dev/null 2>&1 || true
    ok "NAS share configured"
fi

# ==============================================================================
# 10. SYSTEMD SERVICE
# ==============================================================================
hr
info "Installing systemd service..."

# Pre-start guard, kept in a standalone script (NOT inline in the unit) so
# systemd's own ${} expansion can't mangle the shell variables. It waits for a
# USB Device Controller to exist, then unbinds it from any non-iVault gadget so
# iVault can claim it — the backstop that guarantees iVault wins regardless of
# what the base image does.
cat > /usr/local/bin/ivault-udc-guard.sh <<'GUARD'
#!/bin/sh
# Wait up to ~30s for a UDC to appear (dwc3 may probe after we start).
for _ in $(seq 1 30); do
    ls /sys/class/udc/ 2>/dev/null | grep -q . && break
    sleep 1
done
# Release the controller from any gadget that isn't iVault's.
for g in /sys/kernel/config/usb_gadget/*/; do
    [ -e "$g" ] || continue
    [ "$g" = /sys/kernel/config/usb_gadget/ivault/ ] && continue
    echo "" > "${g}UDC" 2>/dev/null || true
done
exit 0
GUARD
chmod +x /usr/local/bin/ivault-udc-guard.sh

cat > /etc/systemd/system/ivault.service <<EOT
[Unit]
Description=MakerUSA Relay - Intelligent USB Storage Appliance
After=local-fs.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/usr/local/bin/ivault-udc-guard.sh
ExecStart=${BIN_PATH} --config ${CONFIG_FILE}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOT
systemctl daemon-reload
systemctl enable ivault.service >/dev/null
systemctl restart ivault.service || warn "service failed to start — check: journalctl -u ivault -e"

# ==============================================================================
# DONE
# ==============================================================================
echo
hr
ok "${c_bold}MakerUSA Relay installation complete.${c_reset}"
echo
echo "  Status : ${c_dim}systemctl status ivault${c_reset}"
echo "  Logs   : ${c_dim}journalctl -u ivault -f${c_reset}"
echo "  Gadget : ${c_dim}cat /sys/class/udc/${UDC_NAME}/state${c_reset}"
echo
echo "  Next: provision the device to your portal by dropping an"
echo "  ${c_bold}ivault.provision${c_reset} file onto the external drive, or set user_id/"
echo "  device_id/device_api_key/cloud_endpoint in ${CONFIG_FILE}."
if [ "$UDC_NAME" = "fc000000.usb" ] && [ -z "$UDC_OVERRIDE" ] && [ ! -e "/sys/class/udc/fc000000.usb" ]; then
    echo
    warn "Remember: udc_name is a PLACEHOLDER. Set it to your board's real UDC"
    warn "(ls /sys/class/udc/) in ${CONFIG_FILE}, then: systemctl restart ivault"
fi
echo
