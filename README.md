# zybo-debian

Debian armhf rootfs for a ZYBO (Zynq-7000) audio player.

Builds a Debian armhf root filesystem with `mmdebstrap`, on GitHub Actions or on
a local host. The kernel, device tree, U-Boot and PL bitstream are **not** built
here — they come from the sibling repositories and are combined on the SD card.

## What it builds

| Item | Value |
|---|---|
| Suite | `trixie` (default) or `bookworm` |
| Architecture | `armhf` (Cortex-A9, ARMv7 + VFPv3-D16) |
| Tool | `mmdebstrap` + `qemu-user-static` (no KVM); components `main`, `contrib`, `non-free`, `non-free-firmware` |
| Output | `rootfs.tar.zst` (extract into an ext4 partition), `rootfs.ext4` (write with `dd`) |

## Layout

- `.github/workflows/build-debian-rootfs.yml` — CI: builds the ARM backend from
  `app/`, then the rootfs tar and the ext4 image.
- `app/` — application sources: the Go backend (`backend/*.go`, `go.mod`),
  the WebUI (`webui/`) and the systemd unit. CI compiles the backend with
  `go build -trimpath` into `files/zybo-audio-web`; the binary is not tracked.
- `files/` — app payload installed by `customize02`: the staged ARM backend
  (built from `app/`), systemd unit (port 8080), `webui/`, `asound.state`,
  `VERSION`, `THIRD-PARTY.md`, `licenses/` and a CI-generated `SHA256SUMS`.
- `hooks/` — host-side mmdebstrap hooks: `customize01-base.sh` (base system),
  `customize02-app.sh` (payload), `customize03-shairport.sh` (AirPlay config);
  each must be executable and start with a stage prefix (`setup`, `extract`,
  `essential`, `customize`).
- `scripts/` — `make_sdcard.sh`, `grow_sdcard.sh`, `test_app_hook.sh`,
  `test_shairport_hook.sh`.

## Requirements

`mmdebstrap`, `qemu-user-static` (registered in `binfmt_misc` for armhf),
`binfmt-support`, `arch-test`, `debian-archive-keyring`, `e2fsprogs`, `fakeroot`
and `zstd`. The SD-card helpers additionally need `parted`, `dosfstools`
(`mkfs.vfat`) and `mtools` (`mcopy`).

## Build

```bash
PKGS=$(grep -oP 'BASE_PACKAGES: "\K[^"]+' .github/workflows/build-debian-rootfs.yml)
sudo ZYBO_FILES="$PWD/files" mmdebstrap \
    --components=main,contrib,non-free,non-free-firmware \
    --architectures=armhf --variant=minbase \
    --aptopt='Apt::Install-Recommends "true"' \
    --include="$PKGS" --hook-dir="$PWD/hooks" --format=tar trixie rootfs.tar
zstd -19 -T0 rootfs.tar -o rootfs.tar.zst
```

`ZYBO_FILES` points the hooks at `files/` (app payload + ALSA state). For
`rootfs.ext4`, extract the tar and run `mke2fs -d` in one single `fakeroot`
session, as the workflow does, so non-root ownership survives into the image.

The backend binary is **not stored in this repository**. Build it before running
`mmdebstrap`, exactly as CI does:

```bash
cd app
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -trimpath -ldflags="-s -w" -o ../files/zybo-audio-web ./backend
```

## Image configuration

`hooks/customize01-base.sh` sets up networking (on-board GEM as `eth0` via
systemd-networkd; every `wl*` interface gets DHCP and `wpa_supplicant`), hostname
`zybo-audio`, timezone `Asia/Shanghai`, `LANG=C.UTF-8`, `/etc/fstab` (root on
`/dev/mmcblk0p2`) and the audio defaults: `/etc/asound.conf` (`plug` to 48 kHz /
`S32_LE`, the format the AXI-I2S driver exposes in DMA mode), `/etc/mpd.conf`
(ALSA `default` device, software mixing, library in `/var/lib/mpd/music`) and
`/var/lib/alsa/asound.state`. It enables `ssh`, `systemd-networkd`,
`systemd-timesyncd`, `avahi-daemon`, `shairport-sync`, `bluetooth` and
`mpd.socket`. The root password comes from `ZYBO_ROOT_PW` (`zybo` when unset) —
change it after the first boot; SSH host keys are generated on first boot.
`customize03-shairport.sh` writes `/etc/shairport-sync.conf`
(`interpolation = "basic"`, 1.0 s buffer, diagnostics off).

## CI

`.github/workflows/build-debian-rootfs.yml` is manual (`workflow_dispatch`) with
the optional inputs `suite` and `extra_packages`. Before `mmdebstrap` it sets up
Go, cross-compiles the backend from `app/` into `files/zybo-audio-web`, stages
the WebUI from `app/webui/` and regenerates `files/SHA256SUMS` over the payload.
The checksum manifest is therefore a build output (uploaded as the
`debian-files` artifact), not a tracked file. Artifacts: `debian-rootfs`
(`rootfs.tar.zst`, `rootfs.ext4`), `debian-files` (`SHA256SUMS`) and
`debian-build-log` on failure.

## Assembling an SD card

Collect the artifact sets — `zybo-linux`: `uImage`, `zybo-audio.dtb`;
`zybo-buildroot`: `boot.bin` (the `BOOT.BIN`), `u-boot.img`, `system.bit`,
`uEnv.txt`; `zybo-debian`: `rootfs.ext4` (or `rootfs.tar.zst`).

Method 1 — copy into existing partitions: `cp boot/* /mnt/boot/` for the FAT32
boot files, then `sudo tar -xpf rootfs.tar.zst -C /mnt/root --numeric-owner` for
the ext4 root.

Method 2 — build one image and `dd` it:

```bash
./scripts/make_sdcard.sh --boot ./boot --rootfs ./rootfs.ext4 --out sdcard.img
sudo dd if=sdcard.img of=/dev/sdX bs=4M status=progress conv=fsync
sudo ./scripts/grow_sdcard.sh /dev/sdX
```

`--boot` takes a directory with `BOOT.BIN` (or `boot.bin`), `u-boot.img`,
`uImage` and `zybo-audio.dtb`, plus optional `system.bit` / `boot.scr`. The image
has two partitions (`BOOT` FAT32, `/` ext4) and is sized to its contents; run
`grow_sdcard.sh` after flashing to expand the root partition to the card. The
boot arguments are `root=/dev/mmcblk0p2 rootwait rw console=ttyPS0,115200`.

## On the board

The serial console is `ttyPS0` at 115200 baud; MPD, shairport-sync (AirPlay,
`zybo-audio`) and the web panel (`http://<board-ip>:8080/`) start at boot.

```bash
aplay -l                     # card 0: ZyboSoundCard (48 kHz / S32_LE)
mpc add <file> && mpc play   # library in /var/lib/mpd/music
wpa_passphrase "SSID" "passphrase" >> /etc/wpa_supplicant/wpa_supplicant.conf
systemctl restart 'wpa_supplicant@*'
```

## Third-party licenses

`files/THIRD-PARTY.md` lists the third-party components distributed in the image
and `files/licenses/` holds their texts; `customize02` installs both under
`/usr/share/doc/zybo-audio/`. Debian packages keep their own copyright files.

## License

This repository is licensed under the **GNU General Public License, version 2**
(GPL-2.0); see [LICENSE](LICENSE) for the full text. It covers the hooks and
scripts, the Go backend and the WebUI in `app/`. Third-party components keep
their own licenses; see `files/THIRD-PARTY.md` and `files/licenses/`.

## Related repositories

- Kernel: [zybo-linux](https://github.com/xufooo/zybo-linux)
- Buildroot rootfs: [zybo-buildroot](https://github.com/xufooo/zybo-buildroot)
