#!/bin/sh
# ============================================================================
# customize01-base.sh — mmdebstrap customize hook
# ============================================================================
# 重要：mmdebstrap 的 hook 在**宿主**上执行，chroot 目录通过 $1 传入。
# 因此所有路径都必须加 "$TARGET" 前缀；需要真正进入 chroot 的命令用 chroot "$TARGET"。
# ============================================================================
set -e

TARGET="${1:?usage: mmdebstrap hook requires the chroot directory as \$1}"

# ── /etc/fstab（必须与 U-Boot 的 root= 一致：SD 卡第二分区）────────────────
mkdir -p "$TARGET/etc"
cat > "$TARGET/etc/fstab" <<'EOF'
# <file system>  <mount point>  <type>     <options>             <dump> <pass>
/dev/mmcblk0p2   /              ext4       defaults,noatime      0      1
proc             /proc          proc       defaults              0      0
sysfs            /sys           sysfs      defaults              0      0
devtmpfs         /dev           devtmpfs   mode=0755,nosuid      0      0
tmpfs            /tmp           tmpfs      defaults,nosuid,nodev 0      0
# 若在卡尾另建音乐分区（mkfs.ext4 -L music），取消下一行注释：
#LABEL=music     /var/lib/mpd/music  ext4  defaults,noatime     0      2
EOF

# ── 主机名 ────────────────────────────────────────────────────────────────
echo zybo-audio > "$TARGET/etc/hostname"
cat > "$TARGET/etc/hosts" <<'EOF'
127.0.0.1   localhost
127.0.1.1   zybo-audio
::1         localhost ip6-localhost ip6-loopback
EOF

# ── 网络：systemd-networkd + DHCP（ZYBO 板载 GEM0 → eth0）─────────────────
# 内核用 CONFIG_MACB=y（已在板上确认 eth0 存在）；这里把接口拉起来。
# 注意：mmdebstrap 的最小 rootfs 默认既没有 ifupdown 配置也没有 networkd 配置，
# 不补的话板子起来是"有网卡但没 IP"，MPD/AirPlay/SSH 全都用不了。
install -d "$TARGET/etc/systemd/network"
cat > "$TARGET/etc/systemd/network/20-wired.network" <<'EOF'
[Match]
Name=en* eth*

[Network]
DHCP=yes
IPv6AcceptRA=yes
EOF

# DNS：不引入 systemd-resolved，直接给静态 resolv.conf（够用且最少变量）
cat > "$TARGET/etc/resolv.conf" <<'EOF'
nameserver 223.5.5.5
nameserver 1.1.1.1
EOF

# ── ALSA 默认设备：软件重采样到 48kHz（MCLK 固定 12.288MHz）────────────────
# 格式必须 S32_LE：axi-i2s 在 DMA 模式下只暴露 S32_LE（上板验证）
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

# ── root 密码（开发用；首次登录后请修改）─────────────────────────────────
chroot "$TARGET" chpasswd <<'EOF'
root:zybo
EOF

# ── MPD：显式走 ALSA default 设备（= 上面的 plug，48k/S32_LE）──────────────
# 不用发行版默认配置：默认可能直接开 hw:0,0 并要硬件混音器，而我们的格式是 S32_LE。
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

# 纯软件混音：不依赖 codec 的硬件混音器控制
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

# ── 启用服务 ──────────────────────────────────────────────────────────────
# 串口 getty 由 systemd-getty-generator 依 console= 自动生成，无需手工建
WANT="$TARGET/etc/systemd/system/multi-user.target.wants"
mkdir -p "$WANT"
for u in ssh.service \
         systemd-networkd.service \
         systemd-timesyncd.service \
         avahi-daemon.service \
         shairport-sync.service \
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
