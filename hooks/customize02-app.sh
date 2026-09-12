#!/bin/sh
# ============================================================================
# customize02-app.sh — install the ZYBO Audio backend + WebUI into the rootfs
# ============================================================================
# Artifacts are staged into this repository's files/ directory by the source
# tree's tools/stage_release.sh.
#
# NOTE: this hook deliberately does NOT call chroot (it only uses install/cp/ln),
#    so it can be dry-run on the host with a temporary directory as $TARGET:
#       TARGET=/tmp/fakeroot ZYBO_FILES=$PWD/files sh hooks/customize02-app.sh
#    That makes "what actually ends up in the image" testable, instead of only
#    being known after a full CI run.
#
# Installed layout:
#   /usr/local/bin/zybo-audio-web                   backend binary (static ARM)
#   /var/www/zybo-audio/index.html + assets/        WebUI
#   /usr/lib/systemd/system/zybo-audio-web.service  service unit (enabled)
#   /usr/share/doc/zybo-audio/                      version + third-party licenses (compliance)
# ============================================================================
set -e

TARGET="${1:?usage: customize02-app.sh <rootfs dir>}"
FILES="${ZYBO_FILES:-}"

if [ -z "$FILES" ] || [ ! -f "$FILES/zybo-audio-web" ]; then
    echo "[hook] WARN: ZYBO_FILES/zybo-audio-web not provided -- the image will have no web panel"
    echo "[hook]       stage it first with tools/stage_release.sh in the source tree"
    exit 0
fi

VER="$(cat "$FILES/VERSION" 2>/dev/null || echo unknown)"
echo "[hook] installing ZYBO Audio panel v$VER"

install -D -m 755 "$FILES/zybo-audio-web"            "$TARGET/usr/local/bin/zybo-audio-web"
install -D -m 644 "$FILES/webui/index.html"          "$TARGET/var/www/zybo-audio/index.html"
if [ -f "$FILES/webui/favicon.svg" ]; then
    install -D -m 644 "$FILES/webui/favicon.svg"     "$TARGET/var/www/zybo-audio/assets/favicon.svg"
fi

# Unit goes to /usr/lib/systemd/system (the distro location); local overrides live in /etc/systemd/system
install -D -m 644 "$FILES/zybo-audio-web.service"    "$TARGET/usr/lib/systemd/system/zybo-audio-web.service"

# Enable it (equivalent to systemctl enable; without chroot this is just a symlink)
WANT="$TARGET/etc/systemd/system/multi-user.target.wants"
mkdir -p "$WANT"
ln -sf "/usr/lib/systemd/system/zybo-audio-web.service" "$WANT/zybo-audio-web.service"
echo "[hook] enabled zybo-audio-web.service"

# --- Compliance: ship the version and third-party licenses with the image -----
DOC="$TARGET/usr/share/doc/zybo-audio"
install -D -m 644 "$FILES/VERSION"                   "$DOC/VERSION"
if [ -f "$FILES/THIRD-PARTY.md" ]; then
    install -D -m 644 "$FILES/THIRD-PARTY.md"        "$DOC/THIRD-PARTY.md"
fi
if [ -d "$FILES/licenses" ]; then
    install -d "$DOC/licenses"
    cp -a "$FILES/licenses/." "$DOC/licenses/"
    echo "[hook] installed third-party license texts with the image ($(ls "$FILES/licenses" | wc -l) files)"
fi

echo "[hook] done: zybo-audio-web v$VER + WebUI + licenses"
