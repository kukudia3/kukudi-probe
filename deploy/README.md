# 部署脚本

四个 POSIX `sh` 脚本（没有 bash 依赖），只做"建用户、放二进制、写 systemd 单元、起服务"这几件事。
**幂等**：重复执行就是升级，不会重复建用户、不会覆盖已有 Token。

| 文件 | 作用 |
|---|---|
| `install-remote.sh` | **一键安装**（Komari 风格）：从 GitHub Release 下载 → 校验 SHA256 → 调用下面的脚本。用法见 `docs/DEPLOY.md` 第 2 节 |
| `install.sh` | 一步式：下载（或指定本地文件）→ 校验 SHA256 → 调用下面的安装脚本 |
| `install-server.sh` | 安装/升级服务端；`--uninstall` 卸载；`--purge` 连数据一起删 |
| `install-agent.sh` | 安装/升级 Agent；`--uninstall` / `--purge` 同上 |
| `package.sh` | 打发布包：产出 `dist/` 下 4 个二进制 + 2 个安装脚本 + `SHA256SUMS`（可直接拖进 GitHub Release） |

发布流程与一键安装命令见 [`docs/DEPLOY.md`](../docs/DEPLOY.md)；
CI（`.github/workflows/release.yml`）在 push `v*` tag 时自动构建并发布 Release。

## 服务端

```bash
# 方式零（需要先发布到 GitHub，见 docs/DEPLOY.md 第 1 节）
curl -fsSL https://raw.githubusercontent.com/你/probe/main/deploy/install-remote.sh \
  | sudo sh -s -- server

# 方式一：本地已有二进制
scp dist/probe-server-linux-amd64 root@vps:/root/
ssh root@vps
sh install-server.sh              # 默认监听 127.0.0.1:25774

# 方式二：从发布地址下载并校验
sh install.sh server --url https://example.com/probe-server-linux-amd64 --sha256 <哈希>
```

> 两种架构都支持：脚本会按 `uname -m` 找 `-linux-amd64` 或 `-linux-arm64`
> （ARM 小鸡不用改名、也不用额外参数）。

安装后的位置（与 `docs/DESIGN.md` §5 一致）：

| 项目 | 路径 |
|---|---|
| 二进制 | `/usr/local/bin/probe-server` |
| 数据（SQLite） | `/var/lib/probe-server/probe.db`（目录 **0700**，由 systemd `StateDirectory=` 保证属主） |
| 单元 | `/etc/systemd/system/probe-server.service` |
| 运行用户 | `probe`（系统用户，无登录 shell，无任何 capability） |

取第一次启动的初始化码：

```bash
journalctl -u probe-server | grep setup_code
```

公网访问建议（也是唯一推荐的方式）：用 Caddy/nginx 反代并启用 TLS，把 `--trusted-proxy` 设为反代地址，
这样日志与操作记录里才是真实访客 IP。反代要**关闭响应缓冲**（SSE 需要）：

```caddy
monitor.example.com {
    reverse_proxy 127.0.0.1:25774 {
        flush_interval -1
    }
}
```

## Agent

```bash
sh install.sh agent --file ./probe-agent-linux-amd64 --sha256 <哈希> \
   --server https://monitor.example.com --token pba_xxx
```

| 项目 | 路径 |
|---|---|
| 二进制 | `/usr/local/bin/probe-agent` |
| Token | `/etc/probe-agent/token`（**0600**，脚本用 printf 写入，不带多余换行） |
| 流量状态 | `/var/lib/probe-agent/state.json`（0600） |
| 单元 | `/etc/systemd/system/probe-agent.service` |
| 运行用户 | `probe-agent`（系统用户；不需要 root、不需要 capability，只出站连接） |

- **Token 绝不进命令行**（`ExecStart` 里是 `--token-file`），否则同机任何用户用 `ps` 就能看到。
- 远端地址必须是 `https://`（脚本会拒绝明文；只有 `127.0.0.1`/`localhost` 例外，便于本机自测）。
- 自检（不连服务端，只读一次 `/proc` 并打印 JSON）：

```bash
probe-agent --print-json --root /
```

## 卸载

```bash
sh install-server.sh --uninstall          # 保留 /var/lib/probe-server
sh install-server.sh --uninstall --purge  # 连数据与用户一起删
sh install-agent.sh  --uninstall --purge
```

## 为什么脚本这么"啰嗦"

- **不用 `curl | sh`**：一步式脚本先下载到文件、校验 SHA256、再执行；哈希不符直接失败并保留文件供检查。
- **不用 root 跑服务**：两个单元都是专用系统用户 + `CapabilityBoundingSet=`（清空）。
- **文件系统防护**：`ProtectSystem=strict` + `ReadWritePaths=` 只放开自己的数据目录，
  `ProtectHome`/`PrivateTmp`/`ProtectKernel*`/`ProtectProc=invisible` 全部打开。
- **不把 SQLite 放到网络盘**：服务端脚本会检测 `nfs/cifs/fuse/overlayfs` 并拒绝安装
  （SQLite 的锁依赖本地文件系统，放网络盘会丢数据）。
- **卸载默认不删数据**：只有显式 `--purge` 才删；避免"手滑一条命令把历史删了"。

这些约束都有自动化测试守着（`deploy/deploy_test.go`）：换行符必须是 LF、单元必须包含全部加固项、
Token 不能出现在命令行、脚本里不能出现 `| sh`、安装脚本必须校验哈希等。
脚本本身只能在 Linux 上执行，因此在这台 Windows 开发机上无法实跑；
在 Linux 上可以先做静态检查：

```bash
sh -n deploy/install.sh deploy/install-server.sh deploy/install-agent.sh
systemd-analyze verify /etc/systemd/system/probe-server.service   # 安装后
```
