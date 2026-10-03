#!/bin/sh
# 本地打发布包：产出 dist/ 下的一组资产，直接用于
#   - GitHub Release（网页拖拽上传，或 gh release create）
#   - 任意静态托管（对象存储 / 自己的服务器 / 内网分发）
#
# 用法：
#   sh deploy/package.sh            # 版本号取 git describe（没有 git 则用 dev）
#   sh deploy/package.sh v0.1.0     # 指定版本号（只影响提示与压缩包名）
#
# 产物（与 deploy/install-remote.sh 期望的布局一致）：
#   probe-server-linux-amd64 / -arm64
#   probe-agent-linux-amd64  / -arm64
#   install-server.sh / install-agent.sh
#   SHA256SUMS                    ← 上面全部文件的校验和
#   probe-<版本>-dist.tar.gz      ← 可选的整体打包（方便一次性上传/内网分发）
#
# 需要本机有 Go（1.27+，见 go.mod）。

set -eu

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
  else
    VERSION="dev"
  fi
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

command -v go >/dev/null 2>&1 || { echo "错误: 需要 Go（见 docs/DEPLOY.md 第 1 节）" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "错误: 需要 sha256sum" >&2; exit 1; }

# 版本注入：没有 -X 的话 --version 永远报 internal/version 的默认值 0.1.0-dev，
# 面板上"Agent 版本"那一行也就永远看不出装的是哪一版，产物无法与 tag 对应。
# 版本号去掉开头的 v（与 Makefile 的口径一致：tag v1.3.0 → 二进制里是 1.3.0）。
VERSION_STR="${VERSION#v}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X probe/internal/version.Version=${VERSION_STR} -X probe/internal/version.Commit=${COMMIT} -X probe/internal/version.BuildTime=${BUILT_AT}"

mkdir -p dist
echo "==> 构建 linux/amd64 + linux/arm64（版本 ${VERSION_STR}，commit ${COMMIT}）"
if [ "$COMMIT" = "unknown" ]; then
  echo "警告：拿不到 git commit（本机没有 git 或不在仓库里）⇒ 这批产物无法追溯到 commit；" >&2
  echo "      发布前请确认它就是你想要的那份源码构建出来的。" >&2
fi

CGO_ENABLED=0
export CGO_ENABLED

for arch in amd64 arm64; do
  GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" \
    -o "dist/probe-server-linux-$arch" ./cmd/probe-server
  GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" \
    -o "dist/probe-agent-linux-$arch" ./cmd/probe-agent
  echo "    linux/$arch 完成"
done

cp deploy/install-server.sh deploy/install-agent.sh dist/

echo "==> 生成 SHA256SUMS"
cd dist
rm -f SHA256SUMS
for f in probe-server-linux-amd64 probe-server-linux-arm64 \
         probe-agent-linux-amd64 probe-agent-linux-arm64 \
         install-server.sh install-agent.sh; do
  [ -f "$f" ] || { echo "缺少 $f" >&2; exit 1; }
  sha256sum "$f" >> SHA256SUMS
done
cat SHA256SUMS

# 发布自检：清单必须与目录里**刚构建出来**的这些文件一致。
# 手工改过 dist、或者把旧产物和旧清单混在一起时，这一步会失败 —— 这正是目的
# （dist/SHA256SUMS 与二进制不一致过一次，而客户端是 fail-closed：装不上）。
echo "==> 自校验 SHA256SUMS"
sha256sum -c SHA256SUMS || { echo "错误: dist 与 SHA256SUMS 不一致，拒绝发布" >&2; exit 1; }

echo "==> 打包 tar.gz（可选，便于内网一次性分发）"
tar czf "probe-${VERSION}-dist.tar.gz" \
  probe-server-linux-amd64 probe-server-linux-arm64 \
  probe-agent-linux-amd64 probe-agent-linux-arm64 \
  install-server.sh install-agent.sh SHA256SUMS

echo
echo "完成。发布方式二选一（资产只从本次构建的 dist/ 里取，别混入旧文件）："
echo
echo "  A) 命令行（需要 gh，先 gh auth login）："
echo "     gh release create ${VERSION} dist/probe-* dist/install-*.sh dist/SHA256SUMS \\"
echo "       --title \"${VERSION}\" --notes \"极简 VPS 探针 ${VERSION}\""
echo
echo "  B) 网页：GitHub → Releases → Draft a new release → 选 tag ${VERSION}"
echo "     把 dist/ 里的 4 个二进制、2 个安装脚本、SHA256SUMS 拖进去 → Publish"
echo "     （把上面那份 SHA256SUMS 内容一并贴进 Release Notes：哈希就有了带外记录）"
echo
echo "发布后即可用一键安装："
echo "  curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/deploy/install-remote.sh | sudo sh -s -- server"
