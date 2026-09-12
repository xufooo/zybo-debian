# 第三方组件与开源许可清单（THIRD-PARTY）

> 版本：见仓库根的 `VERSION`（当前 `0.1.0-beta`）。
> 本文记录**随发布物分发**的每一份第三方代码/数据、它的许可、以及我们要尽的义务。
> 许可原文放在 `licenses/`（都是从源头抓的原文，不是手抄）。

## 0. 一句话结论

| | 状态 |
|---|---|
| 内核 / U-Boot / Go 后端 / Debian 包 | ✅ 义务清楚，能合规 |
| Digilent 自己的 VHDL（wrapper、DMA FIFO） | ✅ MIT，保留声明即可 |
| **ADI 派生的 6 个 VHDL 文件** | ⚠️ **双许可：GPL-2.0 或 ADI-BSD（后者有场所限制）**；我方 vendor 的是旧版、只有 ADI-BSD → 见 §4 |
| 我们自己的代码 | ⚠️ **还没有选许可**（见 §6，需要你定） |

---

## 1. 随**位流**（boot 分区 `system.bit`）分发的 RTL

| 文件 | 来源 | 许可 | 义务 |
|---|---|---|---|
| `axi_i2s_adi_v1_2.vhd`、`axi_i2s_adi_S_AXI.vhd`、`dma_fifo.vhd`、`pl330_dma_fifo.vhd`、`axi_streaming_dma_tx/rx_fifo.vhd`、`component.xml`、`xgui/` | Digilent [vivado-library](https://github.com/Digilent/vivado-library) @ `f4613fff005b098065fd5d619a2b88e55720a423` | **MIT**（`License.txt`，Copyright (c) 2017 Digilent） | 保留版权 + 许可声明（`licenses/Digilent-MIT.txt`） |
| **`i2s_controller.vhd`、`i2s_tx.vhd`、`i2s_rx.vhd`、`i2s_clkgen.vhd`、`fifo_synchronizer.vhd`、`adi_common/axi_ctrlif.vhd`** | 同上（Digilent 拷贝，代码源自 ADI） | **ADI BSD + 场所限制**（文件头 Copyright 2013 (c) Analog Devices, Inc.） | ⚠️ 见 §4 |
| `hdl/adi_common/` 其余文件 | 同上 | 文件头未声明 ADI 版权（按仓库级 MIT 处理） | 保留声明 |
| Xilinx IP：PS7、AXI DMA、AXI IIC、以及 Vivado 生成的网表/原语 | Vivado 2024.1 | Xilinx EULA（专有） | 位流只能跑在 Xilinx 器件上；构建需合法 Vivado 许可 |
| 我们自己的 RTL（`dsp_insert.v`、`biquad_filter.v`、`limiter.v`、`saturator.v`、`volume_control.v`、`tb/*`） | 本项目 | 见 §6 | — |

`saturator.v` 与 `fpga/src/hdl/dsp/volume_control.v` 的注释标注了
`Reference: ZedEQ (ayu-sshhh/ZedEQ, MIT license)` —— 该仓库为 **MIT，Copyright (c) 2026 Lucifer**
（原文 `licenses/ZedEQ-MIT.txt`）。属于"参考实现"，仍按 MIT 署名。

## 2. 随 **rootfs** 分发的组件

| 组件 | 版本 | 许可 | 义务 |
|---|---|---|---|
| Linux 内核（Xilinx `linux-xlnx`） | 6.6.80，`zybo-linux` @ `e1b1ed6` | **GPL-2.0** | 分发二进制（`uImage`）须能提供**对应源码**；源码仓 `zybo-linux` 即对应源码 |
| U-Boot（SPL + `u-boot.img`） | 见 `zybo-buildroot` | **GPL-2.0+** | 同上 |
| Debian trixie 各包（shairport-sync **MIT**、mpd **GPL-2+**、wpasupplicant **BSD-3**、bluez-alsa-utils **Expat**、alsa-utils **GPL-2**、openssh-server、avahi …） | 2026-09 快照 | 各自 | 镜像内 `/usr/share/doc/<pkg>/copyright` 已随包保留（已实测存在）；Debian 自己的合规由各包维护 |
| **Debian `non-free-firmware`**：`firmware-realtek` / `-mediatek` / `-atheros` / `-misc-nonfree` | `20250410-2` | 厂商 blob（**非自由**，可再分发） | 必须在文档里**声明镜像含非自由固件**；具体条款见各包 copyright |
| Go 运行时 + 标准库 | go1.27.1（静态链入后端二进制） | **BSD-3-Clause**，Copyright (c) 2009 The Go Authors | 保留声明（`licenses/Go-BSD-3-Clause.txt` + `Go-PATENTS.txt`） |
| `github.com/gorilla/websocket` | v1.5.3 | **BSD-3-Clause**，Copyright (c) 2013 The Gorilla WebSocket Authors | 保留声明（`licenses/gorilla-websocket-BSD-3-Clause.txt`） |
| RBJ Audio EQ Cookbook（双二阶系数公式） | — | 公开参考（Robert Bristow-Johnson） | 源码注释里署名（已在 `backend/config.go` 头部说明） |

后端二进制的依赖可以用 Go 自带信息核对（发布时留档）：

```bash
go version -m build/bin/zybo-audio-web     # 列出内嵌模块与版本
```

## 3. 仅用于**开发/构建**，不随镜像分发

Vivado 2024.1（Xilinx，专有）、Go 工具链、Python 3、
`mmdebstrap` + `qemu-user-static`（CI）、`parted/mkfs.vfat/mtools`（组卡）、
`socat`/串口工具；前端验证用 `playwright`+Chromium、`jsdom`（都是 MIT/Apache，仅本机测试用）。

这些不进入发布物，故不产生分发义务。

## 4. ⚠️ 关键风险：ADI 派生 HDL（**双许可**，已核到原文）

`i2s_controller.vhd`、`i2s_tx.vhd`、`i2s_rx.vhd`、`i2s_clkgen.vhd`、
`fifo_synchronizer.vhd`、`adi_common/axi_ctrlif.vhd` 这 6 个文件源自 ADI。
**ADI 仓库现在给它们是双许可**（根目录同时有 `LICENSE_GPL2` 与 `LICENSE_ADIBSD`）：

```
-- Redistribution and use of source or resulting binaries ... are permitted under
-- one of the following two license terms:
--   1. The GNU General Public License version 2 ...        ← 无场所限制
--   OR
--   2. An ADI specific BSD license ... as long as it attaches to an ADI device.
```

- **ADI-BSD 那一支**要求"必须运行在 ADI 器件上"——本板是 Xilinx Zynq + **TI** SSM2603，
  不满足，**不能用**。
- **GPL-2.0 那一支**没有场所限制，但要把**整个设计的对应源码**按 GPL-2.0 提供。
- **⚠️ 我方 vendor 的是 Digilent 早年拷贝**（`f4613ff`），文件头**只有 ADI-BSD**、
  没有 GPL 选项 → 照现在这份构建的位流，**两支都用不了**。

差异已核实（忽略大小写）：`i2s_tx`/`i2s_rx`/`i2s_clkgen` 与 ADI 当前版**完全一致**；
`axi_ctrlif` 差 4 行；`i2s_controller` 差 59 行；`fifo_synchronizer` 差 51 行
（ADI 新版把单一 `resetn` 拆成 `in_resetn`/`out_resetn`，CDC 更稳）。
`i2s_controller` 的端口与 ADI 版一致，`fifo_synchronizer` 只被它实例化 → 两个一起换。

**出路（二选一）**：**G** 换成 ADI 当前版（双许可）+ 整个设计按 GPL-2.0 分发
（代价：我们的 FPGA RTL 要开源）；**R** 自己重写 I2S 核心（代价：几天 RTL 工作，
但 RTL 可不开源）。→ 决策与门禁见 **`docs/PUBLISHING.md` §3**。

## 5. 版本号与产物对应

单一来源是仓库根的 **`VERSION`**（当前 `0.1.0-beta`），它会被：

- 编进后端（`-ldflags -X main.appVersion=...`），启动横幅与 `/api/status` 都会带；
- 显示在 WebUI 页眉；
- 用于发布物命名（`zybo-audio-<VERSION>-*`）与本文档。

**为什么是 0.x**：只在开发板上验证过，没有量产测试、没有长期运行测试，
按常识不该叫 1.0。

## 6. 待定：本项目自己的许可

仓库目前**没有 LICENSE**（即默认"保留所有权利"）。要公开就得先选一个：

- 只发布镜像、不发布源码 → 可以不选，但建议写明"镜像内第三方组件见本文"；
- 发布源码 → 需要选（MIT / Apache-2.0 / GPL-2.0 …）。
  注意：如果将来要绕开 §4 自己重写 I2S 引擎，选宽松许可（MIT/Apache）更省事。

**这一条需要你决定，我不替你选。**
