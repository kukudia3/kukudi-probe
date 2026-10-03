#!/bin/sh
# 安装自检：定位 "curl: (22) The requested URL returned error: 404" 这类安装失败。
#
# 用法：
#   sh doctor.sh                          # 默认查 kukudia3/kukudi-probe
#   sh doctor.sh OWNER/REPO               # 换仓库
#   sh doctor.sh OWNER/REPO 分支名         # 已知分支时直接指定
#
# 只读：不做任何修改、不需要 root、不需要登录（私有仓库查不出来时会明确说明）。
# 退出码：0 = 一切正常可以装；1 = 找到阻断原因（脚本会打印怎么修）。

set -eu

REPO="${1:-kukudia3/kukudi-probe}"
BRANCH="${2:-}"
ASSET="probe-server-linux-amd64"
SCRIPT="install-remote.sh"

ok()   { printf '  \033[32m[OK]\033[0m %s\n' "$*"; }
bad()  { printf '  \033[31m[!!]\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m[? ]\033[0m %s\n' "$*"; }
info() { printf '\n=== %s ===\n' "$*"; }

command -v curl >/dev/null 2>&1 || { echo "需要 curl" >&2; exit 1; }

# code <url>：只取 HTTP 状态码（跟随跳转，失败也返回码）。
code() {
  curl -sSL -o /dev/null -w '%{http_code}' --max-time 20 "$1" 2>/dev/null || echo "000"
}

# body <url>：取正文（失败返回空）。
body() {
  curl -sSL --max-time 20 "$1" 2>/dev/null || true
}

# jsonfield <json> <字段名>：从 JSON 里粗略取一个标量值（不依赖 jq）。
jsonfield() {
  printf '%s' "$1" | tr -d '\n' | grep -o "\"$2\"[[:space:]]*:[[:space:]]*[^,}]*" \
    | head -n 1 | sed 's/.*:[[:space:]]*//; s/^"//; s/"$//'
}

FAIL=0

info "0. 网络与 DNS"
for host in github.com raw.githubusercontent.com api.github.com; do
  ip="$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}' || true)"
  if [ -z "$ip" ]; then
    if command -v nslookup >/dev/null 2>&1; then
      ip="$(nslookup "$host" 2>/dev/null | awk '/^Address: /{print $2; exit}' || true)"
    fi
  fi
  if [ -n "$ip" ]; then
    ok "$host → $ip"
    case "$ip" in
      127.*|0.0.0.0|10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*)
        bad "这个地址是内网/本机地址：DNS 被污染或被代理工具改写了，安装必然失败"
        warn "修：换 DNS（1.1.1.1 / 223.5.5.5），或用 --base-url 走镜像"
        FAIL=1 ;;
    esac
  else
    warn "$host 解析不出地址（可能只是本机没有 getent/nslookup）"
  fi
done
printf '  curl 直连 github.com：HTTP %s\n' "$(code https://github.com)"

info "1. 仓库是否存在 / 是否公开"
repo_json="$(body "https://api.github.com/repos/${REPO}")"
repo_code="$(code "https://api.github.com/repos/${REPO}")"
if [ "$repo_code" = "404" ] || printf '%s' "$repo_json" | grep -q '"message":[[:space:]]*"Not Found"'; then
  bad "api.github.com 说 ${REPO} 不存在 —— 私有仓库也长这样（GitHub 用 404 隐藏私有仓库的存在）"
  warn "修：把仓库改成 Public（Settings → General → 最下面 Danger Zone → Change visibility）"
  warn "    或者不用 GitHub，改走第 5 节的 --base-url 自建源"
  FAIL=1
elif [ "$repo_code" = "000" ]; then
  bad "连不上 api.github.com（网络问题，不是仓库问题）"
  FAIL=1
else
  ok "仓库可访问（HTTP $repo_code）"
  private="$(jsonfield "$repo_json" private)"
  default_branch="$(jsonfield "$repo_json" default_branch)"
  if [ "$private" = "true" ]; then
    bad "仓库是 **Private**：raw.githubusercontent.com 不给私有仓库提供匿名下载，一定是 404"
    warn "修：改成 Public，或改走 --base-url 自建源（第 5 节）"
    FAIL=1
  else
    ok "仓库是 Public"
  fi
  [ -n "$default_branch" ] && ok "默认分支：$default_branch"
  [ -z "$BRANCH" ] && BRANCH="$default_branch"
fi

[ -n "$BRANCH" ] || BRANCH="main"

info "2. install-remote.sh 到底在哪个路径 / 哪个分支"
FOUND_URL=""
for b in "$BRANCH" main master; do
  for p in "deploy/${SCRIPT}" "probe/deploy/${SCRIPT}" "${SCRIPT}"; do
    url="https://raw.githubusercontent.com/${REPO}/${b}/${p}"
    c="$(code "$url")"
    if [ "$c" = "200" ]; then
      ok "${b} : ${p}"
      [ -z "$FOUND_URL" ] && FOUND_URL="$url"
    elif [ "$c" = "404" ]; then
      printf '     404  %s : %s\n' "$b" "$p"
    else
      printf '     %s  %s : %s\n' "$c" "$b" "$p"
    fi
  done
done

if [ -z "$FOUND_URL" ]; then
  bad "所有候选路径都是 404：仓库里没有这个文件（或分支不对）"
  warn "修：确认你本地 D:\\DEEPSEEK\\probe\\deploy\\install-remote.sh 已经上传到仓库（网页上传时"
  warn "    拖文件夹经常漏掉子目录；上传后刷新仓库页面，肉眼确认能看到 deploy/ 目录）"
  FAIL=1
else
  ok "一键安装脚本可用地址："
  printf '     %s\n' "$FOUND_URL"
  warn "注意：如果它不是 .../${BRANCH}/deploy/${SCRIPT}，后面的命令要用上面这个地址"
fi

info "3. Release 与资产（这一步不通过，脚本下载二进制时照样 404）"
rel_json="$(body "https://api.github.com/repos/${REPO}/releases/latest")"
rel_code="$(code "https://api.github.com/repos/${REPO}/releases/latest")"
if [ "$rel_code" = "404" ]; then
  bad "还没有已发布的 Release（草稿 Draft 不算，必须点过 Publish）"
  warn "修：GitHub → Releases → Draft a new release → 选/建 tag v0.1.0 → Publish"
  warn "    或者本地跑 sh deploy/package.sh v0.1.0 再把 dist/ 里 7 个文件拖进 Release"
  FAIL=1
else
  tag="$(jsonfield "$rel_json" tag_name)"
  ok "最新 Release：${tag:-未知}"
  printf '     资产：\n'
  printf '%s' "$rel_json" | tr ',' '\n' | grep -o '"name":"[^"]*"' | sed 's/"name":"//; s/"$//' \
    | sed 's/^/       - /' | head -n 20

  sums_code="$(code "https://github.com/${REPO}/releases/latest/download/SHA256SUMS")"
  bin_code="$(code "https://github.com/${REPO}/releases/latest/download/${ASSET}")"
  if [ "$sums_code" = "200" ] && [ "$bin_code" = "200" ]; then
    ok "SHA256SUMS 与 ${ASSET} 都能下载"
  else
    bad "Release 资产不齐：SHA256SUMS=%s ${ASSET}=%s" "$sums_code" "$bin_code"
    warn "修：Release 里必须同时有这 7 个文件："
    warn "    probe-server-linux-amd64 / -arm64、probe-agent-linux-amd64 / -arm64、"
    warn "    install-server.sh、install-agent.sh、SHA256SUMS"
    FAIL=1
  fi
fi

info "4. 结论"
if [ "$FAIL" = "0" ]; then
  ok "一切就绪。可以装了："
  echo
  echo "  # 服务端（把 URL 换成第 2 步打印的那条地址）"
  echo "  curl -fsSL ${FOUND_URL:-https://raw.githubusercontent.com/${REPO}/${BRANCH}/deploy/${SCRIPT}} | sudo sh -s -- server"
  echo
  echo "  # Agent（在每台被监控的机器上）：Token 不进命令行，先写进一个只有 root 能读的文件"
  echo "  umask 077 && cat > /root/probe-token      # 粘贴面板给的 Token 后按 Ctrl-D（文件即 0600）"
  echo "  curl -fsSL ${FOUND_URL:-https://raw.githubusercontent.com/${REPO}/${BRANCH}/deploy/${SCRIPT}} \\"
  echo "    | sudo sh -s -- agent --server https://你的面板域名 --from-file /root/probe-token"
  echo "  # 装完可以删掉 /root/probe-token；旧的 --token pba_xxx 仍然可用，"
  echo "  # 但 Token 会进 ps / shell 历史 / sudo 审计日志，不推荐。"
else
  bad "上面标 [!!] 的就是原因，按提示修完再跑一次本脚本"
  warn "不想折腾 GitHub？看第 5 节：把 7 个文件放到你自己的服务器上，--base-url 直接指向它。"
fi

info "5. 不想用 GitHub（自建下载源，国内小鸡也快）"
cat <<'EOF'
  1) 本地打发布包（Windows PowerShell）：
       cd D:\DEEPSEEK\probe
       bash deploy/package.sh v0.1.0      # 没有 bash 时：见 docs/DEPLOY.md 第 6.1 节的 go build 三条命令
  2) 把 dist/ 里的 7 个文件传到任意可访问的静态目录，例如 /var/www/probe-dist/（nginx 直接托管）
  3) 装的时候加 --base-url：
       curl -fsSL https://你的域名/probe-dist/install-remote.sh | sudo sh -s -- server --base-url https://你的域名/probe-dist
  说明：--base-url 指向的目录里只要有 probe-server-linux-<arch>、SHA256SUMS、install-<角色>.sh 三个文件即可；
        脚本仍然会逐个校验 SHA256。
EOF

exit "$FAIL"
