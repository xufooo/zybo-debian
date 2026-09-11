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
EOF

# ── 主机名 ────────────────────────────────────────────────────────────────
echo zybo-audio > "$TARGET/etc/hostname"
cat > "$TARGET/etc/hosts" <<'EOF'
127.0.0.1   localhost
127.0.1.1   zybo-audio
::1         localhost ip6-localhost ip6-loopback
EOF

# ── 网络：systemd-networkd + DHCP（ZYBO 板载 GEM0）────────────────────────
# 内核用 CONFIG_MACB=y（板上已确认网卡存在）；这里把接口拉起来。
# 注意：mmdebstrap 的最小 rootfs 默认既没有 ifupdown 配置也没有 networkd 配置，
# 不补的话板子起来是"有网卡但没 IP"，MPD/AirPlay/SSH 全都用不了。
install -d "$TARGET/etc/systemd/network"

# 网卡名固定为 eth0。
# Debian 默认启用 systemd 可预测命名（net.ifnames=1），而 Zynq 的 GEM 是 platform
# 设备、没有 PCI 槽位信息，udev 会派生出 end0（en=ethernet, d=devicetree）。
# 单网口板子上 eth0 更好用（文档/脚本/直觉一致），所以用 .link 改回来。
# 放在 rootfs 里而不是往 bootargs 加 net.ifnames=0：这样启动参数保持与
# Buildroot 共用、且换 bootargs 也不会把网卡名弄丢。
# ⚠️ 匹配条件只写 Type=ether：**不要写 OriginalName=en***！
#    .link 的 [Match] 匹配的是**内核原始名**，而 Zynq 上网卡的内核名就是 eth0
#    （"eth0" 第二个字母是 t，glob `en*` 根本不匹配 —— 实测踩过：
#    文件在、格式对，网卡名依旧 end0，只在改名后才叫 en… 已经是 udev 的产物了）。
#    本板单网口，用 Type=ether 即可；若将来插 USB 网卡需再加 MAC/Path 过滤。
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

# 无线网卡：wl* DHCP（关联由 wpa_supplicant 负责，见下面与 README）
cat > "$TARGET/etc/systemd/network/30-wireless.network" <<'EOF'
[Match]
Name=wl*

[Network]
DHCP=yes
IPv6AcceptRA=yes
EOF

# ── 无线：让"**任意** USB WiFi dongle"插上就能用（不写死接口名）────────────
# 动机：内核侧已经带了 rtl8xxxu / rtw88(USB) / mt7601u / mt76 / rt2800usb / ath9k_htc，
# 固件也装了；但 systemd 会按 MAC 把接口命名成 wlx<mac>（每块 dongle 都不一样），
# 如果再按接口名去配 wpa_supplicant，就等于"只支持你手上那一块"。
#
# 做法（三件套）：
#   1. 通用配置文件 /etc/wpa_supplicant/wpa_supplicant.conf（镜像里只放**占位**，
#      出厂不带任何凭据；用户自己填 SSID/PSK，见 README）
#   2. 模板 drop-in：让 wpa_supplicant@<iface> 读**通用**配置，而不是
#      Debian 默认的 wpa_supplicant-<iface>.conf（那个和接口名绑死）
#   3. udev 规则：任何 wl* 网卡 add/move 事件都自动起 wpa_supplicant@%k
#      （move 必须写：wlan0 → wlx<mac> 的改名是一次 move 事件，只写 add 的话
#        第一次起的实例会绑在已经不存在的接口上然后退出）
install -d "$TARGET/etc/wpa_supplicant"
cat > "$TARGET/etc/wpa_supplicant/wpa_supplicant.conf" <<'EOF'
# 通用 WiFi 配置：所有 wl* 接口共用（由 udev 自动起 wpa_supplicant@<iface>）。
# 填自己的网络（执行完会自动追加 network={...}）：
#     wpa_passphrase "你的SSID" "你的密码" >> /etc/wpa_supplicant/wpa_supplicant.conf
#     systemctl restart 'wpa_supplicant@*'
# 也可以直接编辑本文件。注意权限保持 600（里面有明文 PSK）。
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
# 任意 USB WiFi 网卡（接口名 wl*）自动起 wpa_supplicant 实例。
# 不按接口名写死的理由见 zybo-debian 的 hooks/customize01-base.sh。
SUBSYSTEM=="net", ACTION=="add",  KERNEL=="wl*", TAG+="systemd", ENV{SYSTEMD_WANTS}+="wpa_supplicant@%k.service"
# 改名（wlan0 → wlx<mac>）是 move 事件，必须同样处理。
SUBSYSTEM=="net", ACTION=="move", KERNEL=="wl*", TAG+="systemd", ENV{SYSTEMD_WANTS}+="wpa_supplicant@%k.service"
EOF

# ── 时区（板子无 RTC，靠 NTP 对时；时区不对日志时间全是错的）──────────────
ln -sf /usr/share/zoneinfo/Asia/Shanghai "$TARGET/etc/localtime"
echo "Asia/Shanghai" > "$TARGET/etc/timezone"

# ── 语言：用 C.UTF-8（不需要 locale-gen，避免额外生成步骤）────────────────
echo 'LANG=C.UTF-8' > "$TARGET/etc/locale.conf"

# ── journald 容量上限 ─────────────────────────────────────────────────────
# 默认 SystemMaxUse = 文件系统的 10%，而我们的 / 是 14.5G → 允许写到 1.5G，
# 对 SD 卡既费空间又费寿命。盘上实测日志本身只有十几 MB，128M 足够回溯。
install -d "$TARGET/etc/systemd/journald.conf.d"
cat > "$TARGET/etc/systemd/journald.conf.d/10-zybo.conf" <<'EOF'
[Journal]
Storage=persistent
SystemMaxUse=128M
RuntimeMaxUse=32M
EOF

# ── 登录横幅（与 Buildroot 版一致的观感）──────────────────────────────────
cat > "$TARGET/etc/issue" <<'EOF'
ZYBO Audio DSP (Debian armhf) \n \l
EOF
cat > "$TARGET/etc/motd" <<'EOF'
 ZYBO Rev B 音频播放器 — Debian armhf

  声卡   aplay -l            → card 0: ZyboSoundCard（48k / S32_LE）
  音量   alsamixer -c 0      开机默认由 /var/lib/alsa/asound.state 恢复
  播放   mpc add/play/status （曲库 /var/lib/mpd/music）
  推送   shairport-sync 已在跑，手机 AirPlay 里选 zybo-audio
  排查   journalctl -u mpd -f
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

# ── 开机音量：装一份 asound.state ─────────────────────────────────────────
# 没有这个文件时 alsa-restore 无事可做，codec 就停在驱动默认（Master 95%，
# 实测"耳朵要聋"）。这份文件是在板子上 `amixer sset Master 88% && alsactl store`
# 抓回来的（card id = ZyboSoundCard，与板上 /sys/class/sound/card0/id 一致）。
# 由 CI 通过环境变量 ZYBO_FILES 指向仓库 files/ 目录传入。
if [ -n "${ZYBO_FILES:-}" ] && [ -f "$ZYBO_FILES/asound.state" ]; then
    install -D -m 644 "$ZYBO_FILES/asound.state" "$TARGET/var/lib/alsa/asound.state"
    echo "[hook] installed asound.state (initial Master volume)"
else
    echo "[hook] WARN: ZYBO_FILES/asound.state 未提供，开机音量将用驱动默认值"
fi

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
