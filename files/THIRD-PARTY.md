# Third-party components and open-source licenses (THIRD-PARTY)

> Version: see `VERSION` at the repository root (currently `0.1.0-beta`).
> This document records every piece of third-party code/data distributed with the
> release, its license, and the obligations we have to meet.
> License texts live in `licenses/` (copied from the source, not transcribed).

## 0. Summary

| | Status |
|---|---|
| Kernel / U-Boot / Go backend / Debian packages | ✅ obligations understood, compliant; the GPL-2.0 / LGPL-2.1 / MIT texts are shipped in the image (§2) |
| Digilent's own VHDL (wrapper, DMA FIFO) | ✅ MIT, keep the notice |
| **6 ADI-derived VHDL files** | ⚠️ **dual-licensed: GPL-2.0 or ADI-BSD (the latter is venue-restricted)**; the vendored copy is an older revision carrying only ADI-BSD -> see §4 |
| Our own code | ⚠️ **no license chosen yet** (see §6) |

---

## 1. RTL distributed with the **bitstream** (boot partition `system.bit`)

| File | Source | License | Obligation |
|---|---|---|---|
| `axi_i2s_adi_v1_2.vhd`, `axi_i2s_adi_S_AXI.vhd`, `dma_fifo.vhd`, `pl330_dma_fifo.vhd`, `axi_streaming_dma_tx/rx_fifo.vhd`, `component.xml`, `xgui/` | Digilent [vivado-library](https://github.com/Digilent/vivado-library) @ `f4613fff005b098065fd5d619a2b88e55720a423` | **MIT** (`License.txt`, Copyright (c) 2017 Digilent) | Keep the copyright + license notice (`licenses/Digilent-MIT.txt`) |
| **`i2s_controller.vhd`, `i2s_tx.vhd`, `i2s_rx.vhd`, `i2s_clkgen.vhd`, `fifo_synchronizer.vhd`, `adi_common/axi_ctrlif.vhd`** | Same (copied by Digilent, code originally from ADI) | **ADI BSD + venue restriction** (file header: Copyright 2013 (c) Analog Devices, Inc.) | ⚠️ see §4 |
| Rest of `hdl/adi_common/` | Same | No ADI copyright in the file header (treated as repository-level MIT) | Keep the notice |
| Xilinx IP: PS7, AXI DMA, AXI IIC, and Vivado-generated netlists/primitives | Vivado 2024.1 | Xilinx EULA (proprietary) | The bitstream may only run on Xilinx devices; building requires a valid Vivado license |
| Our own RTL (`dsp_insert.v`, `biquad_filter.v`, `limiter.v`, `saturator.v`, `volume_control.v`, `tb/*`) | This project | see §6 | — |

Comments in `saturator.v` and `fpga/src/hdl/dsp/volume_control.v` carry
`Reference: ZedEQ (ayu-sshhh/ZedEQ, MIT license)` -- that repository is **MIT, Copyright (c) 2026 Lucifer**
(text in `licenses/ZedEQ-MIT.txt`). It is a reference implementation and is credited under MIT.

## 2. Components distributed with the **rootfs**

| Component | Version | License | Obligation |
|---|---|---|---|
| Linux kernel (Xilinx `linux-xlnx`) | 6.6.80, `zybo-linux` @ `e1b1ed6` | **GPL-2.0** | Distributing the binary (inside the released `uImage`, a FIT carrying the kernel + DTB) requires making the **corresponding source** available; the `zybo-linux` repository is that source |
| U-Boot (bundled inside the released `BOOT.BIN`, FSBL form) | see `zybo-buildroot` | **GPL-2.0+** | same as above; the `zybo-buildroot` repository is the source |
| Debian trixie packages (shairport-sync **MIT**, mpd **GPL-2+**, wpasupplicant **BSD-3**, bluez-alsa-utils **Expat**, alsa-utils **GPL-2**, openssh-server, avahi **LGPL-2.1+**, ...) | trixie snapshot 2026-09 | respective | The image ships the texts we owe ourselves: `licenses/GPL-2.0.txt`, `licenses/LGPL-2.1.txt`, `licenses/shairport-sync-MIT.txt` (see the table below). Every package's own `/usr/share/doc/<pkg>/copyright` is kept in the image by the package itself |
| **Debian `non-free-firmware`**: `firmware-realtek` / `-mediatek` / `-atheros` / `-misc-nonfree` | `20250410-2` | vendor blobs (**non-free**, redistributable) | The image ships `licenses/non-free-firmware-NOTICE.txt`, which **states that the image contains non-free firmware**; the per-blob terms are in each package's own copyright |
| Go runtime + standard library | go1.27.1 (statically linked into the backend binary) | **BSD-3-Clause**, Copyright (c) 2009 The Go Authors | Keep the notice (`licenses/Go-BSD-3-Clause.txt` + `Go-PATENTS.txt`) |
| `github.com/gorilla/websocket` | v1.5.3 | **BSD-3-Clause**, Copyright (c) 2013 The Gorilla WebSocket Authors | Keep the notice (`licenses/gorilla-websocket-BSD-3-Clause.txt`) |
| RBJ Audio EQ Cookbook (biquad coefficient formulas) | — | public reference (Robert Bristow-Johnson) | credited in the source comments (noted at the top of `backend/config.go`) |

The released boot set is the **FSBL form**: `BOOT.BIN` bundles the FSBL + PL bitstream +
U-Boot, and `uImage` is a FIT carrying the linux-xlnx kernel + DTB. The U-Boot SPL form
(`boot.bin` + `u-boot.img` + a separate `zybo-audio.dtb` on the boot partition) is a
historical/optional path and is not part of the release.

### License texts shipped in `/usr/share/doc/zybo-audio/licenses/`

Every text is a verbatim copy; the provenance (component -> license -> source) is:

| File | Covers | Source (version) |
|---|---|---|
| `licenses/GPL-2.0.txt` | U-Boot inside `BOOT.BIN`; linux-xlnx inside `uImage`; mpd; alsa-utils; the GPL-2.0 parts of the firmware packages | `u-boot` `v2024.01`, file `Licenses/gpl-2.0.txt` -- the same GPL-2.0 text that linux-xlnx `LICENSES/preferred/GPL-2.0` refers to |
| `licenses/LGPL-2.1.txt` | avahi-daemon / libavahi*; the LGPL-2+ parts of alsa-utils | Debian `base-files` `13.8+deb13u6`, `/usr/share/common-licenses/LGPL-2.1` |
| `licenses/shairport-sync-MIT.txt` | shairport-sync `4.3.7-1` (MIT; the package also bundles small ISC and BSD-3-Clause parts, documented in its own copyright) | Debian `shairport-sync` `4.3.7-1`, `/usr/share/doc/shairport-sync/copyright` (upstream: `github.com/mikebrady/shairport-sync`) |
| `licenses/non-free-firmware-NOTICE.txt` | the non-free firmware blobs listed above | written for this release; the per-blob texts stay in `/usr/share/doc/firmware-*/copyright` |
| `licenses/Go-BSD-3-Clause.txt`, `licenses/Go-PATENTS.txt` | Go runtime + standard library | the Go distribution |
| `licenses/gorilla-websocket-BSD-3-Clause.txt` | `github.com/gorilla/websocket` v1.5.3 | upstream `LICENSE` |
| `licenses/Digilent-MIT.txt`, `licenses/ADI-BSD-i2s-controller.txt`, `licenses/ZedEQ-MIT.txt` | RTL distributed in the bitstream (see §1) | see §1 |

Dependencies of the backend binary can be verified with Go's built-in metadata:

```bash
go version -m build/bin/zybo-audio-web     # list embedded modules and versions
```

## 3. Used for **development/build** only, not distributed with the image

Vivado 2024.1 (Xilinx, proprietary), the Go toolchain, Python 3,
`mmdebstrap` + `qemu-user-static` (CI), `parted/mkfs.vfat/mtools` (card assembly),
`socat`/serial tools; front-end verification uses `playwright`+Chromium and `jsdom`
(all MIT/Apache, local testing only).

None of these ship in the release, so they create no distribution obligation.

## 4. ⚠️ Key risk: ADI-derived HDL (**dual-licensed**, checked against the original)

The six files `i2s_controller.vhd`, `i2s_tx.vhd`, `i2s_rx.vhd`, `i2s_clkgen.vhd`,
`fifo_synchronizer.vhd` and `adi_common/axi_ctrlif.vhd` originate from ADI.
**ADI now dual-licenses them** (the repository root carries both `LICENSE_GPL2` and
`LICENSE_ADIBSD`):

```
-- Redistribution and use of source or resulting binaries ... are permitted under
-- one of the following two license terms:
--   1. The GNU General Public License version 2 ...        <- no venue restriction
--   OR
--   2. An ADI specific BSD license ... as long as it attaches to an ADI device.
```

- The **ADI-BSD branch** requires the code to run on an ADI device. This board is a
  Xilinx Zynq with a **TI** SSM2603, so that condition is not met and this branch
  **cannot be used**.
- The **GPL-2.0 branch** has no venue restriction, but requires providing the
  **corresponding source for the whole design** under GPL-2.0.
- **⚠️ The vendored copy is an early Digilent snapshot** (`f4613ff`) whose file
  headers carry **only ADI-BSD**, with no GPL option -> as built today, the
  bitstream satisfies neither branch.

Differences verified (case-insensitive): `i2s_tx`/`i2s_rx`/`i2s_clkgen` are
**identical** to the current ADI version; `axi_ctrlif` differs by 4 lines;
`i2s_controller` by 59 lines; `fifo_synchronizer` by 51 lines (the newer ADI version
splits the single `resetn` into `in_resetn`/`out_resetn`, a more robust CDC design).
The ports of `i2s_controller` match the ADI version, and `fifo_synchronizer` is only
instantiated by it, so the two can be swapped together.

**Two options**: **(G)** switch to the current ADI version (dual-licensed) and
distribute the whole design under GPL-2.0 (cost: our FPGA RTL must be open-sourced);
**(R)** rewrite the I2S core (cost: a few days of RTL work, but the RTL can stay
closed). The choice is still open.

## 5. Version number and artifacts

The single source of truth is **`VERSION`** at the repository root (currently
`0.1.0-beta`); it is:

- compiled into the backend (`-ldflags -X main.appVersion=...`), so the startup banner
  and `/api/status` both carry it;
- shown in the WebUI header;
- used for release artifact naming (`zybo-audio-<VERSION>-*`) and in this document.

**Why 0.x**: it has only been validated on a development board, with no production or
long-run testing, so calling it 1.0 would not be honest.

## 6. Open item: the project's own license

The repository currently has **no LICENSE** file (i.e. all rights reserved by
default). Publishing requires choosing one first:

- images only, no source -> optional, but the documentation should state that
  third-party components inside the image are covered by this document;
- source released -> a license must be chosen (MIT / Apache-2.0 / GPL-2.0 ...).
  Note: if the I2S engine is rewritten to work around §4, a permissive license
  (MIT/Apache) is simpler.

This decision is still open.
