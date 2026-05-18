#!/usr/bin/env bash
# ==============================================================================
# iVault "Relay" Appliance Automated Provisioning Script
# ==============================================================================
set -euo pipefail

# Get absolute path of the repository root before any directory changes
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# 1. Prerequisite Validation
if [ "$EUID" -ne 0 ]; then
    echo "❌ Error: Please run this script as root (sudo)." >&2
    exit 1
fi

echo "🚀 Starting Relay Appliance initialization..."

# 2. Hostname setup
echo "📝 Configuring network hostname..."
hostnamectl set-hostname relay

# 3. Create relay service user & group
if ! getent group relay >/dev/null; then
    echo "👤 Creating relay system group..."
    groupadd -g 2000 relay
fi
if ! getent passwd relay >/dev/null; then
    echo "👤 Creating relay system user..."
    useradd -u 2000 -g relay -d /var/lib/ivault -s /usr/sbin/nologin -c "Relay Service User" relay
fi

# 4. Install Apt Dependencies
echo "📦 Installing system dependencies..."
apt-get update
apt-get install -y samba avahi-daemon exfatprogs exfat-fuse rclone sqlite3 gcc libc6-dev wget tar

# 5. NVMe Storage Mount Setup (Ext4 Partition)
echo "💾 Configuring 1.8TB NVMe storage (/dev/nvme1n1)..."
TARGET_PART="/dev/nvme1n1p1"

if [ -b "/dev/nvme1n1" ]; then
    if [ ! -b "$TARGET_PART" ]; then
        echo "🔨 Partitioning /dev/nvme1n1..."
        echo -e "g\nn\n1\n\n\nw" | fdisk /dev/nvme1n1
        udevadm settle
    fi
    
    # Format ext4 if no existing filesystem is detected
    if ! blkid "$TARGET_PART" >/dev/null; then
        echo "✨ Formatting $TARGET_PART as ext4..."
        mkfs.ext4 -F "$TARGET_PART"
    fi

    # Mount setup
    mkdir -p /nvme
    if ! mountpoint -q /nvme; then
        mount "$TARGET_PART" /nvme
    fi

    # Fstab persistence check
    UUID=$(blkid -s UUID -o value "$TARGET_PART")
    if ! grep -q "$UUID" /etc/fstab; then
        echo "UUID=$UUID /nvme ext4 defaults 0 2" >> /etc/fstab
    fi
else
    echo "⚠️ Warning: Physical NVMe drive /dev/nvme1n1 not found. Skipping hardware format."
fi

# Create directory tree & permissions
mkdir -p /nvme/ingest /nvme/upload_queue /var/lib/ivault /etc/ivault
chown -R relay:relay /nvme /var/lib/ivault /etc/ivault

# 6. Configure exFAT OTG drive label
if [ -b "/dev/nvme0n1p1" ]; then
    echo "🏷️ Setting exFAT volume label on OTG drive to 'RELAY'..."
    exfatlabel /dev/nvme0n1p1 RELAY || true
fi

# 7. Configure Samba NAS Share
echo "📂 Configuring Samba NAS share..."
if ! grep -q "\[relay-storage\]" /etc/samba/smb.conf; then
    cat <<EOT >> /etc/samba/smb.conf

[relay-storage]
   comment = Relay High-Capacity Local NAS Storage
   path = /nvme
   browseable = yes
   read only = no
   guest ok = yes
   create mask = 0777
   directory mask = 0777
   force user = relay
   force group = relay
EOT
fi

systemctl restart smbd
systemctl enable smbd
systemctl restart avahi-daemon
systemctl enable avahi-daemon

# 8. Install Go (if not present)
if ! command -v go >/dev/null 2>&1; then
    echo "🐹 Installing Go 1.25.0 compiler..."
    cd /tmp
    wget https://go.dev/dl/go1.25.0.linux-arm64.tar.gz
    tar -C /usr/local -xzf go1.25.0.linux-arm64.tar.gz
    rm go1.25.0.linux-arm64.tar.gz
    export PATH=$PATH:/usr/local/go/bin
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
fi

# 9. Compile & Install Agent Service
echo "🔨 Compiling and installing Relay daemon..."
cd "$REPO_ROOT"
GO_BIN="/usr/local/go/bin/go"
if command -v go >/dev/null 2>&1; then
    GO_BIN="go"
fi
$GO_BIN build -o /usr/local/bin/ivault main.go

# Write systemd service file
cat <<EOT > /etc/systemd/system/ivault.service
[Unit]
Description=iVault Relay - Intelligent USB Storage Appliance
After=local-fs.target
DefaultDependencies=no

[Service]
Type=simple
ExecStart=/usr/local/bin/ivault --config /etc/ivault/config.json
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOT

# Write default unprovisioned config
if [ ! -f "/etc/ivault/config.json" ]; then
    cat <<EOT > /etc/ivault/config.json
{
  "db_path": "/var/lib/ivault/ivault.db",
  "image_path": "/dev/nvme0n1",
  "mount_point": "/nvme/ingest",
  "upload_queue": "/nvme/upload_queue",
  "udc_name": "fc000000.usb"
}
EOT
fi

chown -R relay:relay /var/lib/ivault /etc/ivault

systemctl daemon-reload
systemctl enable ivault.service
systemctl restart ivault.service

echo "✅ Relay Appliance successfully initialized! Device is running and ready for end-user provisioning."
