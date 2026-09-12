#!/bin/sh
# ============================================================================
# customize03-shairport.sh — 把 AirPlay 播放器（shairport-sync）的**发布配置**
#                            固化进 rootfs，而不是只留在某一块板子的 /etc 里
# ============================================================================
# 为什么需要这个 hook：
#   2026-09-12 在板上排查 AirPlay 卡顿，改好了 /etc/shairport-sync.conf 里的两处，
#   实测立竿见影 —— 但**只在那一块板上**。镜像重刷一次就全丢。所以把它挪进 rootfs
#   构建流程。
#
# 要固化的两项（均为 4.x 的 `general` 段设置）：
#
#   ① interpolation = "basic"
#      依据：shairport-sync 的 ±1 帧漂移补偿默认走 soxr，而 soxr_oneshot() 每次都要
#      重建滤波器。启动自检日志实测：
#          soxr_delay: 22354379 nanoseconds, soxr_delay_threshold: 30 milliseconds.
#          "soxr" interpolation has been chosen.
#      这颗 650 MHz Cortex-A9 上一次要 22.35 ms，只比 30 ms 阈值"刚好合格"就被选中；
#      一个线程实测烧到 105% CPU（纯用户态自旋），统计里 Min DAC Queue = 0（欠载）。
#      改成 "basic"（线性插值地插入/删除整帧）后：CPU 105% → 16.5%，
#      Min DAC Queue 0 → 约 0.96 s，Missing/Late/Too Late 全 0，卡顿消失。
#      ±1 帧的修正量用线性插值听不出差别（shairport-sync 自己的文档也建议慢 CPU 用
#      basic / vernier）。
#
#   ② general.audio_backend_buffer_desired_length_in_seconds = 1.0
#      依据：默认 0.2 s 在这台机器上会被 WiFi 抖动掏空（同一 SSID 多 BSSID 漫游）。
#      调到 1.0 s 后队列能稳定在 0.96 s，代价只是音量调节响应稍慢（本机接受）。
#
# 另外**顺带清理**:排查期临时打开的诊断项不能进发布镜像 ——
#   diagnostics.log_verbosity 必须是 0（排查时是 3）
#   diagnostics.statistics     必须是 "no"（排查时是 "yes"）
#   diagnostics.log_output_to  恢复默认（排查时被改成 "stderr"）
#
# 用法（mmdebstrap customize 阶段 hook，在宿主上执行，chroot 目录是 $1）：
#   sh hooks/customize03-shairport.sh <rootfs dir>
# 幂等：重复跑结果一致；配置文件存在则改、不存在则新建。
# ============================================================================
set -e

TARGET="${1:?usage: customize03-shairport.sh <rootfs dir>}"
CONF="$TARGET/etc/shairport-sync.conf"

mkdir -p "$TARGET/etc"

# ── 配置文件不存在 → 新建一份最小可用配置 ─────────────────────────────────
# （Debian 的 shairport-sync 包会带一份注释齐全的样例；这里兜底：万一上游改了
#   打包方式、或者用了别的来源，也不能让发布配置为空。）
if [ ! -f "$CONF" ]; then
    cat > "$CONF" <<'EOF'
// /etc/shairport-sync.conf — ZYBO Audio 发布配置（由 zybo-debian 的
// hooks/customize03-shairport.sh 生成；不要手工改，改 hook 后重建镜像）
// 完整可选项见 shairport-sync 自带样例或 `man shairport-sync`。

general =
{
	// 650MHz Cortex-A9：soxr 一次补帧实测 22.35ms（阈值 30ms，刚好被选中），
	// 单线程烧到 105% CPU。basic 是线性插值，几乎不耗 CPU，±1 帧修正听不出差别。
	interpolation = "basic";
	// 抗 WiFi 抖动（同 SSID 多 BSSID 漫游）；默认 0.2s 会欠载。实测稳定在 0.96s。
	audio_backend_buffer_desired_length_in_seconds = 1.0;
};

diagnostics =
{
	// 发布配置：诊断项一律关闭（排查时才临时打开）
	log_verbosity = 0;
	statistics = "no";
};
EOF
    echo "[hook] shairport-sync 配置不存在 → 已新建 $CONF"
else
# ── 已存在 → 就地改（存在则改：同段内的键替换；键缺失则在段尾补）─────────
awk '
BEGIN {
    SUBSEP = "\034"
    depth = 0; sec = ""; pend = ""

    nsec = 2; slist[1] = "general"; slist[2] = "diagnostics"

    nw["general"] = 2
    wkey["general", 1] = "interpolation"
    wval["general", 1] = "\tinterpolation = \"basic\"; // 650MHz A9 上 soxr 一次补帧 22.35ms（阈值 30ms），basic 线性插值几乎不耗 CPU；实测 CPU 105%→16.5%"
    wkey["general", 2] = "audio_backend_buffer_desired_length_in_seconds"
    wval["general", 2] = "\taudio_backend_buffer_desired_length_in_seconds = 1.0; // 抗 WiFi 抖动；默认 0.2s 会欠载，实测队列稳定在 0.96s"

    nw["diagnostics"] = 2
    wkey["diagnostics", 1] = "log_verbosity"
    wval["diagnostics", 1] = "\tlog_verbosity = 0; // 发布配置：不要排查用的 3"
    wkey["diagnostics", 2] = "statistics"
    wval["diagnostics", 2] = "\tstatistics = \"no\"; // 发布配置：不要排查用的 yes"
}

# 段头（独占一行）：name =
depth == 0 && $0 ~ /^[ \t]*[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*$/ {
    pend = $0; sub(/[ \t]*=[ \t]*$/, "", pend); sub(/^[ \t]*/, "", pend)
    print; next
}

# 段头 + 左花括号同行：name = {
$0 ~ /^[ \t]*[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*\{[ \t]*$/ {
    s = $0; sub(/[ \t]*=.*/, "", s); sub(/^[ \t]*/, "", s)
    print; sec = s; depth = 1; seen[s] = 1; next
}

# 左花括号（跟在段头之后）
depth == 0 && $0 ~ /^[ \t]*\{[ \t]*$/ {
    print; sec = pend; depth = 1; seen[sec] = 1; next
}

# 段结束：补齐该段缺失的强制键
depth == 1 && $0 ~ /^[ \t]*\}[ \t]*;?[ \t]*$/ {
    for (i = 1; i <= nw[sec]; i++) {
        k = sec SUBSEP wkey[sec, i]
        if (!(k in found)) { print wval[sec, i]; found[k] = 1 }
    }
    print; sec = ""; depth = 0; next
}

{
    if (depth == 1 && sec != "") {
        t = $0
        sub(/^[ \t]+/, "", t)
        active = 1
        if (t ~ /^\/\//) { active = 0; sub(/^\/\/[ \t]*/, "", t) }
        key = t
        sub(/[ \t]*=.*/, "", key)
        if (key ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
            for (i = 1; i <= nw[sec]; i++) {
                if (key == wkey[sec, i]) {
                    k = sec SUBSEP key
                    if (!(k in found)) { print wval[sec, i]; found[k] = 1 }
                    next
                }
            }
            # 排查用的 log_output_to = "stderr" 不能留在发布配置里（默认是 syslog）
            if (sec == "diagnostics" && key == "log_output_to" && active) {
                print "//\tlog_output_to = \"syslog\"; // 发布配置：用默认；排查时才改 \"stderr\""
                next
            }
        }
    }
    print
}

# 文件里根本没有 general / diagnostics 段 → 在文末补上
END {
    for (j = 1; j <= nsec; j++) {
        s = slist[j]
        if (!(s in seen)) {
            print ""
            print s " ="
            print "{"
            for (i = 1; i <= nw[s]; i++) print wval[s, i]
            print "};"
        }
    }
}
' "$CONF" > "$CONF.zybo-new"
    mv -f "$CONF.zybo-new" "$CONF"
fi

# ── 校验：四项必须就位，且诊断项不能是排查值 ──────────────────────────────
fail() { echo "[hook] ERROR: shairport-sync 配置校验失败：$1" >&2; exit 1; }

grep -qE '^[[:space:]]*interpolation[[:space:]]*=[[:space:]]*"basic"[[:space:]]*;' "$CONF" \
    || fail 'interpolation 不是 "basic"'
grep -qE '^[[:space:]]*audio_backend_buffer_desired_length_in_seconds[[:space:]]*=[[:space:]]*1\.0[[:space:]]*;' "$CONF" \
    || fail 'audio_backend_buffer_desired_length_in_seconds 不是 1.0'
grep -qE '^[[:space:]]*log_verbosity[[:space:]]*=[[:space:]]*0[[:space:]]*;' "$CONF" \
    || fail 'diagnostics.log_verbosity 不是 0'
grep -qE '^[[:space:]]*statistics[[:space:]]*=[[:space:]]*"no"[[:space:]]*;' "$CONF" \
    || fail 'diagnostics.statistics 不是 "no"'
grep -qE '^[[:space:]]*log_verbosity[[:space:]]*=[[:space:]]*3' "$CONF" \
    && fail '还留着排查用的 log_verbosity = 3'

# 同一段里不能出现重复键（libconfig 会直接报解析错误，shairport-sync 起不来）
for k in interpolation audio_backend_buffer_desired_length_in_seconds log_verbosity statistics; do
    n="$(grep -cE "^[[:space:]]*${k}[[:space:]]*=" "$CONF" || true)"
    [ "$n" -eq 1 ] || fail "键 $k 出现了 $n 次（必须恰好 1 次，重复键会让 shairport-sync 拒绝启动）"
done

echo "[hook] shairport-sync 配置已应用"
grep -nE '^[[:space:]]*(interpolation|audio_backend_buffer_desired_length_in_seconds|log_verbosity|statistics)[[:space:]]*=' "$CONF" | sed 's/^/  /'
