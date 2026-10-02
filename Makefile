# 极简 VPS 探针 —— 构建入口
#
# 目标是"单二进制、无 CGO、交叉编译即可发布"：
#   make check        运行 vet + 单元测试（提交前必跑）
#   make build        本机二进制 -> dist/
#   make build-linux  交叉编译 linux/amd64 与 linux/arm64（无 CGO），server + agent
#   make release      交叉编译并生成 SHA256SUMS（deploy/install.sh 用它校验下载）
#   make bench        跑性能基准（Agent 采样、服务端查询）
#   make load         跑 50 节点负载用例

MODULE   := probe
DIST     := dist

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

.PHONY: all fmt vet test race check build build-linux release run bench load clean

all: check build

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

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
	cd $(DIST) && sha256sum probe-*-linux-* > SHA256SUMS && cat SHA256SUMS

bench:
	$(GO) test ./internal/agent/ -run XXX -bench BenchmarkCollector -benchtime 5000x
	$(GO) test ./internal/server/ -run XXX -bench Benchmark -benchtime 2000x

load:
	$(GO) test ./internal/e2e/ -run TestFiftyAgentsLoad -v

run:
	$(GO) run ./cmd/probe-server --data-dir ./data

clean:
	rm -rf $(DIST)
