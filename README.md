# zybo-debian

Debian rootfs for a ZYBO (Zynq-7000) audio player.

Builds a Debian armhf root filesystem on GitHub Actions with `mmdebstrap`.
The kernel, device tree, U-Boot and PL bitstream are **not** built here — they
come from the sibling repositories and are combined when writing the SD card.

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
├── hooks/customize01-base.sh   # chroot hook: fstab, hostname, asound.conf, services
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
