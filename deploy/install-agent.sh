#!/bin/sh
# 极简 VPS 探针 —— Agent 安装 / 升级脚本（Linux）
#
# 用法（在被监控的 VPS 上以 root 执行）：
#   umask 077 && cat > /root/probe-token      # 粘贴 Token 后按 Ctrl-D（文件即 0600，装完可以删）
#   sh install-agent.sh --server https://monitor.example.com --from-file /root/probe-token
#   PROBE_TOKEN=pba_xxx sh install-agent.sh --server https://monitor.example.com
#   sh install-agent.sh --server https://monitor.example.com --token pba_xxx   # 旧的写法仍可用但不推荐：会进 ps / shell 历史 / sudo 审计日志
#   sh install-agent.sh --uninstall
#
# 幂等：重复执行只会覆盖二进制与单元、重启服务；不会重复写 Token（除非显式给 --token）。
#
# 安全要点：
#   - Token 只写进 /etc/probe-agent/token（0600），**不进 systemd 的 ExecStart**
#     （否则同机任何用户 `ps` 或读 /proc/*/cmdline 就能拿走它）；
#   - Token 也不进命令行：面板给的一键命令走的是 `--from-file /root/probe-token`，
#     Token 由你自己粘进那个文件（PROBE_TOKEN 环境变量也可以；见 docs/DEPLOY.md）；
#   - 服务以非 root 的 probe-agent 用户运行，只额外持有 CAP_NET_RAW 一个能力：
#     ICMP 探测要开原始套接字（ip4:icmp / ip6:ipv6-icmp），没有它就只能用 TCP 探测；
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
TOKEN_SRC=""
# 注意：probe-agent 的 -interval 是 Go 的 duration，必须带单位。
# 写裸数字会让单元每次启动都失败：invalid value "1" for flag -interval: parse error
INTERVAL="1s"

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

[ "$(id -u)" = "0" ] || die "请用 root 执行（sudo sh $0）"

UNINSTALL=0
PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    # 从文件读 Token：命令行里只出现路径，Token 本身不进 argv、也不会长进 shell 历史。
    --from-file) TOKEN_SRC="${2:-}"; shift 2 ;;
    --interval) INTERVAL="${2:-}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) UNINSTALL=1; PURGE=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

# 没给 --token 时允许用环境变量（PROBE_TOKEN）或 --from-file 提供。
# 为什么要有这条路：面板给的一键命令走的就是 --from-file /root/probe-token，
# Token 由用户自己粘进那个文件 —— 命令行里只有路径，不进 argv、也不进 shell 历史。
if [ -z "${TOKEN}" ]; then
  if [ -n "${TOKEN_SRC}" ]; then
    [ -f "${TOKEN_SRC}" ] || die "--from-file 指向的文件不存在：${TOKEN_SRC}"
    TOKEN="$(tr -d ' \t\r\n' < "${TOKEN_SRC}")"
  elif [ -n "${PROBE_TOKEN:-}" ]; then
    TOKEN="${PROBE_TOKEN}"
  fi
fi

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

# 容忍 `--interval 5` 这种裸数字写法：-interval 是 duration，这里补上单位。
# 不然用户手抄一条带裸数字的命令，装完就是一个起不来的服务。
case "${INTERVAL}" in
  ''|*[!0-9]*) ;;                    # 空或已经带单位：原样
  *) INTERVAL="${INTERVAL}s" ;;      # 纯数字：补 s
esac

# --server / --interval 会被原样拼进 root 拥有的 systemd 单元。systemd 按**行**解析
# 单元文件 ⇒ 值里带换行就能插入任意指令；Exec 行还会按空白切词、认引号分组、展开
# $VAR ⇒ 值里出现空白/引号/反引号/反斜杠/$( 等就能改动这一行的 argv（例如
# --server "https://ok --insecure-skip-verify"）。所以两个值都按各自的字符集白名单
# 收窄：换行/制表/引号/非 ASCII 一律落在白名单之外。（配置文件注入，不是 shell 注入。）
case "${INTERVAL}" in
  *[!0-9a-zA-Z.]*) die "--interval 只能是 Go duration（如 30s、1m30s、500ms）" ;;
esac

if [ -z "${SERVER}" ] && [ ! -f "${TOKEN_FILE}" ]; then
  die "首次安装必须提供 --server（例如 https://monitor.example.com）"
fi

# 升级场景：没给 --server 就沿用上一次单元里写的地址（避免把配置改丢）。
if [ -z "${SERVER}" ] && [ -f "${UNIT_PATH}" ]; then
  SERVER="$(sed -n 's/.*--server \([^ ]*\).*/\1/p' "${UNIT_PATH}" | head -n 1)"
  [ -n "${SERVER}" ] && info "沿用已安装的服务端地址：${SERVER}"
fi
[ -n "${SERVER}" ] || die "无法确定服务端地址，请显式提供 --server"

# URL 字符集白名单（放在"沿用单元里的地址"之后，两条来源都过一遍）。
# 保守集合：字母数字 + :/?#@!+,;=%._~&- ；不接受空白、引号、反引号、反斜杠、
# 美元符号、括号、方括号等（systemd 会解释它们）。
#
# 注意这里用 `wc -c` 数"剩下几个字节"，而不是 `[ -n "$(...)" ]`：命令替换会**吃掉
# 结尾的换行**，像 `--server $'https://x\nExecStartPre=...'` 这种值在 `-n` 判断下
# 会变成空串、被误判成合法，然后换行照样进单元 ⇒ 等于没校验。
leftover="$(printf '%s' "${SERVER}" | tr -d 'A-Za-z0-9:/?#@!+,;=%._~&-' | wc -c | tr -d ' ')"
[ "${leftover}" = "0" ] || die "--server 只能是普通 URL：不接受空白 / 引号 / 反引号 / 反斜杠 / 美元符号等字符（IPv6 字面量 [::1] 请改用域名），当前值请检查一遍"

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
  # amd64 候选只在"本机确实是 amd64"时才用：arm64 机器上装 amd64 会一直
  # "Exec format error"（Agent 还是 Restart=always，反复重启刷日志）。
  case "${cand}" in
    *"-linux-amd64") [ "${HOST_ARCH}" = "amd64" ] || continue ;;
  esac
  if [ -f "${cand}" ]; then
    SRC="${cand}"
    break
  fi
done
[ -n "${SRC}" ] || die "当前目录没有 ${BIN_NAME}-linux-${HOST_ARCH:-amd64}（本机 $(uname -m)；请先下载或自行编译，见 docs/DEPLOY.md）。若你手上的文件叫 -linux-amd64 而本机是 arm64，请按本架构重新取一份并改名为 ${BIN_NAME}-linux-arm64"

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

# Agent 只出站；除了 ICMP 需要的 CAP_NET_RAW，不给任何其它 capability，也不允许提权。
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
# StateDirectory= 的默认模式是 0755，而 systemd **每次启动**都会把目录 chmod 回该值
# （已存在的目录也一样，-EEXIST 不豁免）⇒ 不显式写模式，安装脚本设的 0750 只在
# systemd 第一次拉起之前成立。兼容性：StateDirectory= 与 StateDirectoryMode=
# 都是 systemd v235 引入的（v234 里两者都不存在），不会抬高最低版本。
StateDirectory=probe-agent
StateDirectoryMode=0750
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
# 这里**不能**加 ProcSubset=pid：它会把 /proc/stat、/proc/meminfo、/proc/cpuinfo、
# /proc/uptime、/proc/net/dev、/proc/mounts 这些系统级文件一并藏掉（内核
# subset=pid 的语义）。采集器正是读这些文件，读不到就一条指标都上报不了，
# 而 hello 仍然发得出去（Info() 容忍 /proc 失败）—— 表现是节点"在线"、
# 「系统」有值，但 CPU/内存/磁盘/网络全是空的、图表全"暂无数据"。
# systemd 文档也写明 ProcSubset=pid"不适合多数非平凡程序"。
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
# 为什么这里从"零能力"放宽成 CAP_NET_RAW：
#   ICMP 探测（设置里 type=icmp 的目标，IPv4 的 ip4:icmp 与 IPv6 的
#   ip6:ipv6-icmp）必须开原始套接字，而普通用户默认没有这个权限。不放开的后果
#   是：Agent 只会记一条警告、把该目标留空，图上那条曲线永远是空的 —— 用户怎么
#   查都查不出原因（日志里只有一行"ICMP 探测不可用"）。
# 边界（放宽的只有这一条，其余一律不动）：
#   - 仍然以非 root 的 probe-agent 用户运行，NoNewPrivileges=yes 依然生效；
#   - 只放开 CAP_NET_RAW：不给 CAP_NET_ADMIN（改路由/防火墙）、不给 CAP_SYS_*；
#   - 权限只在进程启动时授予（AmbientCapabilities），进程内无法再提权。
# 不需要 ICMP 的话（只用 type=tcp 的目标），把这两行改回空值即可，
# Agent 的 TCP 探测与全部其它功能都不受影响。
CapabilityBoundingSet=CAP_NET_RAW
AmbientCapabilities=CAP_NET_RAW
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
