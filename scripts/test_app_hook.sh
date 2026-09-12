#!/bin/sh
# ============================================================================
# test_app_hook.sh — 不跑 mmdebstrap，直接干跑 app hook 并断言装出来的树
# ============================================================================
# customize02-app.sh 刻意不 chroot，所以可以拿临时目录当 rootfs 验证。
# 用法：./scripts/test_app_hook.sh
# 退出码 0 = 全部通过。
# ============================================================================
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

FAIL=0
ok() { printf '  ✓ %s\n' "$1"; }
bad() { printf '  ✗ %s\n' "$1"; FAIL=$((FAIL + 1)); }
chk() { [ -e "$2" ] && ok "$1" || bad "$1（缺 $2）"; }
# 软链要用 -L：目标是指向镜像内的绝对路径，宿主上 -e 会失败
chklink() { [ -L "$2" ] && ok "$1" || bad "$1（缺软链 $2）"; }

echo "== 干跑 hooks/customize02-app.sh → $T =="
ZYBO_FILES="$REPO/files" sh "$REPO/hooks/customize02-app.sh" "$T"

echo "== 断言 =="
chk "后端二进制"            "$T/usr/local/bin/zybo-audio-web"
chk "WebUI"                 "$T/var/www/zybo-audio/index.html"
chk "favicon"               "$T/var/www/zybo-audio/assets/favicon.svg"
chk "systemd 单元"          "$T/usr/lib/systemd/system/zybo-audio-web.service"
chklink "单元已启用（软链）" "$T/etc/systemd/system/multi-user.target.wants/zybo-audio-web.service"
chk "版本号随镜像"          "$T/usr/share/doc/zybo-audio/VERSION"
chk "第三方许可清单"        "$T/usr/share/doc/zybo-audio/THIRD-PARTY.md"

# 二进制必须是能跑的 ARM 静态可执行（板子是 armv7）
if file "$T/usr/local/bin/zybo-audio-web" 2>/dev/null | grep -q "ARM, EABI5.*statically linked"; then
    ok "二进制是静态 ARM"
else
    bad "二进制不是静态 ARM"
fi

# 单元里的 ExecStart 必须指向真实装进去的路径
EXEC="$(grep -m1 '^ExecStart=' "$T/usr/lib/systemd/system/zybo-audio-web.service" | cut -d= -f2)"
if [ -x "$T$EXEC" ]; then ok "ExecStart 指向已安装的二进制（$EXEC）"; else bad "ExecStart $EXEC 不存在"; fi

# 启用的软链必须指向真实单元
LINK="$(readlink "$T/etc/systemd/system/multi-user.target.wants/zybo-audio-web.service")"
if [ "$LINK" = "/usr/lib/systemd/system/zybo-audio-web.service" ]; then
    ok "enable 软链目标正确"
else
    bad "enable 软链目标异常：$LINK"
fi

# 许可原文（合规要求）：至少要有 Digilent MIT 与 Go BSD-3
for f in Digilent-MIT.txt Go-BSD-3-Clause.txt gorilla-websocket-BSD-3-Clause.txt; do
    chk "许可原文 $f" "$T/usr/share/doc/zybo-audio/licenses/$f"
done

if [ "$FAIL" -eq 0 ]; then echo "== 全部通过 =="; else echo "== $FAIL 项失败 =="; fi
exit "$FAIL"
