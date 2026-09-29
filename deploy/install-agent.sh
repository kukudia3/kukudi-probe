#!/bin/sh
# 极简 VPS 探针 —— Agent 安装 / 升级脚本（Linux）
#
# 用法（在被监控的 VPS 上以 root 执行）：
#   sh install-agent.sh --server https://monitor.example.com --token pba_xxx
#   sh install-agent.sh --uninstall
#
# 幂等：重复执行只会覆盖二进制与单元、重启服务；不会重复写 Token（除非显式给 --token）。
#
# 安全要点：
#   - Token 只写进 /etc/probe-agent/token（0600），**不进命令行**（否则会出现在 ps 里）；
#   - Agent 不需要 root、不需要任何 capability，只出站连接服务端；
#   - 远端地址必须是 https（脚本会检查，除非是 127.0.0.1/localhost 的自测场景）。

set -eu

BIN_NAME="probe-agent"
SERVICE_NAME="probe-agent"
INSTALL_PATH="/usr/local/bin/${BIN_NAME}"
CONF_DIR="/etc/probe-agent"
TOKEN_FILE="${CONF_DIR}/token"
STATE_DIR="/var/lib/probe-agent"
USER_NAME="probe-agent"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
SERVER=""
TOKEN=""
INTERVAL="1"

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

[ "$(id -u)" = "0" ] || die "请用 root 执行（sudo sh $0）"

UNINSTALL=0
PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --interval) INTERVAL="${2:-}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) UNINSTALL=1; PURGE=1; shift ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

if [ "$UNINSTALL" = "1" ]; then
  info "停止并禁用服务"
  systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
  systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
  rm -f "${UNIT_PATH}"
  systemctl daemon-reload
  rm -f "${INSTALL_PATH}"
  if [ "$PURGE" = "1" ]; then
    rm -rf "${CONF_DIR}" "${STATE_DIR}"
    userdel "${USER_NAME}" 2>/dev/null || true
    echo "已彻底卸载（含 Token 与流量状态）"
  else
    echo "已卸载，Token 与流量状态保留在 ${CONF_DIR} / ${STATE_DIR}"
  fi
  exit 0
fi

command -v systemctl >/dev/null 2>&1 || die "没有 systemd，请手动运行 ${BIN_NAME}"

# ---------------------------------------------------------------- 参数校验

if [ -z "${SERVER}" ] && [ ! -f "${TOKEN_FILE}" ]; then
  die "首次安装必须提供 --server（例如 https://monitor.example.com）"
fi

# 升级场景：没给 --server 就沿用上一次单元里写的地址（避免把配置改丢）。
if [ -z "${SERVER}" ] && [ -f "${UNIT_PATH}" ]; then
  SERVER="$(sed -n 's/.*--server \([^ ]*\).*/\1/p' "${UNIT_PATH}" | head -n 1)"
  [ -n "${SERVER}" ] && info "沿用已安装的服务端地址：${SERVER}"
fi
[ -n "${SERVER}" ] || die "无法确定服务端地址，请显式提供 --server"

if [ -n "${SERVER}" ]; then
  case "${SERVER}" in
    https://*) ;;
    http://127.0.0.1*|http://localhost*) ;; # 自测允许明文回环
    http://*) die "远端必须用 https（明文会让链路上的人看到节点信息）" ;;
    *) die "--server 要以 https:// 开头（当前：${SERVER}）" ;;
  esac
fi

if [ -n "${TOKEN}" ]; then
  case "${TOKEN}" in
    pba_*) ;;
    *) die "Token 形如 pba_xxx，请检查是否复制完整" ;;
  esac
fi

# 认架构：ARM 小鸡拿到的是 -linux-arm64。
HOST_ARCH=""
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
esac

SRC=""
for cand in "./${BIN_NAME}-linux-${HOST_ARCH}" "./${BIN_NAME}-linux-amd64" "./${BIN_NAME}"; do
  [ -n "${HOST_ARCH}" ] || case "${cand}" in *"-linux-amd64") continue ;; esac
  if [ -f "${cand}" ]; then
    SRC="${cand}"
    break
  fi
done
[ -n "${SRC}" ] || die "当前目录没有 ${BIN_NAME}-linux-${HOST_ARCH:-amd64}（请先下载或自行编译；见 docs/DEPLOY.md）"

# ---------------------------------------------------------------- 安装

info "创建用户 ${USER_NAME}（不存在时）"
if ! id "${USER_NAME}" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "${USER_NAME}"
fi

info "安装二进制到 ${INSTALL_PATH}"
install -m 0755 -o root -g root "${SRC}" "${INSTALL_PATH}"

info "写入 Token 到 ${TOKEN_FILE}（0600）"
mkdir -p "${CONF_DIR}"
chmod 0750 "${CONF_DIR}"
if [ -n "${TOKEN}" ]; then
  printf '%s' "${TOKEN}" > "${TOKEN_FILE}"
fi
chown -R "${USER_NAME}:${USER_NAME}" "${CONF_DIR}"
chmod 0600 "${TOKEN_FILE}"

info "准备状态目录 ${STATE_DIR}（流量 checkpoint）"
mkdir -p "${STATE_DIR}"
chown -R "${USER_NAME}:${USER_NAME}" "${STATE_DIR}"
chmod 0750 "${STATE_DIR}"

# Agent 只出站；不给任何 capability，也不允许提权。
info "写入 systemd 单元"
cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=极简 VPS 探针 Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${USER_NAME}
Group=${USER_NAME}
StateDirectory=probe-agent
WorkingDirectory=/var/lib/probe-agent
ExecStart=${INSTALL_PATH} --server ${SERVER} --token-file ${TOKEN_FILE} --interval ${INTERVAL} --state-dir /var/lib/probe-agent

Restart=always
RestartSec=5s
TimeoutStopSec=15s
KillSignal=SIGTERM

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
ReadWritePaths=/var/lib/probe-agent

[Install]
WantedBy=multi-user.target
EOF

info "启动服务"
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}" >/dev/null
systemctl restart "${SERVICE_NAME}"
sleep 2

if systemctl is-active --quiet "${SERVICE_NAME}"; then
  echo
  echo "安装完成。"
  echo "  状态：systemctl status ${SERVICE_NAME}"
  echo "  日志：journalctl -u ${SERVICE_NAME} -f"
  echo "  自检：${INSTALL_PATH} --print-json --root /   # 只打印一次采集结果，不连服务端"
else
  die "服务没有起来，请查看：journalctl -u ${SERVICE_NAME} -n 50"
fi
