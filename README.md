# zybo-debian

Debian rootfs for a ZYBO (Zynq-7000) audio player.

Builds a Debian armhf root filesystem on GitHub Actions with `mmdebstrap`.
The kernel, device tree, U-Boot and PL bitstream are **not** built here — they
come from the sibling repositories and are combined when writing the SD card.

## USB WiFi（任意 dongle，插上即用）

镜像里带 `wpasupplicant / iw / rfkill / wireless-regdb` 与常见固件
（`firmware-realtek / mediatek / atheros / misc-nonfree`），内核侧驱动见
`zybo-linux/kernel/config.fragment`（`rtl8xxxu`、`rtw88` USB、`mt7601u`、`mt76`、
`rt2800usb`、`ath9k_htc`）。

**接口名不写死**：systemd 会按 MAC 命名成 `wlx<mac>`，每块 dongle 都不同，所以：

- `/etc/systemd/network/30-wireless.network` 匹配 `Name=wl*` → DHCP
- `/etc/udev/rules.d/70-wifi-autoconf.rules` → 任何 `wl*` 的 add/move 事件自动起
  `wpa_supplicant@<iface>`
- `/etc/systemd/system/wpa_supplicant@.service.d/10-generic-conf.conf` → 让实例读
  **通用**配置 `/etc/wpa_supplicant/wpa_supplicant.conf`（Debian 默认是按接口名找
  `wpa_supplicant-<iface>.conf`，那等于只支持一块 dongle）

出厂**不带任何 WiFi 凭据**，上板后填自己的网络即可：

```bash
wpa_passphrase "你的SSID" "你的密码" >> /etc/wpa_supplicant/wpa_supplicant.conf
systemctl restart 'wpa_supplicant@*'
ip -br addr show wl*        # 拿到 IP 就行
```

> 已在板上实测两条路径：
> - **带电热插拔**：把按接口名静态启用的服务 `disable` 掉、只留 udev 规则，重新枚举 USB
>   网卡后 **5 秒内**自动重新关联并拿到 IP；
> - **冷启动换 dongle**：换上一块板子从没见过的 MT7601U（`148f:7601`）后重启，开机自动
>   加载固件、改名 `wlx<mac>`、关联并拿到 IP；`wpa_supplicant@<iface>` 的状态是
>   `is-enabled=disabled` 但 `is-active=active` ⇒ 确实由 udev 按需拉起。
>
> 固件归属（常见 dongle 全覆盖）：`firmware-realtek`（rtl8xxxu / rtw88）、
> `firmware-mediatek`（mt7601u、mt76）、`firmware-atheros`（ath9k_htc）、
> `firmware-misc-nonfree`（rt2800usb 等）。细节见 `audio_player` 的
> `docs/TROUBLESHOOTING.md` §19。

## What it builds

| Item | Value |
|---|---|
| Suite | `trixie` (default) or `bookworm` (LTS fallback) |
| Architecture | `armhf` (Cortex-A9, ARMv7 + VFPv3-D16) |
| Tool | `mmdebstrap` + `qemu-user-static` (cross-arch second stage, no KVM) |
| Output | `rootfs.tar.zst` (extract into a mounted ext4 partition) and `rootfs.ext4` (for `dd` / image assembly) |
| Packages | `alsa-utils`, `sox`, `mpd`, `mpc`, `shairport-sync`, `avahi-daemon`, `bluez-alsa-utils`, `libasound2-plugin-bluez`, `systemd-timesyncd`, `i2c-tools`, `iproute2`, `iputils-ping`, `openssh-server`, `curl`, … |

The `hooks/customize01-base.sh` hook runs on the **host** (mmdebstrap passes the
chroot directory as `$1`), and writes:

- `/etc/systemd/network/10-eth0.link` + `20-wired.network` — Debian enables
  systemd's predictable interface names, and the Zynq GEM is a platform device
  with no PCI slot, so udev derives `end0` (`en` + `d`evicetree). A `.link` file
  renames it back to **`eth0`** (rootfs-level, so `net.ifnames` bootargs stay
  shared with the Buildroot image).
- timezone `Asia/Shanghai`, `LANG=C.UTF-8`, `/etc/motd` banner
- `/var/lib/alsa/asound.state` — captured on the board with
  `amixer -c 0 sset Master 88% && alsactl store`. Without it `alsa-restore` has
  nothing to apply and the codec stays at the driver default (Master 95%, which
  is painfully loud). Card id in the file is `ZyboSoundCard`, matching the board.
- `/etc/fstab` — root on `/dev/mmcblk0p2` (must match the U-Boot `root=`)
- `/etc/hostname`, `/etc/hosts` (`zybo-audio`)
- `/etc/systemd/network/20-wired.network` — DHCP on the on-board GEM0 (`eth0`),
  plus **enabled `systemd-networkd`**. A minimal mmdebstrap rootfs ships with no
  network configuration at all (neither ifupdown nor networkd), so without this
  the board boots with the link down and no IP: MPD, AirPlay and SSH are all
  unreachable.
- `/etc/resolv.conf` — static public DNS (no `systemd-resolved` dependency)
- enabled `systemd-timesyncd` — ZYBO has no RTC, so the clock needs NTP before
  anything TLS-based (AirPlay) will work
- enabled `avahi-daemon` — required for AirPlay/Bonjour discovery
- `/etc/asound.conf` — default PCM: `plug` to 48 kHz / **S32_LE** (the axi-i2s
  driver only exposes S32_LE in DMA mode; S16_LE/S24_LE are rejected)
- `/etc/mpd.conf` — explicit ALSA output on the `default` device with software
  mixing, instead of the distro default (which may open `hw:0,0` directly and
  expect a hardware mixer)
- enabled `ssh`, `systemd-networkd`, `systemd-timesyncd`, `avahi-daemon`,
  `shairport-sync` and `mpd.socket`

Hook scripts in `hooks/` must be executable and named with one of the
mmdebstrap stage prefixes (`setup`, `extract`, `essential`, `customize`).

Serial console login needs no extra configuration: systemd's getty generator
instantiates `serial-getty@ttyPS0` from the kernel `console=ttyPS0,115200`.

## CI

`.github/workflows/build-debian-rootfs.yml` is manual (`workflow_dispatch`) and
takes optional inputs for the suite and extra packages. Artifact: `debian-rootfs`.

## Layout

```
zybo-debian/
├── .github/workflows/build-debian-rootfs.yml
├── files/asound.state          # 板上抓取的 ALSA 开机默认值（音量）
├── hooks/customize01-base.sh   # host-side hook: fstab/网络/时区/asound.conf/mpd/服务
├── scripts/make_sdcard.sh      # assemble a flashable sdcard.img
└── README.md
```

## Assembling an SD card

Collect the three artifact sets:

```
zybo-linux    → uImage, zybo-audio.dtb
zybo-buildroot→ boot.bin (BOOT.BIN), u-boot.img, uEnv.txt, system.bit
zybo-debian   → rootfs.ext4 (or rootfs.tar.zst)
```

> `BOOT.BIN` here is U-Boot's `spl/boot.bin` from Buildroot — no FSBL/Vitis needed.
> The bitstream is loaded by `boot.scr` (`fpga loadb system.bit`) in the interim
> boot flow; Phase 3 folds it into a FIT image.

**Method 1 — copy files into existing partitions (no `dd`)**

```bash
# FAT32 boot partition mounted at /mnt/boot, ext4 root partition at /mnt/root
cp boot/*            /mnt/boot/          # BOOT.BIN, u-boot.img, uImage, dtb, uEnv.txt, system.bit
sudo tar -xpf rootfs.tar.zst -C /mnt/root --numeric-owner
sync && umount /mnt/boot /mnt/root
```

**Method 2 — build one image and `dd` it**

```bash
./scripts/make_sdcard.sh \
    --boot ./boot \        # BOOT.BIN, u-boot.img, uImage, zybo-audio.dtb, uEnv.txt, system.bit
    --rootfs ./rootfs.ext4 \
    --out sdcard.img
sudo dd if=sdcard.img of=/dev/sdX bs=4M status=progress conv=fsync

# The image is sized to its contents (~1 GB) so it stays easy to move around.
# After flashing, grow the root partition to the real card size:
sudo ./scripts/grow_sdcard.sh /dev/sdX          # resizepart + e2fsck + resize2fs
```

The layout is deliberately just two partitions — `BOOT` (FAT32) and `/` (ext4).
Music lives on the root filesystem (`/var/lib/mpd/music`).

Boot parameters used by `uEnv.txt`:

```
root=/dev/mmcblk0p2 rootwait rw console=ttyPS0,115200
```

## Related

- Kernel: [zybo-linux](https://github.com/xufooo/zybo-linux)
- Buildroot rootfs (Phase 1/2): [zybo-buildroot](https://github.com/xufooo/zybo-buildroot)
