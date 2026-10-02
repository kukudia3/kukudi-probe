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

`deploy/install-remote.sh` 第二十几行有一行（仓库里的实际默认值就是本项目当前的发布仓库）：

```sh
DEFAULT_GITHUB="kukudia3/kukudi-probe"
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

### 装不上？先跑自检

`curl: (22) The requested URL returned error: 404` 是最常见的失败。GitHub 在四种情况下
**都回 404**（故意不用 403，以免泄漏私有仓库的存在），所以必须逐个排除：

```bash
# 在 VPS 上跑（只读，不改任何东西）
sh doctor.sh kukudia3/kukudi-probe
```

它会依次检查并给出结论与修法：

1. **DNS/网络**：`github.com`、`raw.githubusercontent.com` 解析到内网/本机地址 = DNS 被污染或代理改写；
2. **仓库是否存在/是否私有**：私有仓库的匿名 raw 访问一律 404 → 把仓库改成 Public
   （Settings → General → 最下面 Danger Zone → Change visibility），或走第 5 节的 `--base-url` 自建源；
3. **文件在哪个分支/哪个路径**：自动尝试 `main`/`master` 与
   `deploy/…`、`probe/deploy/…`、`…`（网页拖拽上传经常漏掉子目录，或仓库里多套了一层目录）；
4. **Release 是否真的发布了**：草稿（Draft）状态同样 404，必须点过 Publish；
   并逐个验证 `SHA256SUMS` 与 `probe-server-linux-<arch>` 是否可下载。

> Windows 上没有 `sh` 也能查：把 `doctor.sh` 里第 1~3 节的 URL 复制到浏览器/`curl.exe` 里看状态码即可，
> 或者把脚本 `scp` 到 VPS 上跑。

---

## 3. 拿到初始化码

```bash
journalctl -u probe-server | grep setup_code
# level=WARN msg="尚未初始化管理员…" setup_code=b0e167559ce4 expires_in=30m0s
```

检查服务是否在跑：

```bash
systemctl status probe-server --no-pager
curl -s http://127.0.0.1:25774/healthz     # {"ok":true,"db":"ok"}（匿名只给这两项；版本/commit 要登录，见 DESIGN §20）
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

### 4.3 改 `--trusted-proxy`（v1.0.8 起默认已开，一般不用动）

**安装脚本现在默认就信任 `127.0.0.1`**（写成单元里的 `Environment=PROBE_TRUSTED_PROXY=127.0.0.1`）。
对"本机反代（Caddy/nginx）"和"Cloudflare 隧道（cloudflared）"这两种推荐部署，这个默认值就是对的，
**不需要再手动加**。

为什么默认开：探针只监听 `127.0.0.1`，连得上它的只有本机的反代或隧道；不信任的话，
审计日志与登录限流看到的全是 `127.0.0.1` —— 限流等于所有人共用一个桶，形同虚设。
而外部直连的请求对端不是回环地址，它们伪造的 `X-Forwarded-For` 照样被忽略，所以不亏安全。

**什么时候要改**：

| 情况 | 怎么做 |
|---|---|
| 反代在**另一台机器** | `sh install-server.sh --trusted-proxy <那台机器的 CIDR>` 重装一次 |
| 想完全关掉 | `sh install-server.sh --trusted-proxy ""` |
| 只想临时改 | 见下面的 drop-in |

用 drop-in 覆盖（**只加一行 `Environment=` 即可**，比改 `ExecStart` 干净得多）：

```bash
systemctl edit probe-server
```

```ini
[Service]
Environment=PROBE_TRUSTED_PROXY=10.0.0.0/8
```

```bash
systemctl daemon-reload && systemctl restart probe-server
```

> 为什么用 `Environment=` 而不是把 `--trusted-proxy` 写进 `ExecStart`：
> 一旦有人用 drop-in 整体覆盖了 `ExecStart`（换监听地址、加参数都会那么干），
> 主单元里的命令行参数会被**整个忽略**；`Environment=` 是独立的一条，照样生效。

**怎么验证生效了**：

```bash
systemctl cat probe-server | grep -i trusted
journalctl -u probe-server -n 20 --no-pager | grep -i "ip="    # 应该看到真实访客 IP，而不是 127.0.0.1
```

**顺便可以加的参数**：

| 参数 | 建议值 | 为什么 |
|---|---|---|
| `--timezone` | `Asia/Shanghai` | 日流量/计费周期的"天"按这个时区切。服务器是 UTC 而你想按北京时间算月流量时**必须**显式设置 |
| `--retention-1m` | `2160h`（90 天） | 默认只留 8 天，1d/3d/7d 曲线够用；想留更久就调大 |
| `--retention-10s` | `24h` | 默认 12 小时（1h/6h 档用） |
| `--log-level` | `info` | 排错时临时改 `debug` |

### 4.4 汇率（把外币价格折算成人民币，可选）

节点的价格可以填**外币**（美元/欧元/日元…）。服务端会自己取汇率，把外币金额**同时**
折算成人民币显示（`$100.00 USD · ¥714.29`）。**前端永远不碰外部 URL**，取汇率只发生在服务端。

时机：服务端启动时取一次，之后**每天一次**。数据源按顺序试第一个成功的：

1. `https://api.frankfurter.app/latest?from=CNY`
2. `https://open.er-api.com/v6/latest/CNY`

两者都以人民币为基准。取到的那一份会**落库**（settings 表里的 `fx_rates`），重启不丢；
设置页的「服务器信息」里能看到用的是**哪一天的、从哪取的、是不是兜底的**。

**服务器不能出网**（内网、防火墙白名单）时，把它关掉就再也不会有那次请求：

```bash
systemctl edit probe-server
```

```ini
[Service]
Environment=PROBE_FX=0
```

```bash
systemctl daemon-reload && systemctl restart probe-server
```

> 与 4.3 同一个理由：用 `Environment=` 而不是往 `ExecStart` 里加 `--fx=false` ——
> drop-in 覆盖 `ExecStart` 时命令行参数会被整个忽略，`Environment=` 照样生效。
> 主单元文件**不用改**（安装脚本没有为这个功能加任何一行）。

关掉之后价格照常显示人民币口径：一直用**上次取到的那一份**；从没取到过就用
**内置兜底表**（写死在程序里的量级估计，界面会明确标注「内置兜底」）。
三级降级（新的 → 旧的 → 内置兜底）保证**不会**因为取不到汇率把价格显示成 0、空白或报错。

想指向自建镜像 / 内网代理（逗号分隔，按顺序试）：

```ini
[Service]
Environment=PROBE_FX_RATE_URL=http://mirror.internal/fx,https://backup.example/fx
```

日志：取汇率失败每个源只记**一条** debug 日志（`--log-level debug` 才看得到），
不会刷屏、也不影响落盘/聚合/清理这些后台任务。

### 4.5 响应压缩（默认已经开着，一般不用管）

服务端对浏览器侧的响应做 gzip。**默认开着**，不需要配任何东西。

为什么这件事值得单独说：面板开着（SSE 常连）时，**浏览器 → 面板的流量比 Agent
上报大一个数量级** —— 实测 1 个节点约 139 MB/天、5 个节点约 642 MB/天、
20 个节点约 2.54 GB/天，而这全是 JSON 与 JS 文本；首屏 4 个静态资源一共约 508 KB
（复核时：`index.html` 55235、`app.js` 293479、`style.css` 90727、`chart.js` 68190 字节）。
压完通常只有原来的 1/2 ~ 1/8：

| 响应 | 明文 | gzip | 倍数 |
|---|---|---|---|
| `index.html`（`/`） | 48680 | 15607 | 3.12× |
| `app.js` | 260844 | 100324 | 2.60× |
| `chart.js` | 66984 | 28011 | 2.39× |
| `style.css` | 79407 | 28765 | 2.76× |
| `/api/v1/nodes` | 1416 | 656 | 2.16× |
| `/api/v1/nodes/{id}` | 2240 | 834 | 2.69× |
| SSE 流（8 帧） | 13271 | 1768 | 7.51× |

> 表里的数字是**当时**（前端的某次快照）实测的，前端之后又长大了（见上面那四个字节数），
> 所以绝对字节数别当现值用；**压缩倍数量级没变**，而 `/api/v1/nodes`、SSE 两行本来就随
> 节点数与帧内容浮动。

细节（都有测试守着）：只压 `text/html`、`text/css`、`application/javascript`、
`text/javascript`、`application/json`、`text/event-stream`、`image/svg+xml`
这几种；小于 512 字节的响应不压（压了反而更大）；`204/304/206` 与二进制不压；
已经自己声明了 `Content-Encoding` 的绝不覆盖；每个响应都带
`Vary: Accept-Encoding`；`Content-Length` 会按压缩后的长度处理。
**SSE 不受"小于 512 字节不压"这条限制**，而且每帧写完立刻 flush
（不这么做页面会"连接正常但一个字节都不动"）。

**什么时候要关掉它**：撞上对 gzip 处理有问题的中间设备或抓包/审计工具时。
关掉之后客户端拿到的是明文，功能完全一样，只是流量回到压缩前：

```bash
systemctl edit probe-server
```

```ini
[Service]
Environment=PROBE_GZIP=0
```

```bash
systemctl daemon-reload && systemctl restart probe-server
```

> 与 4.3 / 4.4 同一个理由：用 `Environment=` 而不是往 `ExecStart` 里加
> `--gzip=false` —— drop-in 覆盖 `ExecStart` 时命令行参数会被整个忽略，
> `Environment=` 照样生效。主单元文件**不用改**（安装脚本没有为这个功能加任何
> 一行，`deploy/` 下的单元文件也不该动）。
>
> 取值只认 `1/0`、`true/false`、`yes/no`、`on/off`；写成别的（例如
> `PROBE_GZIP=maybe`）服务端会**启动即报错**，而不是悄悄当成开着。

**怎么验证生效了**：

```bash
curl -sI -H 'Accept-Encoding: gzip' https://你的域名/app.js | grep -i -E 'content-encoding|vary'
# 期望看到：Content-Encoding: gzip 与 Vary: Accept-Encoding
```

> 反代不用做任何事：Caddy 与 nginx 都会原样透传 `Content-Encoding`。
> 但**不要**在反代那一层再开一次 gzip（会二次压缩，客户端解出来是坏的）。

### 4.6 防火墙

只放行 `443/tcp`（以及 Caddy 自动签发证书用的 `80/tcp`）。
**不要**把 `25774` 暴露到公网 —— 它不是给公网用的。

### 4.7 想挂在子路径下（比如 `https://example.com/probe/`）

可以。前端用的是相对资源路径 + 运行时计算的前缀（`apiURL()`），所以在子路径下也能正常工作，
**唯一要求是反代把前缀剥掉**、并且访问时带上尾斜杠。

nginx：

```nginx
location /probe/ {
    proxy_pass http://127.0.0.1:25774/;   # 结尾这个 "/" 就是"剥掉 /probe/"
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_read_timeout 3600s;
    proxy_set_header Connection "";
}
```

Caddy：

```caddy
example.com {
    handle_path /probe/* {          # handle_path 会剥掉 /probe 前缀
        reverse_proxy 127.0.0.1:25774 {
            flush_interval -1
        }
    }
}
```

访问 `https://example.com/probe/`（**带尾斜杠**）。
子路径部署下 `--trusted-proxy` 照样要配（见 4.3），否则 Cookie 不会带 `Secure`。

---

## 5. 打开面板，把它用起来

> **先说清楚：没有"另一个后台"。这个网页就是后台。**
> 服务端没有单独的 admin 站点、也没有需要另开的端口；第一次登录进去时数据库里
> 一个节点都没有，所以页面是"空"的 —— 顶栏 + 概览条 + 一句提示：

```
在线 0/0   抖动 0   离线 0                          更新时间 12:00:00
还没有节点。点右上角「新增节点」创建第一个，然后把页面给出的安装命令贴到 VPS 上执行。
```

界面上的功能分布：

| 位置 | 作用 |
|---|---|
| 右上角「新增节点」 | 建节点；创建后弹窗里给出**只显示一次**的 Token 与安装命令 |
| 右上角「设置」 | Telegram 告警（Bot Token / Chat ID / 开关 / 测试）、告警阈值、**修改管理员密码**、**两步验证**（TOTP）、**操作记录**（谁在什么时候做了什么） |
| 右上角「◐」 | 深浅色切换 |
| 首页节点卡片 | 点进去是详情：CPU/内存/磁盘/网络/延迟/流量曲线，范围 1h/6h/12h/1d/3d/7d |
| 详情页按钮 | 「编辑」「换 Token」「删除」 |

流程：

1. 输入日志里的**初始化码** + 管理员用户名 + 密码（≥10 位）→ 完成初始化并自动登录
   （初始化码用掉即失效；重启服务端会重新生成一个）；
2. 右上角「新增节点」→ 填名称（如 `HK-01`）、分组/地区、上报间隔、月流量额度、流量重置日、到期日；
3. 创建后会**只显示一次 Token**（`pba_...`）—— 立刻复制保存，关掉就看不到了（只能重新生成）；
4. 把 Token 填进 Agent 的安装命令（见第 2 节）→ 1~2 秒后面板上出现卡片并显示**在线**。

> 建议顺手开上两步验证（设置 → 安全 → 两步验证）：二维码由**这台服务器自己**画
> （纯标准库，不经过任何在线二维码服务），扫进验证器 App 之后登录要多输一次 6 位码。
> 启用时给出的 10 个恢复码请抄下来 —— 忘了密码又丢了验证器时的救援办法见 **8.1**。

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

### 8.1 两步验证（TOTP）与"把自己锁在门外"怎么办

面板支持两步验证（设置 → 安全 → 两步验证）：用 Google Authenticator / 1Password /
Authy 扫一下二维码，之后登录除了密码还要输一次 6 位动态码。启用时会显示 **10 个
一次性恢复码**（只显示那一次，请抄下来）。

万一**密码忘了 + 验证器丢了**（或手机换了、恢复码也丢了），面板在服务器本机可以重置：

```bash
# 在本机（能登录这台服务器的 shell）执行，不需要知道面板密码 ——
# 要重置的很可能正是那个密码。
sudo -u probe /usr/local/bin/probe-server --reset-2fa --data-dir /var/lib/probe-server

# 期望输出（WARN 一条，含时间 / 操作系统用户 / 主机名）：
# level=WARN msg="已按 --reset-2fa 关闭两步验证：现在登录只需密码，请在登录后立刻重新启用" \
#   time=2025-01-02T03:04:05Z os_user=root host=vps-01 data_dir=/var/lib/probe-server changed=true

# 重启服务（严格说不必：登录的每一步都会重读数据库，重置立刻生效；
# 重启一次只是让运行中的进程状态最干净）
systemctl restart probe-server
```

几条要知道的事：

- `--reset-2fa` **不校验任何凭据**，它只做一件事：把数据库里的两步验证密钥、
  恢复码、防重放计数器删掉。安全性靠"必须能在服务器本机执行命令"这一条 ——
  能做到这件事的人本来就能直接读数据库、读走 Agent 上报的数据。
- 这次操作会同时写进 **服务端日志**与**面板的操作记录**（动作名 `twofa_reset`，
  带操作系统用户名与主机名），事后翻得到是谁在什么时候重置的。
- **密码不会被重置**：重置之后用原来的密码登录，进去之后请立刻重新启用两步验证。
- 忘了 `--data-dir` 的话：安装脚本用的是 `/var/lib/probe-server`，
  也可以用 `systemctl cat probe-server` 看一眼实际的启动参数。
- 密码**也**忘了的话，本机同样能救（会保留全部节点数据）：

  ```bash
  systemctl stop probe-server
  # 两个键必须一起删：只删哈希的话，用户名那一行还在，
  # 初始化会因为"管理员已存在"直接 409（详见 store.CreateAdminIfAbsent）。
  sqlite3 /var/lib/probe-server/probe.db \
    "DELETE FROM settings WHERE key IN ('admin_username','admin_password_hash');"
  systemctl start probe-server
  # 重启后面板回到"首次初始化"，日志里会重新打印一次性初始化码（见第 3 节）。
  ```

---

## 9. 几个容易踩的坑

| 坑 | 说明 |
|---|---|
| **进去以后"什么都没有"** | 这是**正常的**：面板就是后台，没有第二个管理端。新装好时数据库里 0 个节点，页面会显示"还没有节点。点右上角「新增节点」…"。点它建一个节点、装好 Agent，卡片就出来了 |
| **页面一片空白**（连标题和按钮都没有） | 多半是**子路径部署但反代没剥前缀 / 资源 404**：按 F12 看 Network 里 `app.js`、`style.css` 是不是 404。根路径部署最省事；子路径部署见 4.7（必须剥前缀 + 访问带尾斜杠） |
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
