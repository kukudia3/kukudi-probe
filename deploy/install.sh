#!/bin/sh
# 极简 VPS 探针 —— 一步式安装（下载 + 校验 + 调用安装脚本）
#
# 用法（服务端）：
#   sh install.sh server --url https://example.com/probe/probe-server-linux-amd64 \
#                        --sha256 <期望的哈希>
# 用法（Agent）：Token 不进命令行，先写进一个只有 root 能读的文件：
#   umask 077 && cat > /root/probe-token      # 粘贴 Token 后按 Ctrl-D（文件即 0600，装完可以删）
#   sh install.sh agent --url https://example.com/probe/probe-agent-linux-amd64 \
#                       --sha256 <期望的哈希> --server https://monitor.example.com \
#                       --from-file /root/probe-token
#   （旧的 --token pba_xxx 仍可用但不推荐：Token 会进 ps / shell 历史 / sudo 审计日志）
#
# 为什么走 --url 时必须给 --sha256：从网络下载可执行文件是整条链路里最危险的一步，
# 校验哈希能把"下载被替换"变成"安装失败"。哈希由构建方在 SHA256SUMS 里给出。
# 确实要跳过（自建内网、临时验证）得显式加 --allow-no-hash，脚本会再警告一次。
#
# 也支持本地文件：--file ./probe-server-linux-amd64（本地文件不经过网络，
# 你自己就是那条信任链，所以只警告、不强制哈希）。
#
# 角色脚本（install-server.sh / install-agent.sh）只从"脚本自己所在的目录"或
# "当前目录"取，并且这两个目录只要对同组/其他用户可写就拒绝执行 —— 否则在 /tmp
# 这类目录里，别人预置一个同名脚本就能借 root 的手执行任意内容。
#
# 暂存文件名是可预测的（probe-server-linux-amd64），而 cp 与 curl -o 会跟随符号
# 链接 ⇒ 下载/拷贝一律落到 mktemp 新建的文件上，再用 mv 就位（rename 替换的是链接
# 本身，不会写到它指向的地方）。

set -eu

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

MODE="${1:-}"
[ -n "${MODE}" ] || die "用法: sh install.sh server|agent [参数]（-h 看帮助）"
shift || true

URL=""; FILE=""; SHA=""; SERVER=""; TOKEN=""; TOKEN_FILE_SRC=""; INTERVAL=""; ALLOW_NO_HASH=0
while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="${2:-}"; shift 2 ;;
    --file) FILE="${2:-}"; shift 2 ;;
    --sha256) SHA="${2:-}"; shift 2 ;;
    --allow-no-hash) ALLOW_NO_HASH=1; shift ;;
    --server) SERVER="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    # Agent 的 Token 也可以从文件读（脚本本身不读，原样交给 install-agent.sh）：
    # 命令行里只出现路径，Token 不进 argv、也不会长进 shell 历史。
    --from-file) TOKEN_FILE_SRC="${2:-}"; shift 2 ;;
    --interval) INTERVAL="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,/^$/p' "$0"; exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

case "${MODE}" in
  server) BIN="probe-server" ;;
  agent) BIN="probe-agent" ;;
  *) die "第一个参数只能是 server 或 agent（当前：${MODE}）" ;;
esac

# 暂存文件名要带对架构：角色安装脚本会优先找 -linux-$ARCH，
# 在 ARM 机器上把 arm64 二进制命名成 amd64 会让人以为装错了架构。
HOST_ARCH="amd64"
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
  *) echo "警告：未知架构 $(uname -m)，按 amd64 命名（请确认二进制架构正确）" >&2 ;;
esac

# 角色脚本从哪来：脚本自己所在目录优先，其次当前目录。选定后就在那个目录里干活
# （二进制也得放在角色脚本找得到的地方：它按 ./<bin>-linux-<arch> 找）。
SCRIPT_DIR="$(pwd)"
case "$0" in
  */*) SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)" ;;
esac

ROLE_SCRIPT="install-${MODE}.sh"
WORK_DIR=""
for dir in "${SCRIPT_DIR}" "$(pwd)"; do
  [ -f "${dir}/${ROLE_SCRIPT}" ] || continue
  dir_mode="$(ls -ld "${dir}" | cut -c1-10)"
  [ -n "${dir_mode}" ] || die "无法确认 ${dir} 的权限位"
  case "${dir_mode}" in
    ?????w*|????????w*)
      die "${dir} 对同组或其他用户可写，别人可以预置一个 ${ROLE_SCRIPT} 让 root 执行。请把 install.sh 与 ${ROLE_SCRIPT} 放到只有 root 可写的目录（例如 /root）再执行" ;;
  esac
  WORK_DIR="${dir}"
  break
done
[ -n "${WORK_DIR}" ] || die "找不到 ${ROLE_SCRIPT}（它应当与 install.sh 放在同一个目录，或放在当前目录）"
cd "${WORK_DIR}"

command -v mktemp >/dev/null 2>&1 || die "需要 mktemp"
STAGE="$(mktemp "./.${BIN}-linux-${HOST_ARCH}.XXXXXX")"
trap 'rm -f "${STAGE}"' EXIT INT TERM

TARGET="./${BIN}-linux-${HOST_ARCH}"

if [ -n "${FILE}" ]; then
  info "使用本地文件 ${FILE}"
  cp "${FILE}" "${STAGE}"
elif [ -n "${URL}" ]; then
  command -v curl >/dev/null 2>&1 || die "需要 curl 才能下载（或用 --file 指定本地文件）"
  info "下载 ${URL}"
  curl -fsSL --proto '=https' --tlsv1.2 -o "${STAGE}" "${URL}" || die "下载失败"
else
  die "请提供 --url 或 --file"
fi

# 先就位再校验：角色脚本按可预测的名字找二进制，而 mv 是 rename（替换符号链接本身）。
mv -f "${STAGE}" "${TARGET}"

if [ -n "${SHA}" ]; then
  info "校验 SHA256"
  ACTUAL="$(sha256sum "${TARGET}" | awk '{print $1}')"
  [ "${ACTUAL}" = "${SHA}" ] || die "哈希不匹配：期望 ${SHA}，实际 ${ACTUAL}（文件可能被替换，已保留在 ${TARGET} 供你检查）"
  echo "    哈希一致"
elif [ -n "${FILE}" ]; then
  echo "警告：--file 走的是本地文件，不校验哈希（它没经过网络）。要核对请自己对 SHA256SUMS。"
elif [ "${ALLOW_NO_HASH}" = "1" ]; then
  echo "警告：--allow-no-hash 已指定，跳过完整性校验（不建议）。"
else
  die "走 --url 时必须提供 --sha256 <哈希>（构建方在 SHA256SUMS 里给出）；确实要跳过请显式加 --allow-no-hash"
fi

chmod 0755 "${TARGET}"

case "${MODE}" in
  server)
    info "调用 ${ROLE_SCRIPT}"
    sh "./${ROLE_SCRIPT}"
    ;;
  agent)
    [ -n "${SERVER}" ] || die "Agent 需要 --server https://monitor.example.com"
    if [ -n "${TOKEN_FILE_SRC}" ]; then
      set -- --server "${SERVER}" --from-file "${TOKEN_FILE_SRC}"
    elif [ -n "${TOKEN}" ]; then
      set -- --server "${SERVER}" --token "${TOKEN}"
    else
      die "Agent 需要 --from-file <文件> 或 --token pba_xxx（旧的写法仍可用；Token 在面板里新增节点时会给一次）"
    fi
    info "调用 ${ROLE_SCRIPT}"
    # shellcheck disable=SC2086
    sh "./${ROLE_SCRIPT}" "$@" ${INTERVAL:+--interval "${INTERVAL}"}
    ;;
esac
