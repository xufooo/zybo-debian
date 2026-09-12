#!/bin/sh
# ============================================================================
# test_shairport_hook.sh — dry-run the shairport-sync hook without mmdebstrap and assert
# ============================================================================
# customize03-shairport.sh deliberately does not chroot (it only reads and writes
# files under $TARGET), so a temporary directory can be used as the rootfs for
# verification. Three cases cover three shapes the file can take:
#   A. /etc/shairport-sync.conf missing in the rootfs -> a complete config must be created
#   B. Debian's fully commented sample                 -> must be uncommented/replaced in place
#   C. a debug-time "dirty" config (soxr + all diagnostics on) -> must be corrected to release values
# Idempotency is checked as well: running twice on the same file must be byte-identical.
#
# Usage: ./scripts/test_shairport_hook.sh
# Exit code 0 = all checks passed.
# ============================================================================
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
HOOK="$REPO/hooks/customize03-shairport.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

FAIL=0
ok()  { printf '  ✓ %s\n' "$1"; }
bad() { printf '  ✗ %s\n' "$1"; FAIL=$((FAIL + 1)); }

# Assert that an active line (not commented out with //) exists in the file
has() { # has <description> <file> <regex>
    if grep -qE "$3" "$2"; then ok "$1"; else bad "$1 (missing active line /$3/)"; fi
}
nhas() { # nhas <description> <file> <regex>  -- must NOT be present
    if grep -qE "$3" "$2"; then bad "$1 (must not match /$3/)"; else ok "$1"; fi
}
cnt() { # cnt <description> <file> <key> <expected count>
    n="$(grep -cE "^[[:space:]]*$3[[:space:]]*=" "$2" || true)"
    if [ "$n" -eq "$4" ]; then ok "$1"; else bad "$1 ($3 appears $n times, expected $4)"; fi
}

# Shared assertions for the four release settings
assert_release() { # assert_release <case name> <file>
    has "$1: interpolation=basic"      "$2" '^[[:space:]]*interpolation[[:space:]]*=[[:space:]]*"basic"[[:space:]]*;'
    has "$1: buffer=1.0"               "$2" '^[[:space:]]*audio_backend_buffer_desired_length_in_seconds[[:space:]]*=[[:space:]]*1\.0[[:space:]]*;'
    has "$1: log_verbosity=0"          "$2" '^[[:space:]]*log_verbosity[[:space:]]*=[[:space:]]*0[[:space:]]*;'
    has "$1: statistics=no"            "$2" '^[[:space:]]*statistics[[:space:]]*=[[:space:]]*"no"[[:space:]]*;'
    nhas "$1: no verbosity=3"          "$2" '^[[:space:]]*log_verbosity[[:space:]]*=[[:space:]]*3'
    nhas "$1: no statistics=yes"       "$2" '^[[:space:]]*statistics[[:space:]]*=[[:space:]]*"yes"'
    nhas "$1: no log_output_to=stderr" "$2" '^[[:space:]]*log_output_to[[:space:]]*=[[:space:]]*"stderr"'
    cnt  "$1: interpolation unique"   "$2" 'interpolation' 1
    cnt  "$1: buffer unique"          "$2" 'audio_backend_buffer_desired_length_in_seconds' 1
    cnt  "$1: log_verbosity unique"   "$2" 'log_verbosity' 1
    cnt  "$1: statistics unique"      "$2" 'statistics' 1
}

# --- Case A: config file missing -> create -----------------------------------
echo "== A. missing -> create =="
A="$T/a"; mkdir -p "$A/etc"
sh "$HOOK" "$A" > "$T/a.log" 2>&1 || { bad "hook exited non-zero"; cat "$T/a.log"; }
grep -q '^\[hook\] shairport-sync configuration applied$' "$T/a.log" && ok "printed '[hook] shairport-sync configuration applied'" \
    || bad "did not print the expected line"
[ -f "$A/etc/shairport-sync.conf" ] && ok "config created" || bad "config not created"
assert_release "A" "$A/etc/shairport-sync.conf"

# --- Case B: Debian package sample (fully commented) -> edit in place --------
echo "== B. Debian sample (fully commented) -> edit in place =="
B="$T/b"; mkdir -p "$B/etc"
cat > "$B/etc/shairport-sync.conf" <<'SAMPLE'
// Sample Configuration File for Shairport Sync
general =
{
//	name = "%H";
//	interpolation = "auto"; // aka "stuffing". Default is "auto".
//	output_backend = "alsa";
//	audio_backend_buffer_desired_length_in_seconds = 0.2; // desired buffer size
//		Too long and the response time to volume changes becomes annoying.
};

alsa =
{
//	output_device = "default";
//	period_size = <number>;
};

diagnostics =
{
//	disable_resend_requests = "no";
//	statistics = "no";
//	log_verbosity = 0;
//	log_show_file_and_line = "yes";
};
SAMPLE
B_BEFORE="$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)"
sh "$HOOK" "$B" > "$T/b.log" 2>&1 || { bad "hook exited non-zero"; cat "$T/b.log"; }
assert_release "B" "$B/etc/shairport-sync.conf"
# Other sections must not be modified by mistake
grep -q 'output_device = "default"' "$B/etc/shairport-sync.conf" && ok "B: alsa section untouched" || bad "B: alsa section was modified"
[ "$B_BEFORE" != "$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)" ] && ok "B: file did change" || bad "B: file unchanged (hook did not take effect)"

# --- Case C: debug-time dirty config -> corrected to release values ----------
echo "== C. debug-time dirty config -> corrected to release values =="
C="$T/c"; mkdir -p "$C/etc"
cat > "$C/etc/shairport-sync.conf" <<'DIRTY'
general =
{
	audio_backend_buffer_desired_length_in_seconds = 1.0;
	interpolation = "auto";
};

diagnostics =
{
log_output_to = "stderr";
statistics = "yes";
log_verbosity = 3;
};
DIRTY
sh "$HOOK" "$C" > "$T/c.log" 2>&1 || { bad "hook exited non-zero"; cat "$T/c.log"; }
assert_release "C" "$C/etc/shairport-sync.conf"

# --- Case D: idempotency (same file run twice gives the same result) ---------
echo "== D. idempotency =="
sh "$HOOK" "$B" > /dev/null 2>&1
B_RUN2="$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)"
sh "$HOOK" "$B" > /dev/null 2>&1
B_RUN3="$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)"
[ "$B_RUN2" = "$B_RUN3" ] && ok "D: second and third runs are byte-identical" || bad "D: not idempotent ($B_RUN2 vs $B_RUN3)"
assert_release "D" "$B/etc/shairport-sync.conf"

echo
if [ "$FAIL" -eq 0 ]; then echo "== all checks passed =="; else echo "== $FAIL check(s) failed =="; fi
exit "$FAIL"
