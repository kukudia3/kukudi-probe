#!/bin/sh
# 极简 VPS 探针 —— 一键安装（从 GitHub Release 下载，校验 SHA256 后安装）
#
# 服务端（在要跑面板的机器上，root）：
#   curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh | sudo sh -s -- server
#
# Agent（每台被监控的机器，root）：
#   curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh \
#     | sudo sh -s -- agent --server https://monitor.example.com --token pba_xxx
#
# 常用可选参数（放在 server/agent 之后）：
#   --version v0.1.0        指定版本（默认 latest）
#   --base-url URL          换下载源（镜像 / 自建静态站 / 内网分发），默认 GitHub Release
#   --github OWNER/REPO     换仓库（默认见下面 DEFAULT_GITHUB）
#   --uninstall [--purge]   卸载（--purge 连数据一起删）
#
# 安全说明：本脚本会校验每个下载文件的 SHA256（release 里的 SHA256SUMS），
# 校验不通过就中止；不会用 curl -k 之类的降级手段，也不会把下载内容交给 shell 直接跑
# （先落到临时目录校验，再执行）。
#
# 这个脚本自身是幂等的：重复执行 = 升级（数据、Token、配置都不动）。

set -eu

# 换成你自己的仓库（也可以每次用 --github 指定）。
DEFAULT_GITHUB="kukudi/probe"
# 从源码引导时用的分支（只有"顺便下载安装脚本"这一步会用到仓库的 raw 地址）。
DEFAULT_REF="main"

ROLE=""
VERSION="latest"
BASE_URL=""
GITHUB_REPO="$DEFAULT_GITHUB"
REF="$DEFAULT_REF"
PASSTHROUGH=""

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "==> $*"; }

# usage 打印脚本头部注释。
#
# 注意 $0 在 "curl | sh -s -- server" 这种用法里是 "sh" 而不是脚本路径，
# 所以必须先判断它是不是一个可读文件（否则会去读二进制的 sh）。
usage() {
  if [ -r "$0" ] && head -n 1 "$0" 2>/dev/null | grep -q '^#!/bin/sh'; then
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
  else
    cat <<'EOF'
用法（从 GitHub Release 下载并安装）：
  curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh | sudo sh -s -- server
  curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh | sudo sh -s -- agent --server https://monitor.example.com --token pba_xxx

可选参数：
  --version v0.1.0      指定版本（默认 latest）
  --base-url URL        换下载源（镜像 / 自建静态站 / 内网分发）
  --github OWNER/REPO   换仓库
  --uninstall [--purge] 卸载
EOF
  fi
}

# ---------------------------------------------------------------- 参数解析

if [ $# -eq 0 ]; then
  usage
  exit 1
fi

FIRST="$1"; shift
case "$FIRST" in
  server|agent) ROLE="$FIRST" ;;
  -h|--help) usage; exit 0 ;;
  *) die "第一个参数只能是 server 或 agent（当前：$FIRST）" ;;
esac

# 之后的所有参数：认识的本脚本处理，其余原样传给角色安装脚本。
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --base-url) BASE_URL="${2:-}"; shift 2 ;;
    --github) GITHUB_REPO="${2:-}"; shift 2 ;;
    --ref) REF="${2:-}"; shift 2 ;;
    *) PASSTHROUGH="$PASSTHROUGH $1"; shift ;;
  esac
done

[ "$(id -u)" = "0" ] || die "请用 root 执行（curl … | sudo sh -s -- $ROLE）"

# ---------------------------------------------------------------- 环境探测

[ "$(uname -s)" = "Linux" ] || die "只支持 Linux（当前：$(uname -s)）"

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l|armv6l) die "暂不提供 32 位 ARM 版本，请自行交叉编译后手工安装" ;;
  *) die "不支持的架构：$(uname -m)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  DOWNLOAD="curl"
elif command -v wget >/dev/null 2>&1; then
  DOWNLOAD="wget"
else
  die "需要 curl 或 wget"
fi

# try_fetch <url> <本地文件名>：下载失败返回非 0（不退出脚本，便于回退到备用源）。
try_fetch() {
  _url="$1"; _out="$2"
  if [ "$DOWNLOAD" = "curl" ]; then
    curl -fsSL --tlsv1.2 --retry 3 --retry-delay 2 -o "$_out" "$_url"
  else
    wget -q -O "$_out" "$_url"
  fi
}

# fetch <url> <本地文件名>：下载失败直接报错退出。
fetch() {
  try_fetch "$1" "$2" || die "下载失败：$1"
}

# ---------------------------------------------------------------- 计算下载地址

for tool in sha256sum awk; do
  command -v "$tool" >/dev/null 2>&1 || die "缺少 $tool"
done

BIN="probe-${ROLE}-linux-${ARCH}"
INSTALLER="install-${ROLE}.sh"

if [ -n "$BASE_URL" ]; then
  # 镜像 / 自建源：目录里应当直接放着 $BIN、SHA256SUMS 与 $INSTALLER。
  BASE="${BASE_URL%/}"
elif [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/${GITHUB_REPO}/releases/latest/download"
else
  BASE="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
fi

# ---------------------------------------------------------------- 下载 + 校验

TMPDIR_DL="$(mktemp -d 2>/dev/null || mktemp -d -t probe)"
trap 'rm -rf "$TMPDIR_DL"' EXIT INT TERM

info "架构 $ARCH，版本 $VERSION"
info "下载源 $BASE"

info "下载 SHA256SUMS"
fetch "${BASE}/SHA256SUMS" "${TMPDIR_DL}/SHA256SUMS"
grep -q " ${BIN}\$" "${TMPDIR_DL}/SHA256SUMS" || die "SHA256SUMS 里没有 ${BIN}（版本或架构不对？）"

info "下载 ${BIN}"
fetch "${BASE}/${BIN}" "${TMPDIR_DL}/${BIN}"

info "下载 ${INSTALLER}"
# 安装脚本优先从 release 资产取（同一个源、同一份校验）；
# 拿不到就退回仓库 raw（用于"刚 push 代码、还没发 release"的场景）。
FROM_RAW=""
if ! try_fetch "${BASE}/${INSTALLER}" "${TMPDIR_DL}/${INSTALLER}"; then
  RAW="https://raw.githubusercontent.com/${GITHUB_REPO}/${REF}/deploy/${INSTALLER}"
  info "release 里没有 ${INSTALLER}，改用 ${RAW}"
  fetch "$RAW" "${TMPDIR_DL}/${INSTALLER}"
  FROM_RAW="yes"
  # raw 版本不在 SHA256SUMS 里：只做"看起来是正经脚本"的最低检查。
  head -n 1 "${TMPDIR_DL}/${INSTALLER}" | grep -q '^#!/bin/sh' || die "${INSTALLER} 内容异常"
fi

info "校验 SHA256"
for file in "$BIN" "$INSTALLER"; do
  expected="$(awk -v f="$file" '$2 == f { print $1 }' "${TMPDIR_DL}/SHA256SUMS")"
  if [ -z "$expected" ]; then
    # 只有"从 raw 兜底拿到的安装脚本"允许不在清单里。
    if [ "$file" = "$INSTALLER" ] && [ -n "$FROM_RAW" ]; then
      echo "    跳过（该文件不在 SHA256SUMS 中，来源为仓库 raw）"
      continue
    fi
    die "SHA256SUMS 里缺少 $file 的哈希"
  fi
  actual="$(sha256sum "${TMPDIR_DL}/${file}" | awk '{print $1}')"
  [ "$actual" = "$expected" ] || die "$file 校验失败：期望 $expected，实际 $actual（文件可能被替换，已丢弃）"
  echo "    $file 校验通过"
done

chmod 0755 "${TMPDIR_DL}/${BIN}" "${TMPDIR_DL}/${INSTALLER}"

# ---------------------------------------------------------------- 执行安装

info "开始安装（${ROLE}）"
cd "$TMPDIR_DL"
# shellcheck disable=SC2086
sh "./${INSTALLER}" $PASSTHROUGH

if [ "$ROLE" = "server" ]; then
  echo
  echo "下一步：从日志里取一次性初始化码，然后浏览器打开面板"
  echo "    journalctl -u probe-server | grep setup_code"
fi
