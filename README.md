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
| Output | `rootfs.ext4` |
| Packages | `alsa-utils`, `mpd`, `shairport-sync`, `bluez-alsa-utils`, `libasound2-plugin-bluez`, `i2c-tools`, `openssh-server`, … |

The `hooks/01-base.sh` hook runs inside the chroot and writes:

- `/etc/fstab` — root on `/dev/mmcblk0p2` (must match the U-Boot `root=`)
- `/etc/hostname`, `/etc/hosts`
- `/etc/asound.conf` — default PCM resampled to 48 kHz (`plug`), matching a
  fixed 12.288 MHz MCLK
- enables `ssh`, `mpd`, `shairport-sync`

Serial console login needs no extra configuration: systemd's getty generator
instantiates `serial-getty@ttyPS0` from the kernel `console=ttyPS0,115200`.

## CI

`.github/workflows/build-debian-rootfs.yml` is manual (`workflow_dispatch`) and
takes optional inputs for the suite and extra packages. Artifact: `debian-rootfs`.

## Layout

```
zybo-debian/
├── .github/workflows/build-debian-rootfs.yml
├── hooks/01-base.sh            # chroot hook: fstab, hostname, asound.conf, services
├── scripts/make_sdcard.sh      # assemble a flashable sdcard.img
└── README.md
```

## Assembling an SD card

Collect the three artifact sets:

```
zybo-linux    → uImage, zybo-audio.dtb
zybo-buildroot→ boot.bin (BOOT.BIN), u-boot.img, uEnv.txt, system.bit
zybo-debian   → rootfs.ext4
```

Then:

```bash
./scripts/make_sdcard.sh \
    --boot ./boot \        # BOOT.BIN, u-boot.img, uImage, zybo-audio.dtb, uEnv.txt, system.bit
    --rootfs ./rootfs.ext4 \
    --out sdcard.img
sudo dd if=sdcard.img of=/dev/sdX bs=4M status=progress conv=fsync
```

Boot parameters used by `uEnv.txt`:

```
root=/dev/mmcblk0p2 rootwait rw console=ttyPS0,115200
```

## Related

- Kernel: [zybo-linux](https://github.com/xufooo/zybo-linux)
- Buildroot rootfs (Phase 1/2): [zybo-buildroot](https://github.com/xufooo/zybo-buildroot)
