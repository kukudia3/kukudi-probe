# 极简 VPS 探针 —— 构建入口
#
# 目标是"单二进制、无 CGO、交叉编译即可发布"：
#   make check        运行 vet + 单元测试（提交前必跑）
#   make race         单元测试 + 竞态检查（-race，比 check 慢）
#   make vuln         漏洞扫描（依赖 + 标准库；有"可达"漏洞才算失败）
#   make build        本机二进制 -> dist/
#   make build-linux  交叉编译 linux/amd64 与 linux/arm64（无 CGO），server + agent
#   make release      交叉编译并生成 SHA256SUMS（deploy/install.sh 用它校验下载）
#   make bench        跑性能基准（Agent 采样、服务端查询）
#   make load         跑 50 节点负载用例

MODULE   := probe
DIST     := dist

# 发布资产固定为这 7 个文件：4 个 linux 二进制 + 2 个安装脚本 + SHA256SUMS
# （与 deploy/package.sh、.github/workflows/release.yml、docs/DEPLOY.md 第 1.3 节
# 的口径一致）。必须显式枚举：`sha256sum probe-*-linux-*` 这种 glob 永远盖不到
# 安装脚本，会让本机生成的清单比 CI/package.sh 少两行 —— 而 install-remote.sh
# 会逐个校验，清单缺条目就是"装不上"。
RELEASE_FILES := \
	probe-server-linux-amd64 probe-server-linux-arm64 \
	probe-agent-linux-amd64 probe-agent-linux-arm64 \
	install-server.sh install-agent.sh

# 版本号默认取自【最近的 git tag】（去掉开头的 v），拿不到才退回 0.1.0-dev。
#
# 为什么必须这么做：发布流程（.github/workflows/release.yml）只跑 `make release`，
# 并不会传 VERSION 进来 —— 所以在这之前，每个打出去的 tag 里编译出的二进制
# 都写着 "0.1.0-dev" ✗ 面板上"Agent 版本"那一行永远是它，看不出装的是哪一版。
# 用 ?= 是为了让 CI 或本地仍能显式覆盖（VERSION=1.2.3 make build）。
# sed 去掉 v 前缀；`/^$/d` + `grep .` 用来在 git 不可用 / 没有 tag 时走兜底
# （CI 的 checkout 是带 tag 的浅克隆，`git describe --tags` 在打 tag 触发时可用）。
VERSION  ?= $(shell git describe --tags --abbrev=0 2>/dev/null | sed -e 's/^v//' -e '/^$$/d' | head -n1 | grep . || echo 0.1.0-dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILT_AT)

GO      ?= go

.PHONY: all fmt vet test race vuln check build build-linux release run bench load clean

all: check build

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

# 漏洞扫描（依赖 + 标准库）：只有当漏洞"可达"时才返回非 0。
# 版本钉住（v1.8.0）保证可复现；漏洞库是运行时从 vuln.go.dev 取的，
# 所以新披露的条目一样能发现。CI 的发布闸门跑同一条。
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

check: vet test

build:
	@mkdir -p $(DIST)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/probe-server ./cmd/probe-server
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/probe-agent ./cmd/probe-agent

# 交叉编译：4 条命令写全（不用 foreach 之类的花活——Makefile 出错比多写两行贵得多）。
# 交叉编译出来的名字与 deploy/install*.sh 期望的文件名一致。
build-linux:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/probe-server-linux-amd64 ./cmd/probe-server
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/probe-agent-linux-amd64 ./cmd/probe-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/probe-server-linux-arm64 ./cmd/probe-server
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/probe-agent-linux-arm64 ./cmd/probe-agent

release: build-linux
	@echo "==> 版本 $(VERSION) / commit $(COMMIT) / 构建于 $(BUILT_AT)"
	@[ "$(COMMIT)" != "unknown" ] || echo "警告：拿不到 git commit（本机没有 git 或不在仓库里），这批产物无法追溯到 commit" >&2
	cp deploy/install-server.sh deploy/install-agent.sh $(DIST)/
	cd $(DIST) && rm -f SHA256SUMS && sha256sum $(RELEASE_FILES) > SHA256SUMS
	@echo "==> 自校验：清单必须与刚构建出来的文件一致"
	cd $(DIST) && sha256sum -c SHA256SUMS
	@cat $(DIST)/SHA256SUMS

bench:
	$(GO) test ./internal/agent/ -run XXX -bench BenchmarkCollector -benchtime 5000x
	$(GO) test ./internal/server/ -run XXX -bench Benchmark -benchtime 2000x

load:
	$(GO) test ./internal/e2e/ -run TestFiftyAgentsLoad -v

run:
	$(GO) run ./cmd/probe-server --data-dir ./data

# DIST 可以在命令行覆盖，所以先把"明显会误删"的取值挡掉：
# `make clean DIST=/` 之类不该有第二次机会。
clean:
	@case "$(DIST)" in ""|.|./|..|../*|/*|*/../*) echo "拒绝：DIST=$(DIST) 不是仓库内的构建目录" >&2; exit 1 ;; esac
	rm -rf -- "$(DIST)"
