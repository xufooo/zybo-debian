#!/bin/sh
# ============================================================================
# customize04-volume.sh — the board keeps exactly one volume, and no sender can
#                         move it
# ============================================================================
# The rule this hook enforces:
#
#   * the BOARD has exactly one volume, the codec's ALSA "Master" control, and
#     the WebUI/REST API is the only writer of it. It therefore sounds the same
#     whichever source is selected.
#   * a SOURCE either keeps its own volume or ignores the sender's entirely:
#       - AirPlay: the phone's slider is IGNORED (ignore_volume_control = "yes").
#         Applying it in software lands on shairport-sync's own 0..-96.1 dB
#         scale, so a phone at about -20 dB ends up ~64 dB below every other
#         source -- which is heard as "huge noise floor, faint vocal".
#       - Bluetooth: bluealsa-aplay --volume=software puts the BlueALSA PCM into
#         soft-volume mode. Per bluealsa(8), soft-volume "does not interact with
#         the Bluetooth AVRCP volume property": it scales samples by bluealsa's
#         OWN volume, which defaults to 100% (full scale) on first connect and is
#         then remembered per device in /var/lib/bluealsa/. So the phone's slider
#         does not change the loudness and the stream reaches the board at unity,
#         the same way AirPlay does. It is --volume=mixer (or =auto with a PCM in
#         native mode) that hands AVRCP to an ALSA mixer -- i.e. to the board's
#         Master control -- and that is exactly what this hook prevents.
#         (An earlier version of this note claimed the AVRCP slider was applied
#         in software; that was a wrong reading of bluealsa's volume modes.)
#     Either way a source never writes the board's volume.
#
# Why not let the sources drive the codec mixer (the obvious "one knob" design)?
# It was tried and reverted: with alsa.mixer_control_name = "Master", the phone
# overwrote the board volume (the slider looked stuck) and shairport-sync
# re-wrote the control on every DACP poll, which is audible as pops (measured:
# 346 mixer operations in 20 minutes). With the sender's volume ignored instead,
# the stream arrives at full scale, so the board volume alone sets the loudness
# and switching sources cannot change it -- and the EQ keeps its headroom.
#
# How this is wired:
#   - customize03-shairport.sh: ignore_volume_control = "yes" (the sender's
#     slider is dropped; volume_max_db = 0.0 and volume_range_db = 60 are kept
#     as a bound on the software mixer that would apply it)
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
# Separate volumes: force the BlueALSA PCM into soft-volume mode, so AVRCP (the
# phone's volume slider) is not linked to any ALSA mixer. The stream reaches the
# board's own volume (codec ALSA Master, written by the WebUI) at unity gain --
# the same treatment AirPlay gets via ignore_volume_control. See
# hooks/customize04-volume.sh for the full reasoning.
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
# The sender's volume must be ignored outright: applied in software it lands on
# shairport's own -96 dB scale, so AirPlay ends up ~64 dB below every other source.
grep -qE '^[[:space:]]*ignore_volume_control[[:space:]]*=[[:space:]]*"yes"[[:space:]]*;' "$SH" \
    || fail 'shairport-sync general.ignore_volume_control is not "yes" (AirPlay would be ~64 dB quieter than the other sources)'

MPD="$TARGET/etc/mpd.conf"
[ -f "$MPD" ] || fail "missing $MPD"
bad="$(grep -cE '^[[:space:]]*mixer_type[[:space:]]+"software"' "$MPD" || true)"
[ "$bad" -eq 0 ] || fail "mpd.conf still has mixer_type \"software\" ($bad occurrence(s)); mpd would keep its own volume"

grep -q -- '--volume=software' "$DROPIN/10-volume.conf" \
    || fail 'bluealsa-aplay does not use --volume=software: with --volume=mixer (or =auto on a native-mode PCM) bluealsa-aplay would operate the ALSA Master control, so the phone would drive the board volume'

echo "[hook] volumes are separate: board = ALSA Master (WebUI only), sources = their own"
echo "  shairport-sync : software volume (mixer_control_name absent)"
echo "  shairport-sync : $(grep -hE '^[[:space:]]*volume_max_db' "$SH" | sed 's/^[[:space:]]*//')"
echo "  shairport-sync : $(grep -hE '^[[:space:]]*volume_range_db' "$SH" | sed 's/^[[:space:]]*//')"
echo "  mpd            : $(grep -hE '^[[:space:]]*mixer_type' "$MPD" | head -1 | sed 's/^[[:space:]]*//')"
echo "  bluealsa-aplay : $(grep -h '^ExecStart=' "$DROPIN/10-volume.conf")"
