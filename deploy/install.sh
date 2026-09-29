#!/bin/sh
# 极简 VPS 探针 —— 一步式安装（下载 + 校验 + 调用安装脚本）
#
# 用法（服务端）：
#   sh install.sh server --url https://example.com/probe/probe-server-linux-amd64 \
#                        --sha256 <期望的哈希>
# 用法（Agent）：
#   sh install.sh agent --url https://example.com/probe/probe-agent-linux-amd64 \
#                       --sha256 <期望的哈希> --server https://monitor.example.com --token pba_xxx
#
# 为什么强制 --sha256：从网络下载可执行文件是整条链路里最危险的一步，
# 校验哈希能把"下载被替换"变成"安装失败"。哈希由构建方在 SHA256SUMS 里给出。
#
# 也支持本地文件：--file ./probe-server-linux-amd64

set -eu

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

MODE="${1:-}"
[ -n "${MODE}" ] || die "用法: sh install.sh server|agent [参数]（-h 看帮助）"
shift || true

URL=""; FILE=""; SHA=""; SERVER=""; TOKEN=""; INTERVAL=""
while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="${2:-}"; shift 2 ;;
    --file) FILE="${2:-}"; shift 2 ;;
    --sha256) SHA="${2:-}"; shift 2 ;;
    --server) SERVER="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --interval) INTERVAL="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
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

TARGET="./${BIN}-linux-${HOST_ARCH}"

if [ -n "${FILE}" ]; then
  info "使用本地文件 ${FILE}"
  cp "${FILE}" "${TARGET}"
elif [ -n "${URL}" ]; then
  command -v curl >/dev/null 2>&1 || die "需要 curl 才能下载（或用 --file 指定本地文件）"
  info "下载 ${URL}"
  curl -fsSL --proto '=https' --tlsv1.2 -o "${TARGET}.part" "${URL}" || die "下载失败"
  mv "${TARGET}.part" "${TARGET}"
else
  die "请提供 --url 或 --file"
fi

if [ -n "${SHA}" ]; then
  info "校验 SHA256"
  ACTUAL="$(sha256sum "${TARGET}" | awk '{print $1}')"
  [ "${ACTUAL}" = "${SHA}" ] || die "哈希不匹配：期望 ${SHA}，实际 ${ACTUAL}（文件可能被替换，已保留在 ${TARGET} 供你检查）"
  echo "    哈希一致"
else
  echo "警告：没有提供 --sha256，跳过完整性校验（不建议）"
fi

chmod 0755 "${TARGET}"

case "${MODE}" in
  server)
    info "调用 install-server.sh"
    sh ./install-server.sh
    ;;
  agent)
    [ -n "${SERVER}" ] || die "Agent 需要 --server https://monitor.example.com"
    [ -n "${TOKEN}" ] || die "Agent 需要 --token pba_xxx（在面板里新增节点时会给一次）"
    info "调用 install-agent.sh"
    # shellcheck disable=SC2086
    sh ./install-agent.sh --server "${SERVER}" --token "${TOKEN}" ${INTERVAL:+--interval "${INTERVAL}"}
    ;;
esac
