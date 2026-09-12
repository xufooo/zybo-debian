#!/bin/sh
# ============================================================================
# customize02-app.sh — 把 ZYBO Audio 后端 + WebUI 装进 rootfs
# ============================================================================
# 产物由 audio_player 的 tools/stage_release.sh 摆进本仓库的 files/。
#
# ⚠️ 这个 hook **刻意不调用 chroot**（只用 install/cp/ln），因此可以在宿主机上
#    拿一个临时目录当 $TARGET 直接干跑验证：
#       TARGET=/tmp/fakeroot ZYBO_FILES=$PWD/files sh hooks/customize02-app.sh
#    这样"镜像里到底装了什么"是可测的，而不是只能靠 CI 跑完才知道。
#
# 装出来的东西：
#   /usr/local/bin/zybo-audio-web                   后端二进制（静态 ARM）
#   /var/www/zybo-audio/index.html + assets/        WebUI
#   /usr/lib/systemd/system/zybo-audio-web.service 服务单元（启用）
#   /usr/share/doc/zybo-audio/                      版本 + 第三方许可（合规要求）
# ============================================================================
set -e

TARGET="${1:?usage: customize02-app.sh <rootfs dir>}"
FILES="${ZYBO_FILES:-}"

if [ -z "$FILES" ] || [ ! -f "$FILES/zybo-audio-web" ]; then
    echo "[hook] WARN: ZYBO_FILES/zybo-audio-web 未提供 —— 镜像里不会有网页面板"
    echo "[hook]       生成方法：cd audio_player && ./tools/stage_release.sh"
    exit 0
fi

VER="$(cat "$FILES/VERSION" 2>/dev/null || echo unknown)"
echo "[hook] 安装 ZYBO Audio 面板 v$VER"

install -D -m 755 "$FILES/zybo-audio-web"            "$TARGET/usr/local/bin/zybo-audio-web"
install -D -m 644 "$FILES/webui/index.html"          "$TARGET/var/www/zybo-audio/index.html"
if [ -f "$FILES/webui/favicon.svg" ]; then
    install -D -m 644 "$FILES/webui/favicon.svg"     "$TARGET/var/www/zybo-audio/assets/favicon.svg"
fi

# 单元放 /usr/lib/systemd/system（发行版位置）；本机覆盖走 /etc/systemd/system
install -D -m 644 "$FILES/zybo-audio-web.service"    "$TARGET/usr/lib/systemd/system/zybo-audio-web.service"

# 启用（等价 systemctl enable，不 chroot 也能做：就是建个软链）
WANT="$TARGET/etc/systemd/system/multi-user.target.wants"
mkdir -p "$WANT"
ln -sf "/usr/lib/systemd/system/zybo-audio-web.service" "$WANT/zybo-audio-web.service"
echo "[hook] enabled zybo-audio-web.service"

# ── 合规：版本 + 第三方许可随镜像分发 ──────────────────────────────────
DOC="$TARGET/usr/share/doc/zybo-audio"
install -D -m 644 "$FILES/VERSION"                   "$DOC/VERSION"
if [ -f "$FILES/THIRD-PARTY.md" ]; then
    install -D -m 644 "$FILES/THIRD-PARTY.md"        "$DOC/THIRD-PARTY.md"
fi
if [ -d "$FILES/licenses" ]; then
    install -d "$DOC/licenses"
    cp -a "$FILES/licenses/." "$DOC/licenses/"
    echo "[hook] 已随镜像放置第三方许可原文（$(ls "$FILES/licenses" | wc -l) 份）"
fi

echo "[hook] 完成：zybo-audio-web v$VER + WebUI + 许可"
