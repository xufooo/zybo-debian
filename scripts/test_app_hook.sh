#!/bin/sh
# ============================================================================
# test_app_hook.sh — dry-run the app hook without mmdebstrap and assert the tree
# ============================================================================
# customize02-app.sh deliberately does not chroot, so a temporary directory can
# be used as the rootfs for verification.
# Usage: ./scripts/test_app_hook.sh
# Exit code 0 = all checks passed.
# ============================================================================
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

FAIL=0
ok() { printf '  ✓ %s\n' "$1"; }
bad() { printf '  ✗ %s\n' "$1"; FAIL=$((FAIL + 1)); }
chk() { [ -e "$2" ] && ok "$1" || bad "$1 (missing $2)"; }
# Use -L for symlinks: the target is an absolute path inside the image, so -e fails on the host
chklink() { [ -L "$2" ] && ok "$1" || bad "$1 (missing symlink $2)"; }

echo "== dry-run hooks/customize02-app.sh -> $T =="
ZYBO_FILES="$REPO/files" sh "$REPO/hooks/customize02-app.sh" "$T"

echo "== assertions =="
chk "backend binary"           "$T/usr/local/bin/zybo-audio-web"
chk "WebUI"                    "$T/var/www/zybo-audio/index.html"
chk "favicon"                  "$T/var/www/zybo-audio/assets/favicon.svg"
chk "systemd unit"             "$T/usr/lib/systemd/system/zybo-audio-web.service"
chklink "unit enabled (symlink)" "$T/etc/systemd/system/multi-user.target.wants/zybo-audio-web.service"
chk "version shipped"          "$T/usr/share/doc/zybo-audio/VERSION"
chk "third-party license list" "$T/usr/share/doc/zybo-audio/THIRD-PARTY.md"

# The binary must be a runnable static ARM executable (the board is armv7)
if file "$T/usr/local/bin/zybo-audio-web" 2>/dev/null | grep -q "ARM, EABI5.*statically linked"; then
    ok "binary is static ARM"
else
    bad "binary is not static ARM"
fi

# The unit's ExecStart must point at the path that was actually installed
EXEC="$(grep -m1 '^ExecStart=' "$T/usr/lib/systemd/system/zybo-audio-web.service" | cut -d= -f2)"
if [ -x "$T$EXEC" ]; then ok "ExecStart points at the installed binary ($EXEC)"; else bad "ExecStart $EXEC does not exist"; fi

# The enable symlink must point at a real unit
LINK="$(readlink "$T/etc/systemd/system/multi-user.target.wants/zybo-audio-web.service")"
if [ "$LINK" = "/usr/lib/systemd/system/zybo-audio-web.service" ]; then
    ok "enable symlink target is correct"
else
    bad "unexpected enable symlink target: $LINK"
fi

# License texts (compliance requirement): Digilent MIT and Go BSD-3 at minimum
for f in Digilent-MIT.txt Go-BSD-3-Clause.txt gorilla-websocket-BSD-3-Clause.txt; do
    chk "license text $f" "$T/usr/share/doc/zybo-audio/licenses/$f"
done

if [ "$FAIL" -eq 0 ]; then echo "== all checks passed =="; else echo "== $FAIL check(s) failed =="; fi
exit "$FAIL"
