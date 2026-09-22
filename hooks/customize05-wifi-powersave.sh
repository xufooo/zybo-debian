#!/bin/sh
# ============================================================================
# customize05-wifi-powersave.sh — disable WiFi power save (otherwise the uplink
# throughput collapses to ~14 KB/s)
# ============================================================================
# Why this exists (measured 2026-09-20):
#
#   The board's WiFi is a USB dongle (wlx085700a58fd0). With power save on,
#   `iw dev <if> link` reports:
#       rx bitrate: 1.0 MBit/s   (tx 6.5 MBit/s MCS 0, signal -59 dBm)
#   so scp'ing the 7.7 MB backend binary takes **9 minutes** (~14 KB/s); worse,
#   when the outer `timeout` kills it the truncated binary is left on the board
#   and the service crash-loops (`fatal error: invalid function symbol table`).
#   After `iw dev <if> set power_save off` rx goes **1.0 -> 2.0 MBit/s** at once,
#   and together with `tools/board.sh --putz` (compressed push) the same binary
#   goes **9 min -> 2 min 11 s**.
#
# Why a systemd unit instead of a networkd setting:
#   * the board manages wireless with systemd-networkd + wpa_supplicant, but the
#     .network file format has no stable "disable WiFi power save" option (the
#     wording differs between versions);
#   * a unit plus a small script is **version independent**, and it can retry
#     when the interface comes up before the network does.
#
# NOTE: **the interface name is not hard-coded** — a different USB dongle gets a
#   different name (wlx+MAC). The script scans `/sys/class/net/*/wireless` and
#   disables power save on every wireless interface it finds.
#
# Usage (one file, two scenarios):
#   1) while building the rootfs: called automatically by mmdebstrap's
#      --hook-dir, with $TARGET as the argument;
#   2) on a live board: `TARGET=/ sh customize05-wifi-powersave.sh /` followed by
#      `systemctl daemon-reload && systemctl enable --now wifi-powersave-off`.
#      (This script does **not** chroot and does **not** call systemctl, so it
#      works in both scenarios.)
# ============================================================================
set -eu

TARGET="${1:?usage: customize05-wifi-powersave.sh <rootfs dir>}"

S="$TARGET/usr/local/sbin/wifi-powersave-off.sh"
U="$TARGET/etc/systemd/system/wifi-powersave-off.service"
W="$TARGET/etc/systemd/system/multi-user.target.wants"

mkdir -p "$(dirname "$S")" "$(dirname "$U")" "$W"

# -- (1) runtime script (identical for a rootfs and a live board) -----------
cat > "$S" <<'EOS'
#!/bin/sh
# wifi-powersave-off.sh — disable power save on every wireless interface;
# retries while the interface is not up yet.
# Called by wifi-powersave-off.service at boot (or manually: just run it).
IW=/usr/sbin/iw
[ -x "$IW" ] || IW=$(command -v iw 2>/dev/null || echo /usr/sbin/iw)

apply() {
    found=0
    for w in /sys/class/net/*/wireless; do
        [ -e "$w" ] || continue
        iface=$(basename "$(dirname "$w")")
        found=1
        "$IW" dev "$iface" set power_save off 2>/dev/null || return 1
    done
    [ "$found" = 1 ] || return 2   # no wireless interface yet
    return 0
}

# wait up to ~36 s (network/driver may come up after this unit)
n=0
while [ "$n" -lt 12 ]; do
    # NOTE: the exit status must be captured explicitly: after
    #   `if apply; then break; fi` a later `$?` is the status of the `if`
    #   statement itself (always 0), so the branch would never be taken.
    apply && break
    n=$((n + 1))
    sleep 3
done

# read back and log one line, so it is easy to tell later whether it took effect
for w in /sys/class/net/*/wireless; do
    [ -e "$w" ] || continue
    iface=$(basename "$(dirname "$w")")
    echo "wifi-powersave: $iface -> $("$IW" dev "$iface" get power_save 2>&1)"
done
exit 0
EOS
chmod 755 "$S"

# -- (2) systemd unit ------------------------------------------------------
cat > "$U" <<'EOU'
[Unit]
Description=Disable WiFi power save (keeps AirPlay uplink throughput usable)
Documentation=man:iw(8)
# run after the network is up; the wireless interface may still be waiting for
# wpa_supplicant to associate, which is why the script retries
After=network.target systemd-networkd.service wpa_supplicant.service
Wants=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/wifi-powersave-off.sh
# a failure must not break boot: this only affects throughput, not audio
SuccessExitStatus=0 1 2

[Install]
WantedBy=multi-user.target
EOU
chmod 644 "$U"

# -- (3) enable it (create the wants symlink by hand: there is no systemd to
#        talk to at build time, and this works on a live board as well) -----
ln -sf ../wifi-powersave-off.service "$W/wifi-powersave-off.service"

echo "customize05: installed $S + $U and linked it into multi-user.target.wants/" >&2
