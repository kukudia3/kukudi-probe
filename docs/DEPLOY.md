# 部署手册（照着抄就行）

目标：把这套探针装到你的小鸡上 —— **一台跑服务端（面板 + 数据库），每台被监控的机器跑一个 Agent**。
服务端和 Agent 可以是同一台机器（自己监控自己）。

- 服务端：一个二进制 + SQLite，**不需要** MySQL/Redis/Docker；常驻内存约 40 MB。
- Agent：一个二进制，**只出站**连接服务端，不监听任何端口。
- 面板：浏览器打开即用，默认**只监听 127.0.0.1**，公网访问走 Caddy/nginx 反代 + TLS。

有两条路，挑一条：

| | 一次性把代码发到 GitHub，之后一条命令装 | 每次都自己传文件 |
|---|---|---|
| 怎么装 | `curl … \| sudo sh -s -- server`（见第 1 节） | 交叉编译 + `scp` + `sh install-server.sh`（见第 6 节） |
| 适合 | 机器多、要经常升级、想给朋友一条命令 | 只想先跑起来、代码不想公开 |
| 前置 | 一个 GitHub 仓库（公开或私有都行，见第 1 节） | 无 |

---

## 1. 先发布一次（只做一次，之后永远是一条命令）

### 1.1 把代码放到 GitHub

代码现在只在你自己的电脑上（`D:\DEEPSEEK\probe`）。**这一步必须你自己操作**——我不会替你上传任何代码。

三种方式任选：

**A. 网页上传（不用装 git）**

1. GitHub → New repository → 名字如 `probe` → Private 或 Public 都行（Private 也能用于 Releases）→ Create；
2. 在仓库页点 `uploading an existing file`；
3. 把 `D:\DEEPSEEK\probe` 里的**除 `dist\` 以外**的所有文件/文件夹拖进去（`.github`、`cmd`、`deploy`、`docs`、`internal`、`web`、`go.mod`、`go.sum`、`Makefile`、`README.md` 等）→ Commit。

> 注意：GitHub 网页上传对**单文件 25 MB、一次 100 个文件**有限制。本项目源码文件很小，一般没问题；
> `.gitignore` 已经把 `dist/` 排除，别把编译产物传上去。

**B. 装了 GitHub CLI（`gh`）**：`gh auth login` 之后
```bash
cd D:\DEEPSEEK\probe
git init -b main
git add .
git commit -m "极简 VPS 探针"
gh repo create probe --private --source=. --push
```

**C. 装了 git**：`git init -b main && git add . && git commit -m init && git remote add origin git@github.com:你/probe.git && git push -u origin main`

### 1.2 把脚本里的仓库名改成你的

`deploy/install-remote.sh` 第二十几行有一行：

```sh
DEFAULT_GITHUB="OWNER/probe"
```

改成 `你的用户名/probe`（不改也行，只要每次命令都带 `--github 你的用户名/probe`）。

### 1.3 打一个 tag，让 CI 自动构建并发布 Release

仓库里已经带了 `.github/workflows/release.yml`：**push 一个 `v*` 的 tag，它就会跑测试、交叉编译 linux/amd64 + arm64、并用 GitHub Release 发布**这 7 个资产：

```
probe-server-linux-amd64   probe-server-linux-arm64
probe-agent-linux-amd64    probe-agent-linux-arm64
install-server.sh          install-agent.sh
SHA256SUMS
```

```bash
# 网页方式：仓库 → Releases → Draft a new release → Choose a tag → 输入 v0.1.0 → Create new tag
#           → Publish release（CI 会在 tag 出现后自动把资产传上去）
# 或者命令行：
git tag v0.1.0 && git push origin v0.1.0
```

不想用 CI（或没有 Actions 额度）：在**本地**跑打包脚本，然后把文件拖到 Release 里：

```powershell
cd D:\DEEPSEEK\probe
# 需要本机有 Go；产物在 dist\ 里
bash deploy/package.sh v0.1.0      # 没有 bash 就按第 6 节的三条 go build 手工产出
```
然后 Releases → Draft a new release → 选 tag `v0.1.0` → 把 `dist\` 里的 4 个二进制、2 个安装脚本、`SHA256SUMS` 拖进去 → Publish。

---

## 2. 一条命令安装（之后的日常）

### 服务端

```bash
curl -fsSL https://raw.githubusercontent.com/你的用户名/probe/main/deploy/install-remote.sh \
  | sudo sh -s -- server
```

### Agent（每台被监控的机器）

```bash
curl -fsSL https://raw.githubusercontent.com/你的用户名/probe/main/deploy/install-remote.sh \
  | sudo sh -s -- agent --server https://monitor.example.com --token pba_xxx
```

这条命令做的事，和你手工做完全一样：

1. 认架构（`x86_64`→amd64、`aarch64`→arm64）；
2. 从 `https://github.com/你的用户名/probe/releases/latest/download/` 下载二进制、`SHA256SUMS`、`install-<角色>.sh`；
3. **逐个校验 SHA256**，任何一个对不上就中止（文件落在临时目录，校验通过才 `chmod +x` 并执行）；
4. 调用角色安装脚本完成建用户、写 systemd 单元、装二进制、启动服务；
5. 结束后提示下一步（服务端会提示去哪里取初始化码）。

升级就是**再跑一次同一条命令**：数据、Token、配置都不动。

### 常用变体

```bash
# 指定版本（可回滚）
... | sudo sh -s -- server --version v0.1.0

# GitHub 拉不动时换源（镜像前缀 + 原地址，或任何自己搭的静态目录）
... | sudo sh -s -- server --base-url https://ghfast.top/https://github.com/你/probe/releases/latest/download
... | sudo sh -s -- agent  --base-url https://你的域名/probe-dist --server https://monitor.example.com --token pba_xxx

# 换仓库
... | sudo sh -s -- server --github 别人/probe

# 卸载（默认保留数据；--purge 连数据一起删）
... | sudo sh -s -- server --uninstall
```

> `--base-url` 指向的目录里只要有 `probe-server-linux-amd64`、`SHA256SUMS`、`install-server.sh` 这三个文件就能装。
> 用 http 镜像只适合你信任的内网；公网请用 https。

### 安全边界（写死在脚本里，有测试守着）

- 只从 https 默认源或你显式指定的源下载；**不用** `curl -k` 之类降级；
- 先落临时目录 → 校验 SHA256 → 才执行；校验失败立即退出并丢弃文件；
- 安装脚本自己（`install-remote.sh`）不联网执行任何"管道进来的内容"；
- 脚本是纯 POSIX sh、LF 换行、`set -eu`。

---

## 3. 拿到初始化码

```bash
journalctl -u probe-server | grep setup_code
# level=WARN msg="尚未初始化管理员…" setup_code=b0e167559ce4 expires_in=30m0s
```

检查服务是否在跑：

```bash
systemctl status probe-server --no-pager
curl -s http://127.0.0.1:25774/healthz     # {"ok":true,...}
```

---

## 4. 反代 + TLS（公网访问的唯一推荐方式）

面板默认只监听 `127.0.0.1:25774`，所以反代必须装在同一台机器上。

### 4.1 Caddy（最省事，自动申请证书）

`/etc/caddy/Caddyfile`：

```caddy
monitor.example.com {
    reverse_proxy 127.0.0.1:25774 {
        flush_interval -1          # 关键：SSE 必须不缓冲，否则实时数据会攒着一起发
    }
}
```

```bash
systemctl reload caddy
```

Caddy 会自动带上 `X-Forwarded-Proto`，所以按下面的第 4.3 步加上 `--trusted-proxy` 之后，
登录 Cookie 会自动带 `Secure`、日志/审计里也是真实访客 IP。

### 4.2 nginx（已有 nginx 时）

```nginx
server {
    listen 443 ssl http2;
    server_name monitor.example.com;
    ssl_certificate     /etc/letsencrypt/live/monitor.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/monitor.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:25774;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE（实时数据）必须关缓冲，否则面板不会"每秒刷新"
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_set_header Connection "";
    }
}
```

### 4.3 加 `--trusted-proxy`（强烈建议）

让服务端认识反代地址，这样它才敢采信转发头（否则日志里记的都是 `127.0.0.1`，
而且 Cookie 不会带 `Secure`）：

```bash
systemctl edit probe-server
```

在打开的编辑器里写（第一行 `ExecStart=` 是清空，必须保留）：

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/probe-server --listen 127.0.0.1:25774 --data-dir /var/lib/probe-server --trusted-proxy 127.0.0.1/32
```

```bash
systemctl daemon-reload && systemctl restart probe-server
```

**顺便可以加的参数**（都在这里一起写进 `ExecStart`）：

| 参数 | 建议值 | 为什么 |
|---|---|---|
| `--timezone` | `Asia/Shanghai` | 日流量/计费周期的"天"按这个时区切。服务器是 UTC 而你想按北京时间算月流量时**必须**显式设置 |
| `--retention-1m` | `2160h`（90 天） | 默认只留 8 天，1d/3d/7d 曲线够用；想留更久就调大 |
| `--retention-10s` | `24h` | 默认 12 小时（1h/6h 档用） |
| `--log-level` | `info` | 排错时临时改 `debug` |

### 4.4 防火墙

只放行 `443/tcp`（以及 Caddy 自动签发证书用的 `80/tcp`）。
**不要**把 `25774` 暴露到公网 —— 它不是给公网用的。

---

## 5. 打开面板，把它用起来

浏览器访问 `https://monitor.example.com/`：

1. 输入日志里的**初始化码** + 管理员用户名 + 密码（≥10 位）→ 完成初始化并自动登录
   （初始化码用掉即失效；重启服务端会重新生成一个）；
2. 右上角「新增节点」→ 填名称（如 `HK-01`）、分组/地区、上报间隔、月流量额度、流量重置日、到期日；
3. 创建后会**只显示一次 Token**（`pba_...`）—— 立刻复制保存，关掉就看不到了（只能重新生成）。

> 想监控服务端这台机器自己？照着第 2 节在本机也装一个 Agent，`--server` 填公网域名。

---

## 6. 手工路线（不想用 GitHub Releases）

> 如果你走了第 2 节，这一节可以跳过。

### 6.1 在 Windows 上交叉编译

```powershell
cd D:\DEEPSEEK\probe
$env:CGO_ENABLED = "0"

# x86_64 的小鸡
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w" -o dist\probe-server-linux-amd64 .\cmd\probe-server
go build -trimpath -ldflags "-s -w" -o dist\probe-agent-linux-amd64  .\cmd\probe-agent

# arm64 的小鸡（Oracle Ampere / Hetzner CAX / 树莓派等）
$env:GOARCH = "arm64"
go build -trimpath -ldflags "-s -w" -o dist\probe-server-linux-arm64 .\cmd\probe-server
go build -trimpath -ldflags "-s -w" -o dist\probe-agent-linux-arm64  .\cmd\probe-agent
$env:GOOS = ""; $env:GOARCH = ""

# 记下哈希
Get-ChildItem dist\probe-*-linux-* | ForEach-Object {
  "$((Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower())  $($_.Name)"
} | Tee-Object dist\SHA256SUMS
```

**先确认架构**：小鸡上 `uname -m` → `x86_64` 用 amd64，`aarch64` 用 arm64。
两个架构都支持的安装脚本会自动挑对的那个。

### 6.2 上传 + 装

```powershell
cd D:\DEEPSEEK\probe
scp dist\probe-server-linux-amd64 dist\probe-agent-linux-amd64 dist\SHA256SUMS root@1.2.3.4:/root/
scp deploy\install.sh deploy\install-server.sh deploy\install-agent.sh root@1.2.3.4:/root/
```

```bash
ssh root@1.2.3.4
cd /root
sh install-server.sh            # 幂等；重复执行 = 升级
```
Agent 那台：
```bash
cd /root
sh install.sh agent --file ./probe-agent-linux-amd64 --sha256 <哈希> \
   --server https://monitor.example.com --token pba_xxx
```

---

## 7. 告警（可选，5 分钟）

面板右上角「设置」：

1. 在 Telegram 里给 `@BotFather` 发 `/newbot` 拿 **Bot Token**（形如 `123456789:AA...`）；
2. 给你的 bot 发一条消息（或把它拉进群），再用 `@userinfobot` 拿你的数字 **Chat ID**；
3. 面板里填 Token + Chat ID，勾选「启用」→ 保存 → 点「发送测试」；
4. 收到测试消息就成了。之后**离线 / 恢复 / 流量超限 / 到期**都会推送。

没配 Telegram 也不影响使用：所有告警仍然会写进服务端日志（`journalctl -u probe-server | grep 触发告警`）。

---

## 8. 日常运维

```bash
# 看状态 / 日志
systemctl status probe-server --no-pager
journalctl -u probe-server -f

# 改参数（见第 4.3 步）：systemctl edit probe-server 后
systemctl daemon-reload && systemctl restart probe-server

# 备份（先停服务，保证 WAL 一起落盘；数据目录 0700，只有 root 能读）
systemctl stop probe-server
tar czf /root/probe-backup-$(date +%F).tar.gz -C /var/lib probe-server
systemctl start probe-server

# 恢复：把 tar 解回 /var/lib（覆盖 probe-server 目录），确认属主是 probe:probe
chown -R probe:probe /var/lib/probe-server && systemctl start probe-server

# 升级：走 GitHub 的话就是重跑一条命令（见第 2 节）；手工路线重跑安装脚本
sh install-server.sh

# 卸载（默认保留数据；加 --purge 才连数据与用户一起删）
sh install-server.sh --uninstall
sh install-agent.sh  --uninstall
```

面板里的「设置 → 操作记录」能看到谁在什么时候做了什么（含登录失败、节点增删、改密）。

---

## 9. 几个容易踩的坑

| 坑 | 说明 |
|---|---|
| 面板不刷新 / 一直"未连接" | 反代没关缓冲。Caddy 要 `flush_interval -1`，nginx 要 `proxy_buffering off` |
| 详情页只有最近 1 分钟有数据 | 1d/3d/7d 档读的是**1 分钟层**，由每分钟的 rollup 生成；服务端刚起来时等 1~2 分钟 |
| Agent 报 "拒绝以明文连接非本机地址" | `--server` 必须用 `https://`（只有 127.0.0.1/localhost 允许 http）。这是故意的：明文会把节点信息送给链路上的人 |
| 日志里访客 IP 都是 127.0.0.1 | 没配 `--trusted-proxy`（见 4.3） |
| 一天/一月流量归属不对 | 没配 `--timezone`，服务端按 UTC 切天 |
| `uname -m` 是 aarch64，却拿了 amd64 的二进制 | 装不上或一启动就 `Exec format error`。用 `--base-url`/`--version` 重跑，或手工下 `-linux-arm64`（安装脚本会优先找本架构的文件） |
| GitHub 下载很慢或连不上 | 用 `--base-url` 换镜像（见第 2 节变体） |
| SQLite 放在 NFS/对象存储挂载上 | **不行**，SQLite 的锁依赖本地文件系统；安装脚本会检测并拒绝（nfs/cifs/fuse/overlay） |
| 容器里跑 | 可以，但数据目录要挂到宿主机卷上，并且同样别用网络文件系统 |
| Release 里少了某个文件 | `install-remote.sh` 会明确报"下载失败"或"SHA256SUMS 里没有 …"；对着第 1.3 节的 7 个资产核对 |
| 端口忘了关 | 只暴露 443（+80 给 ACME）。`25774` 只给本机反代用 |

---

## 10. 只有一台小鸡、也不想暴露公网？

两条路，都不用买域名：

1. **SSH 隧道**（最省事）：服务端保持默认只监听本机，在**你电脑上**执行
   `ssh -N -L 25774:127.0.0.1:25774 root@小鸡IP`，然后浏览器打开 `http://127.0.0.1:25774/`；
   Agent 仍然连 `http://127.0.0.1:25774`（同一台机器时）。
2. **WireGuard/Tailscale 内网**：服务端 `--listen` 改成内网地址（例如 `10.0.0.1:25774`），
   Agent 用 `http://10.0.0.1:25774`（内网可接受；要更稳可以给内网地址配 TLS 证书）。

> 注意：只要 Agent 不是连 `127.0.0.1`，就必须用 `https://` —— 用内网 IP 时请给它配上证书，
> 或显式加 `--allow-plaintext`（仅在你完全信任该内网时）。
