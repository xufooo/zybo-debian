#!/bin/sh
# ============================================================================
# customize01-base.sh — mmdebstrap customize hook
# ============================================================================
# IMPORTANT: mmdebstrap runs hooks on the HOST and passes the chroot directory
# in $1. All paths must therefore be prefixed with "$TARGET"; use
# chroot "$TARGET" for commands that must really run inside the chroot.
# ============================================================================
set -e

TARGET="${1:?usage: mmdebstrap hook requires the chroot directory as \$1}"

# --- /etc/fstab (must match U-Boot's root= : the second SD card partition) ----
mkdir -p "$TARGET/etc"
cat > "$TARGET/etc/fstab" <<'EOF'
# <file system>  <mount point>  <type>     <options>             <dump> <pass>
/dev/mmcblk0p2   /              ext4       defaults,noatime      0      1
proc             /proc          proc       defaults              0      0
sysfs            /sys           sysfs      defaults              0      0
devtmpfs         /dev           devtmpfs   mode=0755,nosuid      0      0
tmpfs            /tmp           tmpfs      defaults,nosuid,nodev 0      0
EOF

# --- Hostname ---------------------------------------------------------------
echo zybo-audio > "$TARGET/etc/hostname"
cat > "$TARGET/etc/hosts" <<'EOF'
127.0.0.1   localhost
127.0.1.1   zybo-audio
::1         localhost ip6-localhost ip6-loopback
EOF

# --- Network: systemd-networkd + DHCP (ZYBO on-board GEM0) ------------------
# The kernel uses CONFIG_MACB=y (the NIC is confirmed present on the board);
# this section just brings the interface up.
# NOTE: a minimal mmdebstrap rootfs ships neither ifupdown nor networkd
# configuration; without it the board boots with "NIC present but no IP" and
# MPD/AirPlay/SSH are all unusable.
install -d "$TARGET/etc/systemd/network"

# Pin the interface name to eth0.
# Debian enables systemd predictable naming by default (net.ifnames=1), and the
# Zynq GEM is a platform device with no PCI slot information, so udev derives
# end0 (en=ethernet, d=devicetree). On a single-NIC board eth0 is friendlier
# (docs, scripts and expectations all agree), so rename it back with a .link file.
# This lives in the rootfs instead of adding net.ifnames=0 to bootargs so the
# boot arguments stay shared with Buildroot and the name survives bootargs changes.
# NOTE: match on Type=ether only -- do NOT write OriginalName=en* !
#    The [Match] section of a .link file matches the KERNEL name, and on Zynq the
#    kernel name of the NIC is already eth0 (the second letter of "eth0" is "t",
#    so the glob `en*` does not match at all: the file can be present and correct
#    and the interface still ends up named end0).
#    This board has a single NIC, so Type=ether is enough; add MAC/Path filters if
#    a USB NIC is ever used.
cat > "$TARGET/etc/systemd/network/10-eth0.link" <<'EOF'
[Match]
Type=ether

[Link]
Name=eth0
EOF

cat > "$TARGET/etc/systemd/network/20-wired.network" <<'EOF'
[Match]
Name=eth0 en* eth*

[Network]
DHCP=yes
IPv6AcceptRA=yes
EOF

# Wireless NIC: DHCP on wl* (association is handled by wpa_supplicant, see below and the README)
cat > "$TARGET/etc/systemd/network/30-wireless.network" <<'EOF'
[Match]
Name=wl*

[Network]
DHCP=yes
IPv6AcceptRA=yes
EOF

# --- WiFi: make ANY USB WiFi dongle work out of the box (no hardcoded interface name) ---
# Rationale: the kernel already carries rtl8xxxu / rtw88(USB) / mt7601u / mt76 /
# rt2800usb / ath9k_htc and the firmware is installed; but systemd names the
# interface wlx<mac> per MAC address (different for every dongle), so configuring
# wpa_supplicant by interface name would support only one specific dongle.
#
# Approach (three parts):
#   1. A shared config file /etc/wpa_supplicant/wpa_supplicant.conf (the image
#      ships only a placeholder with no credentials; the user fills in SSID/PSK,
#      see the README)
#   2. A template drop-in so wpa_supplicant@<iface> reads the shared config
#      instead of Debian's default wpa_supplicant-<iface>.conf, which is tied to
#      the interface name
#   3. A udev rule that starts wpa_supplicant@%k for every wl* add/move event
#      (move must be included: the wlan0 -> wlx<mac> rename is a move event, and
#       with add only, the first instance would bind to an interface that no
#       longer exists and then exit)
install -d "$TARGET/etc/wpa_supplicant"
cat > "$TARGET/etc/wpa_supplicant/wpa_supplicant.conf" <<'EOF'
# Shared WiFi configuration: used by every wl* interface (udev starts
# wpa_supplicant@<iface> automatically).
# Add your own network (this appends a network={...} block):
#     wpa_passphrase "YOUR_SSID" "YOUR_PASSWORD" >> /etc/wpa_supplicant/wpa_supplicant.conf
#     systemctl restart 'wpa_supplicant@*'
# You can also edit this file directly. Keep permissions at 600 (it holds a plaintext PSK).
ctrl_interface=DIR=/run/wpa_supplicant GROUP=netdev
update_config=1
country=CN
EOF
chmod 600 "$TARGET/etc/wpa_supplicant/wpa_supplicant.conf"

install -d "$TARGET/etc/systemd/system/wpa_supplicant@.service.d"
cat > "$TARGET/etc/systemd/system/wpa_supplicant@.service.d/10-generic-conf.conf" <<'EOF'
[Service]
ExecStart=
ExecStart=/sbin/wpa_supplicant -c/etc/wpa_supplicant/wpa_supplicant.conf -i%I
EOF

cat > "$TARGET/etc/udev/rules.d/70-wifi-autoconf.rules" <<'EOF'
# Start a wpa_supplicant instance automatically for any USB WiFi NIC (interface wl*).
# See hooks/customize01-base.sh in zybo-debian for why the interface name is not hardcoded.
SUBSYSTEM=="net", ACTION=="add",  KERNEL=="wl*", TAG+="systemd", ENV{SYSTEMD_WANTS}+="wpa_supplicant@%k.service"
# The rename (wlan0 -> wlx<mac>) is a move event and must be handled as well.
SUBSYSTEM=="net", ACTION=="move", KERNEL=="wl*", TAG+="systemd", ENV{SYSTEMD_WANTS}+="wpa_supplicant@%k.service"
EOF

# --- Time zone (the board has no RTC and relies on NTP; a wrong zone makes every log timestamp wrong) ---
ln -sf /usr/share/zoneinfo/Asia/Shanghai "$TARGET/etc/localtime"
echo "Asia/Shanghai" > "$TARGET/etc/timezone"

# --- Locale: C.UTF-8 (no locale-gen needed, avoiding an extra build step) ---
echo 'LANG=C.UTF-8' > "$TARGET/etc/locale.conf"

# --- journald size cap ------------------------------------------------------
# The default SystemMaxUse is 10% of the filesystem, and our / is 14.5G, which
# would allow 1.5G -- wasteful in both space and SD card lifetime. The journal is
# only a few tens of MB in practice, so 128M is plenty to look back through.
install -d "$TARGET/etc/systemd/journald.conf.d"
cat > "$TARGET/etc/systemd/journald.conf.d/10-zybo.conf" <<'EOF'
[Journal]
Storage=persistent
SystemMaxUse=128M
RuntimeMaxUse=32M
EOF

# --- Login banner (same look as the Buildroot image) ------------------------
cat > "$TARGET/etc/issue" <<'EOF'
ZYBO Audio DSP (Debian armhf) \n \l
EOF
cat > "$TARGET/etc/motd" <<'EOF'
 ZYBO Rev B audio player - Debian armhf

  sound card  aplay -l             -> card 0: ZyboSoundCard (48k / S32_LE)
  volume      alsamixer -c 0       restored at boot from /var/lib/alsa/asound.state
  playback    mpc add/play/status  (music library in /var/lib/mpd/music)
  streaming   shairport-sync is running; pick zybo-audio from AirPlay
  debug       journalctl -u mpd -f
EOF

# DNS: no systemd-resolved; a static resolv.conf is enough and keeps variables minimal
cat > "$TARGET/etc/resolv.conf" <<'EOF'
nameserver 223.5.5.5
nameserver 1.1.1.1
EOF

# --- ALSA default device: software resample to 48kHz (MCLK fixed at 12.288MHz) ---
# The format must be S32_LE: in DMA mode axi-i2s only exposes S32_LE (verified on the board)
cat > "$TARGET/etc/asound.conf" <<'EOF'
pcm.!default {
    type plug
    slave {
        pcm "hw:0,0"
        rate 48000
        format S32_LE
        channels 2
    }
}

ctl.!default {
    type hw
    card 0
}
EOF

# --- Boot volume: install an asound.state -----------------------------------
# Without this file alsa-restore has nothing to do and the codec stays at the
# driver default (Master 95%, painfully loud). This file was captured on the
# board with `amixer sset Master 88% && alsactl store` (card id = ZyboSoundCard,
# matching /sys/class/sound/card0/id on the board).
# CI passes it in through the ZYBO_FILES environment variable, which points at
# the repository's files/ directory.
if [ -n "${ZYBO_FILES:-}" ] && [ -f "$ZYBO_FILES/asound.state" ]; then
    install -D -m 644 "$ZYBO_FILES/asound.state" "$TARGET/var/lib/alsa/asound.state"
    echo "[hook] installed asound.state (initial Master volume)"
else
    echo "[hook] WARN: ZYBO_FILES/asound.state not provided; boot volume will use the driver default"
fi

# --- root password (beta: the factory default is public, change it right after first boot) ---
# NOTE: this writes a PLAINTEXT default password that anyone with the image knows.
# The image is therefore only suitable for private use on a trusted LAN. To ship it:
#   set ZYBO_ROOT_PW=<random value> in CI, or run `passwd` on the board.
ROOT_PW="${ZYBO_ROOT_PW:-zybo}"
chroot "$TARGET" chpasswd <<EOF
root:${ROOT_PW}
EOF
if [ "${ZYBO_ROOT_PW:-}" = "" ]; then
    echo "[hook] WARN: root password is the default 'zybo' (override with ZYBO_ROOT_PW)"
fi

# --- SSH host keys: do NOT bake them into the image -------------------------
# openssh-server generates host keys at install time, which would mean every SD
# card built by the same CI job shares one host key (impersonation risk). Delete
# them so they are generated on first boot instead: ssh-keygen -A only fills in
# missing keys, is idempotent and never overwrites existing ones.
rm -f "$TARGET"/etc/ssh/ssh_host_*
install -d "$TARGET/etc/systemd/system/ssh.service.d"
cat > "$TARGET/etc/systemd/system/ssh.service.d/10-generate-host-keys.conf" <<'EOF'
# Regenerate host keys on first boot (or whenever they are missing) so that no
# two images share the same keys
[Service]
ExecStartPre=/usr/bin/ssh-keygen -A
EOF
echo "[hook] SSH host keys will be generated on first boot (not included in the image)"

# --- MPD: use the ALSA default device explicitly (= the plug above, 48k/S32_LE) ---
# Not the distro default config: that may open hw:0,0 directly and expect a
# hardware mixer, while our format is S32_LE.
cat > "$TARGET/etc/mpd.conf" <<'EOF'
music_directory     "/var/lib/mpd/music"
playlist_directory  "/var/lib/mpd/playlists"
db_file             "/var/lib/mpd/tag_cache"
log_file            "syslog"
pid_file            "/run/mpd/pid"
state_file          "/var/lib/mpd/state"
sticker_file        "/var/lib/mpd/sticker.sql"

user                "mpd"
bind_to_address     "any"
port                "6600"

# Pure software mixing: does not rely on the codec's hardware mixer controls
mixer_type          "software"
volume_normalization "no"

audio_output {
    type        "alsa"
    name        "ZYBO (SSM2603 @48k)"
    device      "default"
    mixer_type  "software"
}
EOF
chroot "$TARGET" install -d -o mpd -g audio /var/lib/mpd/music /var/lib/mpd/playlists
chroot "$TARGET" usermod -aG audio mpd || true

# --- Enable services --------------------------------------------------------
# The serial getty is generated automatically by systemd-getty-generator from
# console=; nothing needs to be created by hand
WANT="$TARGET/etc/systemd/system/multi-user.target.wants"
mkdir -p "$WANT"
for u in ssh.service \
         systemd-networkd.service \
         systemd-timesyncd.service \
         avahi-daemon.service \
         shairport-sync.service \
         bluetooth.service \
         mpd.socket; do
    if [ -e "$TARGET/usr/lib/systemd/system/$u" ]; then
        ln -sf "/usr/lib/systemd/system/$u" "$WANT/$u"
        echo "[hook] enabled $u"
    else
        echo "[hook] WARN: unit not found, skipped: $u"
    fi
done
chroot "$TARGET" systemctl set-default multi-user.target 2>/dev/null || true

echo "[hook] base configuration applied to $TARGET"
