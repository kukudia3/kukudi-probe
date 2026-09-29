#!/bin/sh
# 极简 VPS 探针 —— 服务端安装 / 升级脚本（Linux）
#
# 用法（在服务器上以 root 执行）：
#   sh install-server.sh              安装或升级到最新已下载的二进制
#   sh install-server.sh --uninstall  卸载（保留数据，除非再加 --purge）
#   sh install-server.sh --purge      卸载并删除数据目录与用户
#
# 它做的事很少，且每一步都可重复执行（幂等）：
#   1. 建专用非 root 用户（无登录 shell）
#   2. 放二进制到 /usr/local/bin
#   3. 建数据目录（0750）与 systemd 单元
#   4. 启动服务并打印初始化提示
#
# 二进制从哪来？脚本不会去联网下载：请先把 probe-server-linux-amd64 放到当前目录
# （或用 install.sh 一步完成"下载 + 安装"）。这样"装什么版本"永远由你决定。

set -eu

BIN_NAME="probe-server"
SERVICE_NAME="probe-server"
INSTALL_PATH="/usr/local/bin/${BIN_NAME}"
DATA_DIR="/var/lib/probe-server"
USER_NAME="probe"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
LISTEN="127.0.0.1:25774"

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

[ "$(id -u)" = "0" ] || die "请用 root 执行（sudo sh $0）"

# ---------------------------------------------------------------- 卸载

UNINSTALL=0
PURGE=0
for arg in "$@"; do
  case "$arg" in
    --uninstall) UNINSTALL=1 ;;
    --purge) UNINSTALL=1; PURGE=1 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "未知参数: $arg" ;;
  esac
done

if [ "$UNINSTALL" = "1" ]; then
  info "停止并禁用服务"
  systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
  systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
  rm -f "${UNIT_PATH}"
  systemctl daemon-reload
  info "删除二进制"
  rm -f "${INSTALL_PATH}"
  if [ "$PURGE" = "1" ]; then
    info "删除数据目录 ${DATA_DIR} 与用户 ${USER_NAME}"
    rm -rf "${DATA_DIR}"
    userdel "${USER_NAME}" 2>/dev/null || true
    echo "已彻底卸载（数据已删除）"
  else
    echo "已卸载，数据保留在 ${DATA_DIR}（要一并删除请加 --purge）"
  fi
  exit 0
fi

# ---------------------------------------------------------------- 前置检查

command -v systemctl >/dev/null 2>&1 || die "没有 systemd，请手动运行 ${BIN_NAME}（或用容器）"

# 认架构：ARM 小鸡（Oracle Ampere / Hetzner CAX / 树莓派）拿到的是 -linux-arm64。
HOST_ARCH=""
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
esac

# 依次尝试：本架构的发布名 → amd64 名（兼容老流程/手工改名）→ 不带后缀。
SRC=""
for cand in "./${BIN_NAME}-linux-${HOST_ARCH}" "./${BIN_NAME}-linux-amd64" "./${BIN_NAME}"; do
  [ -n "${HOST_ARCH}" ] || case "${cand}" in *"-linux-amd64") continue ;; esac
  if [ -f "${cand}" ]; then
    SRC="${cand}"
    break
  fi
done
[ -n "${SRC}" ] || die "当前目录没有 ${BIN_NAME}-linux-${HOST_ARCH:-amd64}（请先下载或自行编译；见 docs/DEPLOY.md）"

# 数据目录不能放在网络文件系统上：SQLite 的锁依赖本地文件系统。
if [ -d "${DATA_DIR}" ]; then
  FS_TYPE="$(stat -f -c %T "${DATA_DIR}" 2>/dev/null || echo unknown)"
  case "${FS_TYPE}" in
    nfs*|cifs|smb*|fuse*|overlayfs)
      die "${DATA_DIR} 位于 ${FS_TYPE}：SQLite 不能放在网络文件系统上，请改用本地磁盘" ;;
  esac
fi

# ---------------------------------------------------------------- 安装

info "创建用户 ${USER_NAME}（不存在时）"
if ! id "${USER_NAME}" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "${USER_NAME}"
fi

info "安装二进制到 ${INSTALL_PATH}"
install -m 0755 -o root -g root "${SRC}" "${INSTALL_PATH}"

info "准备数据目录 ${DATA_DIR}"
mkdir -p "${DATA_DIR}"
chown -R "${USER_NAME}:${USER_NAME}" "${DATA_DIR}"
# 0700：目录里有 SQLite 的 -wal/-shm，只允许服务账号进入（见 docs/SECURITY.md）。
chmod 0700 "${DATA_DIR}"

info "写入 systemd 单元"
cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=极简 VPS 探针 服务端
Documentation=https://github.com/your/probe
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${USER_NAME}
Group=${USER_NAME}
# 可变状态（SQLite）放在 /var/lib，systemd 会保证目录存在且属主正确。
StateDirectory=probe-server
WorkingDirectory=/var/lib/probe-server
ExecStart=${INSTALL_PATH} --listen ${LISTEN} --data-dir /var/lib/probe-server

Restart=on-failure
RestartSec=3s
# 优雅退出：默认 10s 内把内存里的历史落盘（见 --shutdown-grace）
TimeoutStopSec=20s
KillSignal=SIGTERM

# ---- 安全加固（能加的都加上；不需要 root、不需要任何 capability）----
NoNewPrivileges=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=
AmbientCapabilities=
UMask=0077
ReadWritePaths=/var/lib/probe-server

[Install]
WantedBy=multi-user.target
EOF

info "启动服务"
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}" >/dev/null
systemctl restart "${SERVICE_NAME}"
sleep 1

if systemctl is-active --quiet "${SERVICE_NAME}"; then
  echo
  echo "安装完成。"
  echo "  状态：  systemctl status ${SERVICE_NAME}"
  echo "  日志：  journalctl -u ${SERVICE_NAME} -f"
  echo "  初始化码在第一次启动的日志里："
  echo "      journalctl -u ${SERVICE_NAME} | grep setup_code"
  echo
  echo "提示：服务默认只监听 ${LISTEN}（本机）。公网访问请用 Caddy/nginx 反代并启用 TLS，"
  echo "      并把 --trusted-proxy 设为反代地址（这样日志与审计里才是真实访客 IP）。"
else
  die "服务没有起来，请查看：journalctl -u ${SERVICE_NAME} -n 50"
fi
