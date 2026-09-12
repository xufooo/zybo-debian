#!/bin/sh
# ============================================================================
# test_shairport_hook.sh — 不跑 mmdebstrap，直接干跑 shairport-sync hook 并断言
# ============================================================================
# customize03-shairport.sh 刻意不 chroot（只读写 $TARGET 下的文件），所以可以拿
# 临时目录当 rootfs 验证。三个用例覆盖三种落地形态：
#   A. rootfs 里没有 /etc/shairport-sync.conf   → 必须新建出完整配置
#   B. 是 Debian 包自带的样例（全注释）          → 必须就地解注释/替换
#   C. 是排查期的"脏"配置（soxr + 诊断全开）     → 必须被纠回发布值
# 另外验幂等：同一份文件连跑两次，结果必须逐字节相同。
#
# 用法：./scripts/test_shairport_hook.sh
# 退出码 0 = 全部通过。
# ============================================================================
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
HOOK="$REPO/hooks/customize03-shairport.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

FAIL=0
ok()  { printf '  ✓ %s\n' "$1"; }
bad() { printf '  ✗ %s\n' "$1"; FAIL=$((FAIL + 1)); }

# 断言行在文件里存在（且是"活动"行，即未被 // 注释）
has() { # has <描述> <文件> <正则>
    if grep -qE "$3" "$2"; then ok "$1"; else bad "$1（缺活动行 /$3/）"; fi
}
nhas() { # nhas <描述> <文件> <正则>  —— 必须**不**存在
    if grep -qE "$3" "$2"; then bad "$1（不该出现 /$3/）"; else ok "$1"; fi
}
cnt() { # cnt <描述> <文件> <键名> <期望次数>
    n="$(grep -cE "^[[:space:]]*$3[[:space:]]*=" "$2" || true)"
    if [ "$n" -eq "$4" ]; then ok "$1"; else bad "$1（$3 出现 $n 次，期望 $4）"; fi
}

# 四项发布配置的统一断言
assert_release() { # assert_release <用例名> <文件>
    has "$1: interpolation=basic"      "$2" '^[[:space:]]*interpolation[[:space:]]*=[[:space:]]*"basic"[[:space:]]*;'
    has "$1: buffer=1.0"               "$2" '^[[:space:]]*audio_backend_buffer_desired_length_in_seconds[[:space:]]*=[[:space:]]*1\.0[[:space:]]*;'
    has "$1: log_verbosity=0"          "$2" '^[[:space:]]*log_verbosity[[:space:]]*=[[:space:]]*0[[:space:]]*;'
    has "$1: statistics=no"            "$2" '^[[:space:]]*statistics[[:space:]]*=[[:space:]]*"no"[[:space:]]*;'
    nhas "$1: 没有 verbosity=3"        "$2" '^[[:space:]]*log_verbosity[[:space:]]*=[[:space:]]*3'
    nhas "$1: 没有 statistics=yes"     "$2" '^[[:space:]]*statistics[[:space:]]*=[[:space:]]*"yes"'
    nhas "$1: 没有 log_output_to=stderr" "$2" '^[[:space:]]*log_output_to[[:space:]]*=[[:space:]]*"stderr"'
    cnt  "$1: interpolation 唯一"      "$2" 'interpolation' 1
    cnt  "$1: buffer 唯一"             "$2" 'audio_backend_buffer_desired_length_in_seconds' 1
    cnt  "$1: log_verbosity 唯一"      "$2" 'log_verbosity' 1
    cnt  "$1: statistics 唯一"         "$2" 'statistics' 1
}

# ── 用例 A：配置文件不存在 → 新建 ─────────────────────────────────────────
echo "== A. 不存在 → 新建 =="
A="$T/a"; mkdir -p "$A/etc"
sh "$HOOK" "$A" > "$T/a.log" 2>&1 || { bad "hook 退出码非 0"; cat "$T/a.log"; }
grep -q '^\[hook\] shairport-sync 配置已应用$' "$T/a.log" && ok "打印了 '[hook] shairport-sync 配置已应用'" \
    || bad "没有打印约定的那行"
[ -f "$A/etc/shairport-sync.conf" ] && ok "配置已创建" || bad "配置未创建"
assert_release "A" "$A/etc/shairport-sync.conf"

# ── 用例 B：Debian 包样例（全注释）→ 就地改 ───────────────────────────────
echo "== B. Debian 样例（全注释）→ 就地改 =="
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
sh "$HOOK" "$B" > "$T/b.log" 2>&1 || { bad "hook 退出码非 0"; cat "$T/b.log"; }
assert_release "B" "$B/etc/shairport-sync.conf"
# 别的段不能被误改
grep -q 'output_device = "default"' "$B/etc/shairport-sync.conf" && ok "B: alsa 段未被误改" || bad "B: alsa 段被改动了"
[ "$B_BEFORE" != "$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)" ] && ok "B: 文件确实变了" || bad "B: 文件没变（hook 没生效）"

# ── 用例 C：排查期脏配置 → 纠回发布值 ─────────────────────────────────────
echo "== C. 排查期脏配置 → 纠回发布值 =="
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
sh "$HOOK" "$C" > "$T/c.log" 2>&1 || { bad "hook 退出码非 0"; cat "$T/c.log"; }
assert_release "C" "$C/etc/shairport-sync.conf"

# ── 用例 D：幂等（同一份文件连跑两次结果相同）─────────────────────────────
echo "== D. 幂等 =="
sh "$HOOK" "$B" > /dev/null 2>&1
B_RUN2="$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)"
sh "$HOOK" "$B" > /dev/null 2>&1
B_RUN3="$(md5sum "$B/etc/shairport-sync.conf" | cut -d' ' -f1)"
[ "$B_RUN2" = "$B_RUN3" ] && ok "D: 第二次与第三次结果逐字节相同" || bad "D: 不幂等（$B_RUN2 vs $B_RUN3）"
assert_release "D" "$B/etc/shairport-sync.conf"

echo
if [ "$FAIL" -eq 0 ]; then echo "== 全部通过 =="; else echo "== $FAIL 项失败 =="; fi
exit "$FAIL"
