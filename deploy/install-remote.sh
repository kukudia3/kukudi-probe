#!/bin/sh
# 极简 VPS 探针 —— 一键安装（从 GitHub Release 下载，校验 SHA256 后安装）
#
# 服务端（在要跑面板的机器上，root）：
#   curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh | sudo sh -s -- server
#
# Agent（每台被监控的机器，root）：Token 不进命令行，先写进一个只有 root 能读的文件
#   umask 077 && cat > /root/probe-token      # 粘贴 Token 后按 Ctrl-D（文件即 0600，装完可以删）
#   curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh \
#     | sudo sh -s -- agent --server https://monitor.example.com --from-file /root/probe-token
#   （旧的 --token pba_xxx 仍可用但不推荐：Token 会进 ps / shell 历史 / sudo 审计日志）
#
# 常用可选参数（放在 server/agent 之后）：
#   --version v0.1.0        指定版本（默认 latest）
#   --base-url URL          换下载源（镜像 / 自建静态站 / 内网分发），默认 GitHub Release；只接受 https
#   --allow-insecure-base-url   允许 --base-url 用 http（校验会失去意义，见上面的安全说明）
#   --allow-raw-installer   允许在 release 缺安装脚本时回退到仓库 raw（那条路不校验哈希）
#   --github OWNER/REPO     换仓库（默认见下面 DEFAULT_GITHUB）
#   --uninstall [--purge]   卸载（--purge 连数据一起删）
#
# 安全说明：本脚本会校验每个下载文件的 SHA256（release 里的 SHA256SUMS），
# 校验不通过就中止；不会用 curl -k 之类的降级手段，也不会把下载内容交给 shell 直接跑
# （先落到临时目录校验，再执行）。
#
# 两处"降低保证"的路径默认都关着，必须显式开关：
#   - http:// 明文下载源：SHA256SUMS 与二进制同源，中间人可以同时替换两者
#     ⇒ 要加 --allow-insecure-base-url（脚本会再警告一次）；
#   - release 里缺安装脚本时回退到仓库 raw：那条路**没有哈希校验**（只有 TLS 与
#     shebang 检查），要加 --allow-raw-installer。
#
# 这个脚本自身是幂等的：重复执行 = 升级（数据、Token、配置都不动）。

set -eu

# 换成你自己的仓库（也可以每次用 --github 指定）。
DEFAULT_GITHUB="kukudia3/kukudi-probe"
# 从源码引导时用的分支（只有"顺便下载安装脚本"这一步会用到仓库的 raw 地址）。
DEFAULT_REF="main"

ROLE=""
VERSION="latest"
BASE_URL=""
GITHUB_REPO="$DEFAULT_GITHUB"
REF="$DEFAULT_REF"
REF_SET=""
ALLOW_INSECURE_BASE_URL=""
ALLOW_RAW_INSTALLER=""
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

Agent（每台被监控的机器，root）：Token 不进命令行，先写进一个只有 root 能读的文件
  umask 077 && cat > /root/probe-token      # 粘贴 Token 后按 Ctrl-D（文件即 0600，装完可以删）
  curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh | sudo sh -s -- agent --server https://monitor.example.com --from-file /root/probe-token
  （旧的 --token pba_xxx 仍可用但不推荐：Token 会进 ps / shell 历史 / sudo 审计日志）

可选参数：
  --version v0.1.0      指定版本（默认 latest）
  --base-url URL        换下载源（镜像 / 自建静态站 / 内网分发），只接受 https
  --allow-insecure-base-url  允许 --base-url 用 http（同源校验会失去意义）
  --allow-raw-installer 允许 release 缺安装脚本时回退到仓库 raw（不校验哈希）
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
    --allow-insecure-base-url) ALLOW_INSECURE_BASE_URL="yes"; shift ;;
    --allow-raw-installer) ALLOW_RAW_INSTALLER="yes"; shift ;;
    --github) GITHUB_REPO="${2:-}"; shift 2 ;;
    --ref) REF="${2:-}"; REF_SET="yes"; shift 2 ;;
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

# BusyBox 的 wget 不认 --https-only，先探一次：认就用（多一层"不许被跳到 http"的保护），
# 不认就退回纯 -q -O —— 与老版本行为一致，不会因为一个开关把 BusyBox 用户挡在门外。
WGET_HTTPS_ONLY=""
if [ "$DOWNLOAD" = "wget" ] && wget --help 2>&1 | grep -q -- '--https-only'; then
  WGET_HTTPS_ONLY="yes"
fi

# try_fetch <url> <本地文件名>：下载失败返回非 0（不退出脚本，便于回退到备用源）。
#
# 协议限制：curl 的 --proto '=https' 既禁止 http、也禁止 https 被跳转到 http；
# wget 用 --https-only 做同样的事。只有"操作员用 --allow-insecure-base-url 显式放行的
# http 源"才放宽成 https,http（否则 http 源根本下不动）。
try_fetch() {
  _url="$1"; _out="$2"
  case "$_url" in
    https://*) _proto="https" ;;
    *) _proto="https,http" ;;
  esac
  if [ "$DOWNLOAD" = "curl" ]; then
    curl -fsSL --proto "=$_proto" --tlsv1.2 --retry 3 --retry-delay 2 -o "$_out" "$_url"
  elif [ -n "$WGET_HTTPS_ONLY" ] && [ "$_proto" = "https" ]; then
    wget -q --https-only -O "$_out" "$_url"
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
  # http:// 下 SHA256SUMS 与二进制来自同一个源：链路上任何人（或那个镜像本身）
  # 可以同时替换两者，校验"通过"却装上了别人的二进制。默认只收 https://。
  case "$BASE" in
    https://*) ;;
    http://*)
      [ -n "$ALLOW_INSECURE_BASE_URL" ] || die "--base-url 必须用 https://（当前：$BASE）。http 源里 SHA256SUMS 与二进制同源，中间人可以同时替换两者 ⇒ 哈希校验只能防住误传。确实要用 http 内网镜像，请显式加 --allow-insecure-base-url"
      echo "警告：--base-url 是 http 明文源。SHA256SUMS 与二进制同源 ⇒ 哈希校验只能防住误传（下错文件），" >&2
      echo "      防不了中间人。这条路径只适合你完全信任的内网，公网请用 https。" >&2
      ;;
    *) die "--base-url 要以 https:// 或 http:// 开头（当前：$BASE）" ;;
  esac
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
# 安装脚本优先从 release 资产取（同一个源、同一份校验）。
#
# 拿不到时**默认中止**：回退到仓库 raw 会把安装脚本的完整性从"SHA256SUMS 校验"
# 降级成"TLS + 一个 shebang 检查"，然后以 root 执行。要接受这个降级必须显式授权
# （--allow-raw-installer），并且会在下面再警告一次。
FROM_RAW=""
if ! try_fetch "${BASE}/${INSTALLER}" "${TMPDIR_DL}/${INSTALLER}"; then
  [ -n "$ALLOW_RAW_INSTALLER" ] || die "release 里没有 ${INSTALLER}（${BASE}/${INSTALLER} 下载失败）。
       仓库 raw 兜底会以 root 执行一个**未经哈希校验**的安装脚本，脚本默认不做这件事。
       要么把 ${INSTALLER} 放进 --base-url 指向的目录 / 重新发布 release，
       要么确认接受这个风险后重跑并加 --allow-raw-installer"
  # 指定了 --version（且不是走 --base-url 的镜像）时用同名 tag 取 raw：
  # 与本次安装的二进制同一代、可追溯；默认分支（main）是移动目标。
  RAW_REF="$REF"
  if [ -z "$REF_SET" ] && [ "$VERSION" != "latest" ] && [ -z "$BASE_URL" ]; then
    RAW_REF="$VERSION"
  fi
  RAW="https://raw.githubusercontent.com/${GITHUB_REPO}/${RAW_REF}/deploy/${INSTALLER}"
  echo "警告：release 里没有 ${INSTALLER}，改用仓库 raw：${RAW}" >&2
  echo "      ${INSTALLER} 不在 SHA256SUMS 里 ⇒ 本次执行的安装脚本没有哈希校验（只有 TLS 与 shebang 检查）。" >&2
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
