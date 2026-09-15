#!/bin/sh
# ============================================================================
# customize04-volume.sh — separate volumes: the sender keeps its own, the board
#                         keeps exactly one
# ============================================================================
# The rule this hook enforces:
#
#   * the BOARD has exactly one volume, the codec's ALSA "Master" control, and
#     the WebUI/REST API is the only writer of it. It therefore sounds the same
#     whichever source is selected.
#   * every SOURCE keeps its own volume, applied in software *inside that
#     source*: the phone's AirPlay slider (shairport-sync's software mixer) and
#     the phone's AVRCP slider over Bluetooth (bluealsa-aplay --volume=software).
#     Those adjust that one stream only and never move the board's volume.
#
# Why not let the sources drive the codec mixer (the obvious "one knob" design)?
# It was tried and reverted: with alsa.mixer_control_name = "Master", the phone
# overwrote the board volume (the slider looked stuck) and shairport-sync
# re-wrote the control on every DACP poll, which is audible as pops (measured:
# 346 mixer operations in 20 minutes). Keeping the sender's volume in software
# also puts that attenuation *before* the DSP, which leaves the EQ its headroom.
#
# How this is wired:
#   - customize03-shairport.sh: no mixer_control_name (software volume), plus
#     volume_max_db = 0.0 and volume_range_db = 60 for the software mixer
#   - customize01-base.sh: mpd.conf with mixer_type "none" (mpd adds no volume)
#   - this hook: bluealsa-aplay --volume=software, and the assertions below
#   - the backend writes/reads the codec control for the WebUI, calibrated in dB
#
# Usage (mmdebstrap customize-stage hook; runs on the host, chroot dir is $1):
#   sh hooks/customize04-volume.sh <rootfs dir>
# Idempotent: the drop-in is rewritten with the same content.
# ============================================================================
set -e

TARGET="${1:?usage: customize04-volume.sh <rootfs dir>}"

# --- Bluetooth: the AVRCP volume stays on the Bluetooth stream --------------
DROPIN="$TARGET/etc/systemd/system/bluealsa-aplay.service.d"
mkdir -p "$DROPIN"
cat > "$DROPIN/10-volume.conf" <<'EOF'
# Separate volumes: the phone's AVRCP volume is applied in software to the
# Bluetooth stream only; the board's own volume is the codec's ALSA Master,
# written by the WebUI. The two never fight. See hooks/customize04-volume.sh.
[Service]
ExecStart=
ExecStart=/usr/bin/bluealsa-aplay -S --volume=software
EOF

# --- Assert the invariant ---------------------------------------------------
fail() { echo "[hook] ERROR: volume separation invariant broken: $1" >&2; exit 1; }

SH="$TARGET/etc/shairport-sync.conf"
[ -f "$SH" ] || fail "missing $SH"
if grep -qE '^[[:space:]]*mixer_control_name[[:space:]]*=' "$SH"; then
    fail 'shairport-sync drives the codec mixer: the sender would overwrite the board volume and re-write it on every poll (pops)'
fi
grep -qE '^[[:space:]]*volume_max_db[[:space:]]*=[[:space:]]*0(\.0)?[[:space:]]*;' "$SH" \
    || fail 'shairport-sync general.volume_max_db is not 0.0'
grep -qE '^[[:space:]]*volume_range_db[[:space:]]*=[[:space:]]*60[[:space:]]*;' "$SH" \
    || fail 'shairport-sync general.volume_range_db is not 60'

MPD="$TARGET/etc/mpd.conf"
[ -f "$MPD" ] || fail "missing $MPD"
bad="$(grep -cE '^[[:space:]]*mixer_type[[:space:]]+"software"' "$MPD" || true)"
[ "$bad" -eq 0 ] || fail "mpd.conf still has mixer_type \"software\" ($bad occurrence(s)); mpd would keep its own volume"

grep -q -- '--volume=software' "$DROPIN/10-volume.conf" \
    || fail 'bluealsa-aplay does not use --volume=software (the phone would drive the board volume)'

echo "[hook] volumes are separate: board = ALSA Master (WebUI only), sources = their own"
echo "  shairport-sync : software volume (mixer_control_name absent)"
echo "  shairport-sync : $(grep -hE '^[[:space:]]*volume_max_db' "$SH" | sed 's/^[[:space:]]*//')"
echo "  shairport-sync : $(grep -hE '^[[:space:]]*volume_range_db' "$SH" | sed 's/^[[:space:]]*//')"
echo "  mpd            : $(grep -hE '^[[:space:]]*mixer_type' "$MPD" | head -1 | sed 's/^[[:space:]]*//')"
echo "  bluealsa-aplay : $(grep -h '^ExecStart=' "$DROPIN/10-volume.conf")"
