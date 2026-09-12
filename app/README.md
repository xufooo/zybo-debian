# zybo-audio-web application sources

Go backend and WebUI for the ZYBO (Zynq-7000) audio player. The rootfs CI
workflow in this repository compiles the backend from this directory and installs
it into the Debian image; none of the files here are build products.

## Layout

```
backend/                 Go sources (package main)
  api.go config.go dsp.go main.go source.go status.go ws.go
  dsp_design_test.go     offline DSP design tests
go.mod  go.sum           Go module (module zybo-audio-web, gorilla/websocket)
webui/                   WebUI source (single-page index.html + assets/)
zybo-audio-web.service   systemd unit
```

## Build

Cross-compile the static ARM binary that the Cortex-A9 runs:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -trimpath -ldflags="-s -w" -o ../files/zybo-audio-web ./backend
```

`-trimpath` is required: without it the binary embeds the builder's absolute
source paths. `CGO_ENABLED=0` produces a statically linked binary. This is
exactly the command the CI workflow runs before `mmdebstrap`.

Run the offline design tests with:

```bash
go test ./...
```

## Requirements

- Go 1.21 or newer
- network access (or a warm module cache) for the single dependency,
  `github.com/gorilla/websocket` v1.5.3

## What the backend does

- REST API: status, DSP/EQ control, limiter, volume, source selection,
  reboot/shutdown
- WebSocket push of player, DSP and system state
- manages the audio source processes (shairport-sync, ...) through systemd
- controls the FPGA DSP registers over `/dev/mem` (mmap)
- serves the WebUI from `/var/www/zybo-audio`

## License

GPL-2.0; see [LICENSE](LICENSE). The Go runtime and
`github.com/gorilla/websocket` are BSD-3-Clause, and the biquad coefficient
formulas follow the public RBJ Audio EQ Cookbook (credited in
`backend/config.go`).
