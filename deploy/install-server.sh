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
# 默认信任来自 127.0.0.1 的转发头（X-Forwarded-For / X-Real-IP）。
#
# 为什么默认开：探针只监听 127.0.0.1，唯一连得上它的是本机的反向代理
# （Caddy / nginx）或 Cloudflare 隧道（cloudflared）。不信任的话，审计日志与
# 登录限流看到的全是 127.0.0.1 —— 限流等于所有人共用一个桶，形同虚设。
# 而外部直连的请求对端不是 127.0.0.1，它们伪造的转发头照样被忽略。
#
# ⚠️ 前提：这台机器是**单租户**（面板主机上没有别的、你不信任的登录用户）。
# 反代与面板同机时服务端看到的 TCP 对端恒为 127.0.0.1，于是**本机任何用户**
# 都被当作可信来源：他们能直接连 127.0.0.1:25774 并自带 X-Forwarded-For，
# 来源 IP 就变成他们自选的地址 —— 按 IP 的登录限流可以靠"换一个假 IP 就是一个
# 新桶"绕过，审计与访问日志也会记成别人（伪造者还能绕过反代上的 ACL/TLS 直连面板）。
# 多租户主机（共享 VPS、有协作账号、跑着别人的服务）请用 --trusted-proxy ""
# 关掉转发头信任，代价是限流退化成所有人共用一个桶（见 docs/DEPLOY.md §4.3）。
#
# 换别的前端（比如反代在另一台机器）：--trusted-proxy <CIDR>；传空串则完全不信任。
TRUSTED_PROXY="127.0.0.1"

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

[ "$(id -u)" = "0" ] || die "请用 root 执行（sudo sh $0）"

# ---------------------------------------------------------------- 卸载

UNINSTALL=0
PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --uninstall) UNINSTALL=1; shift ;;
    --purge) UNINSTALL=1; PURGE=1; shift ;;
    --trusted-proxy)
      [ $# -ge 2 ] || die "--trusted-proxy 后面要跟 CIDR/IP（传空串 \"\" 表示不信任任何转发头）"
      TRUSTED_PROXY="$2"
      shift 2
      ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "未知参数: $1" ;;
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

# --trusted-proxy 是操作员传进来的值，会被原样拼进 root 拥有的 systemd 单元，而
# systemd 按**行**解析单元文件 ⇒ 值里带换行就能插入任意指令（配置文件注入，不是
# shell 注入：经 install-remote.sh 时换行会被 IFS 切词，手工执行这条路才可达）。
# 另外 Environment= 按空白切分赋值，值里带空格会让后面的 CIDR 被当成另一个（无效的）
# 赋值而静默丢掉。所以：先去掉分隔用的空白，再按字符集白名单校验 —— 白名单只有
# 数字/十六进制/点/冒号/逗号/斜杠，换行、制表、引号、非 ASCII 全部被挡在外面
# （与 Go 侧 internal/config.ParseTrustedProxies 接受的形式一致）。
TRUSTED_PROXY="$(printf '%s' "${TRUSTED_PROXY}" | tr -d ' ')"
case "${TRUSTED_PROXY}" in
  *[!0-9A-Fa-f:.,/]*) die "--trusted-proxy 只接受 IP / CIDR 列表（逗号分隔，如 10.0.0.0/8,127.0.0.1）" ;;
esac

command -v systemctl >/dev/null 2>&1 || die "没有 systemd，请手动运行 ${BIN_NAME}（或用容器）"

# 认架构：ARM 小鸡（Oracle Ampere / Hetzner CAX / 树莓派）拿到的是 -linux-arm64。
HOST_ARCH=""
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
esac

# 依次尝试：本架构的发布名 → amd64 名（只有本机确实是 amd64 时才用）→ 不带后缀。
# amd64 候选不能拿到 arm64 机器上用：装得上去、之后每次启动都是 "Exec format error"，
# 服务反复重启而安装脚本只报"服务没有起来"，排查成本极高。
SRC=""
for cand in "./${BIN_NAME}-linux-${HOST_ARCH}" "./${BIN_NAME}-linux-amd64" "./${BIN_NAME}"; do
  case "${cand}" in
    *"-linux-amd64") [ "${HOST_ARCH}" = "amd64" ] || continue ;;
  esac
  if [ -f "${cand}" ]; then
    SRC="${cand}"
    break
  fi
done
[ -n "${SRC}" ] || die "当前目录没有 ${BIN_NAME}-linux-${HOST_ARCH:-amd64}（本机 $(uname -m)；请先下载或自行编译，见 docs/DEPLOY.md）。若你手上的文件叫 -linux-amd64 而本机是 arm64，请按本架构重新取一份并改名为 ${BIN_NAME}-linux-arm64"

# 数据目录不能放在网络文件系统上：SQLite 的锁依赖本地文件系统。
if [ -d "${DATA_DIR}" ]; then
  # `stat -f -c %T` 是 GNU coreutils 专有语法；BusyBox / BSD 的 stat 会失败。
  # 失败必须显式说出来，不能 `|| echo unknown` 让这项检查看起来像"通过了"。
  FS_TYPE=""
  if ! FS_TYPE="$(stat -f -c %T "${DATA_DIR}" 2>/dev/null)"; then
    FS_TYPE=""
  fi
  if [ -z "${FS_TYPE}" ]; then
    echo "警告：认不出 ${DATA_DIR} 的文件系统类型（stat 不支持 -f -c），已跳过网络盘检查。" >&2
    echo "      如果它挂在 NFS/CIFS 上，请换成本地磁盘：SQLite 的锁依赖本地文件系统。" >&2
  else
    case "${FS_TYPE}" in
      nfs*|cifs|smb*|fuse*|overlayfs)
        die "${DATA_DIR} 位于 ${FS_TYPE}：SQLite 不能放在网络文件系统上，请改用本地磁盘" ;;
    esac
  fi
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
Documentation=https://github.com/kukudia3/kukudi-probe
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${USER_NAME}
Group=${USER_NAME}
# 可变状态（SQLite）放在 /var/lib，systemd 会保证目录存在且属主正确。
#
# StateDirectoryMode= 是必须的：StateDirectory= 的默认模式是 0755，而且 systemd
# **每次启动**都会把目录 chmod 回该值（已存在的目录也一样，-EEXIST 不豁免）
# ⇒ 不写这一行，下面安装脚本设的 0700 只在 systemd 第一次拉起之前成立，
# 目录里有 SQLite 的 -wal/-shm（最近的提交页：密码哈希、会话、审计）。
# 兼容性：StateDirectory= 与 StateDirectoryMode= 都是 systemd v235 引入的
# （v234 的 systemd.exec 里两者都不存在），本项目本来就在用 StateDirectory=，
# 所以这一行不会把最低 systemd 版本抬高。
StateDirectory=probe-server
StateDirectoryMode=0700
WorkingDirectory=/var/lib/probe-server
# 信任本机反代/隧道带来的 X-Forwarded-For（说明见脚本顶部的 TRUSTED_PROXY）。
#
# 用 Environment= 而不是把它塞进 ExecStart：有人用 drop-in 整体覆盖 ExecStart 时
# （换监听地址、加参数都会那么干），主单元里的命令行参数会被整个忽略，而
# Environment= 是独立的一条，照样生效；以后要改也只需要一行 drop-in。
Environment=PROBE_TRUSTED_PROXY=${TRUSTED_PROXY}
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
  echo "提示：服务默认只监听 ${LISTEN}（本机）。公网访问请用 Caddy/nginx 反代并启用 TLS。"
  if [ -n "${TRUSTED_PROXY}" ]; then
    echo "      已信任来自 ${TRUSTED_PROXY} 的转发头，日志与审计里会是真实访客 IP。"
    echo "      反代不在本机时：重跑本脚本并加 --trusted-proxy <那个代理的 CIDR>。"
    # 信任回环地址是"单租户"前提下的取舍：同机任何用户都在可信网段里，
    # 都能自带 X-Forwarded-For 伪造来源 IP（绕过按 IP 的登录限流、污染审计日志）。
    case "${TRUSTED_PROXY}" in
      *127.0.0.1*|*::1*)
        echo "      ⚠️ 注意：信任回环地址意味着**本机任何用户**都能伪造来源 IP（绕过按 IP 的"
        echo "         登录限流、让审计日志记成别的地址），也能绕开反代直连 ${LISTEN}。"
        echo "         多租户主机请重跑本脚本并加 --trusted-proxy \"\"（限流会退化成全局单桶，"
        echo "         详见 docs/DEPLOY.md 4.3）。"
        ;;
    esac
  else
    echo "      当前不信任任何转发头（--trusted-proxy 传了空串）。"
    echo "      如果前面挂了反代，日志与审计里会全是反代的地址，登录限流也会失效。"
  fi
else
  die "服务没有起来，请查看：journalctl -u ${SERVICE_NAME} -n 50"
fi
