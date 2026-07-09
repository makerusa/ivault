#!/usr/bin/env bash
# ==============================================================================
# Relay appliance hardening / slimming
# ------------------------------------------------------------------------------
# Reduces attack surface and background load on a vendor board image. Meant to
# be run deliberately AFTER install.sh, and reviewed — not auto-run.
#
# SAFE BY DEFAULT: masks clearly-unused services, caps logs, applies sysctl
# hardening. All reversible (systemctl unmask / delete the drop-in files).
# RISKIER steps are opt-in behind flags:
#   --ssh-lockdown   disable SSH password auth (refuses unless a key is present)
#   --firewall       install an nftables inbound allowlist
#   --samba=MODE     keep (default) | lockdown (no guest, read-only) | remove
#
# It NEVER touches: networking (networkd/resolved/wpa_supplicant/netplan),
# the SSH daemon itself, the dwc3/USB gadget stack, storage mounts, fail2ban,
# zram/ramlog, fstrim, or the serial console (your headless recovery path).
#
# Preview everything first:   sudo ./scripts/harden.sh --dry-run
# ==============================================================================
set -euo pipefail

DRY_RUN=0
SSH_LOCKDOWN=0
FIREWALL=0
SAMBA="keep"

c_reset=$'\033[0m'; c_grn=$'\033[32m'; c_ylw=$'\033[33m'; c_red=$'\033[31m'; c_cyn=$'\033[36m'
info() { echo "${c_cyn}▸${c_reset} $*"; }
ok()   { echo "${c_grn}✓${c_reset} $*"; }
warn() { echo "${c_ylw}⚠${c_reset}  $*" >&2; }
die()  { echo "${c_red}✗ $*${c_reset}" >&2; exit 1; }

for arg in "$@"; do
    case "$arg" in
        --dry-run)       DRY_RUN=1 ;;
        --ssh-lockdown)  SSH_LOCKDOWN=1 ;;
        --firewall)      FIREWALL=1 ;;
        --samba=*)       SAMBA="${arg#*=}" ;;
        -h|--help)       grep '^# ' "$0" | sed 's/^# \{0,1\}//' | head -25; exit 0 ;;
        *) die "unknown argument: $arg (try --help)" ;;
    esac
done
[ "$EUID" -eq 0 ] || die "run as root (sudo $0)"
case "$SAMBA" in keep|lockdown|remove) ;; *) die "--samba must be keep|lockdown|remove" ;; esac

run() { if [ "$DRY_RUN" = 1 ]; then echo "    would run: $*"; else "$@"; fi; }
writefile() { # writefile <path>  (content on stdin)
    local path="$1"; local content; content="$(cat)"
    if [ "$DRY_RUN" = 1 ]; then echo "    would write $path"; else
        mkdir -p "$(dirname "$path")"; printf '%s\n' "$content" > "$path"
    fi
}

echo; echo "Relay hardening ${DRY_RUN:+}"; [ "$DRY_RUN" = 1 ] && warn "DRY RUN — no changes will be made"

# ---- 1. Mask clearly-unused services -----------------------------------------
info "Masking unused services (reverse with: systemctl unmask <name>)"
# Bluetooth (no BT use), Rockchip camera ISP 3A daemon (no camera), the vendor
# LED manager (Relay drives the LED itself), doc indexing, and unattended apt
# upgrades (avoid surprise kernel/USB breakage on a pinned vendor image).
UNITS=(bluetooth.service aic-bluetooth.service rkaiq_3A.service armbian-led-state.service
       man-db.timer apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service)
for u in "${UNITS[@]}"; do
    if systemctl list-unit-files --no-legend "$u" 2>/dev/null | grep -q .; then
        run systemctl disable --now "$u" 2>/dev/null || true
        run systemctl mask "$u" 2>/dev/null || true
        ok "masked $u"
    fi
done

# ---- 2. Cap journald so logs can't fill storage ------------------------------
info "Capping systemd journal size"
writefile /etc/systemd/journald.conf.d/99-relay.conf <<'EOF'
[Journal]
SystemMaxUse=100M
RuntimeMaxUse=50M
EOF
run systemctl restart systemd-journald || true

# ---- 3. sysctl network hardening ---------------------------------------------
info "Applying sysctl network hardening"
writefile /etc/sysctl.d/99-relay-hardening.conf <<'EOF'
# Relay appliance hardening — safe for a non-router LAN device
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv6.conf.all.accept_source_route = 0
net.ipv4.icmp_echo_ignore_broadcasts = 1
net.ipv4.conf.all.log_martians = 1
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
EOF
run sysctl --quiet --system || true

# ---- 4. SSH lockdown (opt-in, guarded) ---------------------------------------
if [ "$SSH_LOCKDOWN" = 1 ]; then
    info "SSH lockdown requested — verifying an authorized key exists first"
    if ! ls /home/*/.ssh/authorized_keys /root/.ssh/authorized_keys >/dev/null 2>&1; then
        warn "No authorized_keys found — REFUSING to disable password auth (would lock you out)."
    else
        writefile /etc/ssh/sshd_config.d/99-relay.conf <<'EOF'
PasswordAuthentication no
PermitRootLogin prohibit-password
KbdInteractiveAuthentication no
EOF
        run systemctl reload ssh || run systemctl reload sshd || true
        ok "SSH set to key-only (password auth disabled)"
    fi
fi

# ---- 5. Samba ----------------------------------------------------------------
case "$SAMBA" in
    keep)
        if systemctl is-enabled --quiet smbd 2>/dev/null && grep -q 'guest ok = yes' /etc/samba/smb.conf 2>/dev/null; then
            warn "Samba share is guest-writable (unauthenticated LAN access). Re-run with"
            warn "--samba=lockdown (auth + read-only) or --samba=remove to close this."
        fi
        ;;
    lockdown)
        info "Locking down Samba share (no guest, read-only)"
        if [ "$DRY_RUN" = 0 ]; then
            sed -i '/^\[relay\]/,/^$/{s/guest ok = yes/guest ok = no/; s/read only = no/read only = yes/}' /etc/samba/smb.conf 2>/dev/null || true
        fi
        run systemctl restart smbd || true
        warn "Set a Samba user to access it: sudo smbpasswd -a <user>"
        ;;
    remove)
        info "Removing Samba NAS share entirely"
        run systemctl disable --now smbd 2>/dev/null || true
        run systemctl mask smbd 2>/dev/null || true
        ;;
esac

# ---- 6. Firewall (opt-in) ----------------------------------------------------
if [ "$FIREWALL" = 1 ]; then
    info "Installing nftables inbound allowlist"
    warn "You have a serial console (ttyFIQ0) for recovery if this locks out SSH."
    run apt-get install -y nftables >/dev/null 2>&1 || true
    SAMBA_RULE=""
    [ "$SAMBA" != "remove" ] && SAMBA_RULE='tcp dport { 139, 445 } accept
        udp dport { 137, 138 } accept'
    writefile /etc/nftables.conf <<EOF
#!/usr/sbin/nft -f
flush ruleset
table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;
        ct state established,related accept
        iif lo accept
        ip protocol icmp accept
        ip6 nexthdr ipv6-icmp accept
        udp sport 67 udp dport 68 accept      # DHCP client replies
        udp dport 5353 accept                 # mDNS (discovery / Avahi)
        tcp dport 22 accept                   # SSH
        ${SAMBA_RULE}
    }
    chain forward { type filter hook forward priority 0; policy drop; }
    chain output  { type filter hook output  priority 0; policy accept; }
}
EOF
    run systemctl enable --now nftables || true
    ok "firewall applied (inbound: SSH, mDNS, DHCP replies$([ "$SAMBA" != remove ] && echo ', Samba'))"
fi

# ---- Notes -------------------------------------------------------------------
echo
warn "Not touched (by design): networking, ssh daemon, USB gadget stack, storage,"
warn "fail2ban, zram/ramlog, fstrim, serial console."
warn "dnsmasq is running on this image — likely vestigial from the vendor USB"
warn "gadget network. Verify it's unused (ss -lunp | grep :53) before disabling."
echo
ok "Hardening complete.${DRY_RUN:+ (dry run — nothing changed)}"
