#!/bin/sh
# ============================================================================
# customize04-volume.sh — one system volume knob: the codec's ALSA "Master"
# ============================================================================
# Why this hook exists:
#   Every audio path used to carry a volume of its own, so switching sources
#   changed the loudness:
#     - AirPlay   : shairport-sync applied the sender's volume in software
#     - WebUI/API : amixer sset Master <n>%   (the codec's hardware control)
#     - Bluetooth : bluealsa-aplay's own AVRCP volume
#     - mpd       : its own software volume
#   This product now has exactly one: the codec's ALSA "Master" control.
#     - customize03-shairport.sh sets alsa.mixer_control_name = "Master"
#       and general.volume_max_db = 0.0 (the control's top end is +5 dB)
#     - customize01-base.sh writes mpd.conf with mixer_type "none"
#     - this hook makes bluealsa-aplay drive the same control
#     - the backend reads the control back, so the WebUI follows the phone
#
# This hook is also the one place that *asserts* the invariant, so a later edit
# to either of the other hooks fails the image build instead of shipping a
# product whose volume jumps when the user switches sources.
#
# Usage (mmdebstrap customize-stage hook; runs on the host, chroot dir is $1):
#   sh hooks/customize04-volume.sh <rootfs dir>
# Idempotent: the drop-in is rewritten with the same content.
# ============================================================================
set -e

TARGET="${1:?usage: customize04-volume.sh <rootfs dir>}"

# --- Bluetooth: AVRCP volume drives the same ALSA control -------------------
DROPIN="$TARGET/etc/systemd/system/bluealsa-aplay.service.d"
mkdir -p "$DROPIN"
cat > "$DROPIN/10-volume.conf" <<'EOF'
# One system volume: the AVRCP absolute volume sent by the phone drives the same
# ALSA "Master" control that the WebUI and AirPlay use, so switching sources
# does not change the loudness. See hooks/customize04-volume.sh.
[Service]
ExecStart=
ExecStart=/usr/bin/bluealsa-aplay -S --volume=mixer --mixer-device=default --mixer-name=Master --mixer-index=0
EOF

# --- Assert the invariant (AirPlay side and mpd side) -----------------------
fail() { echo "[hook] ERROR: unified volume invariant broken: $1" >&2; exit 1; }

SH="$TARGET/etc/shairport-sync.conf"
[ -f "$SH" ] || fail "missing $SH"
grep -qE '^[[:space:]]*mixer_control_name[[:space:]]*=[[:space:]]*"Master"[[:space:]]*;' "$SH" \
    || fail 'shairport-sync alsa.mixer_control_name is not "Master" (AirPlay would use a private software gain)'
grep -qE '^[[:space:]]*volume_max_db[[:space:]]*=[[:space:]]*0(\.0)?[[:space:]]*;' "$SH" \
    || fail 'shairport-sync general.volume_max_db is not 0.0 (the codec control reaches +5 dB and would clip)'

MPD="$TARGET/etc/mpd.conf"
[ -f "$MPD" ] || fail "missing $MPD"
bad="$(grep -cE '^[[:space:]]*mixer_type[[:space:]]+"software"' "$MPD" || true)"
[ "$bad" -eq 0 ] || fail "mpd.conf still has mixer_type \"software\" ($bad occurrence(s)); mpd would keep its own volume"

echo "[hook] unified volume: the codec's ALSA Master control is the only one"
echo "  shairport-sync : $(grep -hE '^[[:space:]]*mixer_control_name' "$SH" | sed 's/^[[:space:]]*//')"
echo "  shairport-sync : $(grep -hE '^[[:space:]]*volume_max_db' "$SH" | sed 's/^[[:space:]]*//')"
echo "  mpd            : $(grep -hE '^[[:space:]]*mixer_type' "$MPD" | head -1 | sed 's/^[[:space:]]*//')"
echo "  bluealsa-aplay : $(grep -h '^ExecStart=' "$DROPIN/10-volume.conf")"
