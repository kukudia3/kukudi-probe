// Package deploy 只包含安装脚本的"护栏测试"。
//
// 这些脚本只能在 Linux 上真正执行，而开发机是 Windows，所以这里用测试守住
// 那些"在 Linux 上才会暴露、但一眼能查出来"的坑：
//   - CRLF 换行（会让 shebang 变成 "bad interpreter"）；
//   - systemd 单元缺少关键加固项 / 用 root 跑 / 把 Token 写进命令行；
//   - 脚本里出现 `curl | sh` 这类"把网络内容直接执行"的写法。
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readScript(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("读取 %s: %v", name, err)
	}
	return string(data)
}

func TestScriptsUseUnixLineEndings(t *testing.T) {
	// install-remote.sh 也在列表里：它要能 `curl | sh` 跑起来，同 CRLF 就会报
	// "bad interpreter" 或语法错误。
	for _, name := range []string{"install.sh", "install-server.sh", "install-agent.sh", "install-remote.sh", "package.sh", "doctor.sh"} {
		content := readScript(t, name)
		if strings.Contains(content, "\r\n") || strings.Contains(content, "\r") {
			t.Errorf("%s 含 CR：Linux 上 shebang 会失效（必须是 LF）", name)
		}
		if !strings.HasPrefix(content, "#!/bin/sh") {
			t.Errorf("%s 必须以 #!/bin/sh 开头", name)
		}
	}
}

func TestScriptsAreStrictAndFailFast(t *testing.T) {
	for _, name := range []string{"install.sh", "install-server.sh", "install-agent.sh", "install-remote.sh", "package.sh", "doctor.sh"} {
		content := readScript(t, name)
		if !strings.Contains(content, "set -eu") {
			t.Errorf("%s 缺少 set -eu（出错要立刻停，不能带病继续）", name)
		}
	}
}

// codeOnly 去掉整行注释，只留真正的代码。
//
// 守卫测试针对的是"脚本会做什么"，而注释/帮助文本里出现某个写法（例如
// "不会用 curl -k"）不代表脚本真的那么做。
func codeOnly(content string) string {
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// 绝不允许"把网络上的内容直接交给 shell 执行"。
//
// install-remote.sh 不参与这一条：它的**用法本身**就是 `curl … | sudo sh -s -- server`
// （帮助文本里会原样出现这个写法）。它"执行远程内容"的方式由
// TestRemoteInstallerVerifiesBeforeExecuting 单独管住：必须先落盘、校验 SHA256、再执行。
func TestScriptsNeverPipeRemoteContentToShell(t *testing.T) {
	for _, name := range []string{"install.sh", "install-server.sh", "install-agent.sh", "package.sh"} {
		content := readScript(t, name)
		for _, pattern := range []string{"| sh", "| bash", "|sh", "|bash", "eval \"$(curl", "eval \"$(wget"} {
			if strings.Contains(content, pattern) {
				t.Errorf("%s 里出现了把远程内容直接执行的写法：%q", name, pattern)
			}
		}
	}
}

// 一键安装脚本的安全底线：先下载到临时文件 → 校验 SHA256 → 才执行。
//
// 它是整个项目里唯一"从网络取程序并运行"的地方，所以规则写死：
//   - 必须有 sha256sum 校验，且校验失败必须退出；
//   - 不允许 curl -k / --insecure 之类的降级；
//   - 不允许校验之前 chmod +x 或执行；
//   - 临时目录必须清理（trap）。
func TestRemoteInstallerVerifiesBeforeExecuting(t *testing.T) {
	content := readScript(t, "install-remote.sh")

	must := []string{
		"set -eu",
		"sha256sum",
		"SHA256SUMS",
		"mktemp -d",
		"trap 'rm -rf",
		"die \"$file 校验失败", // 校验失败必须中止
	}
	for _, needle := range must {
		if !strings.Contains(content, needle) {
			t.Errorf("install-remote.sh 缺少 %q", needle)
		}
	}

	forbidden := []string{"curl -k", "curl --insecure", "wget --no-check-certificate", "sh -c \"$("}
	for _, needle := range forbidden {
		if strings.Contains(codeOnly(content), needle) {
			t.Errorf("install-remote.sh 不该出现 %q（会绕过或削弱校验）", needle)
		}
	}

	// 校验必须发生在 chmod/执行之前。
	verifyAt := strings.Index(content, "校验 SHA256")
	execAt := strings.Index(content, "开始安装")
	if verifyAt < 0 || execAt < 0 || verifyAt > execAt {
		t.Error("install-remote.sh 必须先校验再执行")
	}
	// 下载目标必须是临时目录，不能直接写进 /usr/local/bin。
	if strings.Contains(content, "-o /usr/local/bin/") {
		t.Error("install-remote.sh 不该把未校验的文件直接写进 /usr/local/bin")
	}
}

// 一键安装脚本必须支持"换源"，否则 GitHub 连不上的机器没法装；
// 并且必须支持指定版本（可回滚、可复现）。
func TestRemoteInstallerSupportsMirrorAndVersion(t *testing.T) {
	content := readScript(t, "install-remote.sh")
	for _, needle := range []string{"--base-url", "--version", "releases/latest/download", "releases/download"} {
		if !strings.Contains(content, needle) {
			t.Errorf("install-remote.sh 缺少 %q", needle)
		}
	}
	// 架构探测必须覆盖 amd64 与 arm64。
	for _, needle := range []string{"x86_64", "aarch64", "amd64", "arm64"} {
		if !strings.Contains(content, needle) {
			t.Errorf("install-remote.sh 缺少架构探测 %q", needle)
		}
	}
	// `curl | sh -s -- server` 时 $0 是 "sh"，不能拿它当脚本路径去读用法。
	if !strings.Contains(content, `[ -r "$0" ]`) {
		t.Error("install-remote.sh 打印用法前必须判断 $0 是不是可读文件（管道执行时 $0 是 sh）")
	}
}

// 发布资产清单必须与一键安装脚本的期望一致，否则发出来的 release 装不上。
func TestReleaseAssetsMatchRemoteInstaller(t *testing.T) {
	remote := readScript(t, "install-remote.sh")
	for _, asset := range []string{
		"probe-${ROLE}-linux-${ARCH}", // 4 个二进制
		"SHA256SUMS",
		"install-${ROLE}.sh",
	} {
		if !strings.Contains(remote, asset) {
			t.Errorf("install-remote.sh 期望的资产 %q 缺失", asset)
		}
	}

	// 打包脚本与 CI 必须产出同样的文件名。
	pkg := readScript(t, "package.sh")
	for _, asset := range []string{
		"probe-server-linux-amd64", "probe-server-linux-arm64",
		"probe-agent-linux-amd64", "probe-agent-linux-arm64",
		"install-server.sh", "install-agent.sh", "SHA256SUMS",
	} {
		if !strings.Contains(pkg, asset) {
			t.Errorf("package.sh 没有产出 %q", asset)
		}
	}

	ci, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("读取 CI 配置: %v", err)
	}
	ciText := string(ci)
	for _, asset := range []string{"probe-server-linux-amd64", "probe-server-linux-arm64",
		"probe-agent-linux-amd64", "probe-agent-linux-arm64", "install-server.sh", "install-agent.sh", "SHA256SUMS"} {
		if !strings.Contains(ciText, asset) {
			t.Errorf("CI 发布资产缺少 %q", asset)
		}
	}
	if !strings.Contains(ciText, "go test ./... -count=1") {
		t.Error("CI 必须在发布前跑测试（不能把没测过的二进制发出去）")
	}
	if strings.Contains(ciText, "secrets.") {
		t.Error("CI 里不该引用任何 secrets（本项目不需要）")
	}
}

// Agent 的 Token 只能走文件，绝不能出现在 systemd 的 ExecStart 里
// （否则同机任何用户都能用 ps 看到）。
// 安装脚本必须同时认 amd64 与 arm64。
//
// ARM 小鸡（Oracle Ampere / Hetzner CAX / 树莓派）拿到的是 -linux-arm64；
// 早期版本硬编码 -linux-amd64，导致 ARM 上直接报"当前目录没有 …"、装不上。
func TestInstallersHandleBothArchitectures(t *testing.T) {
	for _, name := range []string{"install.sh", "install-server.sh", "install-agent.sh", "install-remote.sh"} {
		content := readScript(t, name)
		if !strings.Contains(content, "aarch64") && !strings.Contains(content, "arm64") {
			t.Errorf("%s 认不出 arm64（ARM 小鸡会装不上）", name)
		}
		if !strings.Contains(content, "amd64") {
			t.Errorf("%s 认不出 amd64", name)
		}
	}
	// 角色脚本必须按"本机架构"查找二进制，不能写死一个架构。
	for _, name := range []string{"install-server.sh", "install-agent.sh"} {
		if content := readScript(t, name); !strings.Contains(content, "-linux-${HOST_ARCH}") {
			t.Errorf("%s 应当优先查找 -linux-${HOST_ARCH} 的二进制", name)
		}
	}
}

// 数据目录必须是 0700：里面有 SQLite 的 -wal/-shm，WAL 含最近的提交页
// （密码哈希、会话、审计日志），只允许服务账号进入。
func TestServerScriptTightensDataDirPermissions(t *testing.T) {
	content := readScript(t, "install-server.sh")
	if !strings.Contains(content, `chmod 0700 "${DATA_DIR}"`) {
		t.Error("install-server.sh 应当把数据目录设成 0700")
	}
	if strings.Contains(content, `chmod 0750 "${DATA_DIR}"`) {
		t.Error("install-server.sh 不该把数据目录设成 0750（同组用户可读 WAL）")
	}
}

// 自检脚本（doctor.sh）必须覆盖"404 的四种成因"，否则用户拿不到有效结论。
func TestDoctorCoversEvery404Cause(t *testing.T) {
	content := readScript(t, "doctor.sh")

	must := []string{
		"api.github.com/repos/",     // 仓库是否存在/是否私有
		"raw.githubusercontent.com", // raw 路径与分支
		"releases/latest",           // Release 是否已发布
		"releases/latest/download",  // 资产是否齐
		"SHA256SUMS",                // 资产清单
		"default_branch",            // 默认分支可能是 master
		"probe/deploy/",             // 常见错误：仓库里多套了一层目录
		"--base-url",                // 不用 GitHub 的替代方案
		"Change visibility",         // 私有 → 公开的具体路径
		"Draft",                     // 草稿 Release 不算发布
		"1.1.1.1",                   // DNS 被污染时的修法
	}
	for _, needle := range must {
		if !strings.Contains(content, needle) {
			t.Errorf("doctor.sh 缺少 %q（少一项就有一类 404 查不出来）", needle)
		}
	}

	// 自检必须只读：不许出现任何写操作/安装动作。
	forbidden := []string{"rm -rf", "useradd", "systemctl", "install -m", "> /usr/", "chmod"}
	for _, needle := range forbidden {
		if strings.Contains(content, needle) {
			t.Errorf("doctor.sh 应当是只读自检，不该出现 %q", needle)
		}
	}

	// 必须有明确的退出码语义（0=可以装 / 1=有阻断原因）。
	if !strings.Contains(content, `exit "$FAIL"`) {
		t.Error("doctor.sh 应当用退出码反映自检结果")
	}
}

func TestAgentUnitKeepsTokenOutOfCommandLine(t *testing.T) {
	content := readScript(t, "install-agent.sh")
	if !strings.Contains(content, "--token-file ${TOKEN_FILE}") {
		t.Error("Agent 的 ExecStart 应当用 --token-file")
	}
	if strings.Contains(content, "--token ${TOKEN}") {
		t.Error("Agent 的 ExecStart 不该出现 --token 明文")
	}
	if !strings.Contains(content, "chmod 0600 \"${TOKEN_FILE}\"") {
		t.Error("Token 文件应当是 0600")
	}
	if !strings.Contains(content, "printf '%s' \"${TOKEN}\" > \"${TOKEN_FILE}\"") {
		t.Error("Token 应当用 printf 写入（避免多余换行）")
	}
}

// 服务端单元要默认信任本机反代的转发头。
//
// 不配的话，审计日志与登录限流看到的全是 127.0.0.1：反代（Caddy/nginx）和
// Cloudflare 隧道（cloudflared）都在本机，TCP 对端恒为回环地址，于是登录限流
// 变成"所有人共用一个桶"，形同虚设。探针只监听 127.0.0.1、外部连不进来，
// 而外部直连的请求对端不是回环地址、伪造的转发头照样被忽略，所以信任回环不亏安全。
//
// 用 Environment= 而不是把 --trusted-proxy 塞进 ExecStart：有人用 drop-in 整体
// 覆盖 ExecStart 时（换监听地址、加参数都会那么干），主单元里的命令行参数会被
// 整个忽略，Environment= 却依然生效。
func TestServerUnitTrustsLoopbackProxy(t *testing.T) {
	content := readScript(t, "install-server.sh")

	if !strings.Contains(content, "Environment=PROBE_TRUSTED_PROXY=${TRUSTED_PROXY}") {
		t.Error("install-server.sh 的单元应当用 Environment=PROBE_TRUSTED_PROXY 传可信代理")
	}
	if got := assignmentValue(t, content, "TRUSTED_PROXY"); got != "127.0.0.1" {
		t.Errorf("TRUSTED_PROXY 默认值 = %q，期望 127.0.0.1", got)
	}
	// 必须留出改的口子：反代在另一台机器时用户得能换掉，或干脆关掉。
	if !strings.Contains(content, "--trusted-proxy)") {
		t.Error("install-server.sh 应当支持 --trusted-proxy 覆盖默认值")
	}
}

// Agent 的单元里不能出现 ProcSubset=pid。
//
// 它的语义（内核 subset=pid）是把 /proc 下所有与进程无关的顶层文件藏起来，
// 包括 /proc/stat、/proc/meminfo、/proc/cpuinfo、/proc/uptime、/proc/net/dev、
// /proc/mounts —— 而 Agent 的采集器读的正是这些。后果特别隐蔽：
//
//   - Info() 对 /proc 读失败是容忍的，所以 hello 照样发得出去，
//     面板上「系统」（来自 /etc/os-release）、出口地址、Agent 版本都有值；
//   - Sample() 对 /proc/stat、/proc/meminfo 是硬错误，于是 metrics 一条都发不出，
//     所有图表永远"暂无数据"。
//
// 排查成本极高（节点看着在线，日志里才有一行"采集失败"），所以在 CI 里钉死。
func TestAgentUnitKeepsProcReadable(t *testing.T) {
	content := readScript(t, "install-agent.sh")
	// 只匹配真正的指令行，别被解释性注释（里面会提到这个名字）误伤。
	if regexp.MustCompile(`(?m)^\s*ProcSubset\s*=\s*pid\s*$`).MatchString(content) {
		t.Error("install-agent.sh 不能给 Agent 加 ProcSubset=pid：它藏掉 /proc/stat 等系统级文件，采集会全部失败")
	}
	// ProtectProc 只影响"别的进程可不可见"，系统级文件照常可读，留着没问题。
	if !strings.Contains(content, "ProtectProc=invisible") {
		t.Error("install-agent.sh 应当保留 ProtectProc=invisible（它不影响系统级 /proc 文件）")
	}
}

// systemd 单元的加固项：少一条都是安全性的净损失。
func TestUnitsCarrySecurityHardening(t *testing.T) {
	required := []string{
		"NoNewPrivileges=yes",
		"PrivateTmp=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectControlGroups=yes",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"RestrictNamespaces=yes",
		"RestrictSUIDSGID=yes",
		"LockPersonality=yes",
		"MemoryDenyWriteExecute=yes",
		"SystemCallArchitectures=native",
		"SystemCallFilter=@system-service",
		"CapabilityBoundingSet=",
		"AmbientCapabilities=",
		"UMask=0077",
		"StateDirectory=",
		"Restart=",
	}
	for _, name := range []string{"install-server.sh", "install-agent.sh"} {
		content := readScript(t, name)
		for _, needle := range required {
			if !strings.Contains(content, needle) {
				t.Errorf("%s 的 systemd 单元缺少加固项 %s", name, needle)
			}
		}
		if strings.Contains(content, "User=root") {
			t.Errorf("%s 不该用 root 运行", name)
		}
	}
}

// 单元文件必须能被 systemd 解析：每个非空行要么是 [Section]，要么是 Key=Value，
// 且 [Unit]/[Service]/[Install] 三个段都在。
func TestSystemdUnitsAreSyntacticallySane(t *testing.T) {
	for _, name := range []string{"install-server.sh", "install-agent.sh"} {
		content := readScript(t, name)
		unit := extractHeredoc(t, content)
		if unit == "" {
			t.Fatalf("%s 里没有找到 systemd 单元内容", name)
		}

		sections := map[string]bool{}
		for _, line := range strings.Split(unit, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				sections[trimmed] = true
				continue
			}
			if !strings.Contains(trimmed, "=") {
				t.Errorf("%s 的单元里有无法解析的行：%q", name, trimmed)
			}
		}
		for _, want := range []string{"[Unit]", "[Service]", "[Install]"} {
			if !sections[want] {
				t.Errorf("%s 的单元缺少 %s 段", name, want)
			}
		}
		if !strings.Contains(unit, "WantedBy=multi-user.target") {
			t.Errorf("%s 的单元缺少 WantedBy", name)
		}
	}
}

// extractHeredoc 取出 `cat > "${UNIT_PATH}" <<EOF ... EOF` 之间的内容。
func extractHeredoc(t *testing.T, content string) string {
	t.Helper()
	start := strings.Index(content, "<<EOF\n")
	if start < 0 {
		return ""
	}
	start += len("<<EOF\n")
	end := strings.Index(content[start:], "\nEOF")
	if end < 0 {
		return ""
	}
	return content[start : start+end]
}

// 卸载必须三思：默认保留数据，只有 --purge 才删。
func TestUninstallKeepsDataByDefault(t *testing.T) {
	for _, name := range []string{"install-server.sh", "install-agent.sh"} {
		content := readScript(t, name)
		if !strings.Contains(content, "--purge") {
			t.Errorf("%s 应当支持 --purge（且只有它才删数据）", name)
		}
		if !strings.Contains(content, "数据保留") && !strings.Contains(content, "保留在") {
			t.Errorf("%s 的卸载提示应当说明数据是否保留", name)
		}
	}
}

// ICMP 探测要开原始套接字，Agent 需要 CAP_NET_RAW —— 但只允许这一个能力。
//
// 这条用例守两件事：
//   - 服务端单元必须保持**零能力**（它不需要任何原始套接字，多给一个都是净损失）；
//   - Agent 单元恰好是 CAP_NET_RAW，不许顺手加上 CAP_NET_ADMIN 之类的"顺便"能力
//     （有了 CAP_NET_ADMIN 就能改路由与防火墙，那才是真正危险的组合）。
//
// 放开的理由与边界写在脚本的注释里（也必须在 docs/SECURITY.md 里有对应说明）。
func TestUnitsGrantOnlyCapNetRawToAgent(t *testing.T) {
	agentScript := readScript(t, "install-agent.sh")
	serverUnit := extractHeredoc(t, readScript(t, "install-server.sh"))
	agentUnit := extractHeredoc(t, agentScript)

	cases := []struct {
		name string
		unit string
		want string
	}{
		{"install-agent.sh", agentUnit, "CAP_NET_RAW"},
		{"install-server.sh", serverUnit, ""},
	}
	for _, tc := range cases {
		for _, key := range []string{"CapabilityBoundingSet", "AmbientCapabilities"} {
			got, count := unitDirective(t, tc.name, tc.unit, key)
			if count != 1 {
				t.Errorf("%s 的单元里 %s= 出现了 %d 次，期望恰好 1 次", tc.name, key, count)
			}
			if got != tc.want {
				t.Errorf("%s 的 %s = %q，期望 %q", tc.name, key, got, tc.want)
			}
		}
	}

	// 除了 CAP_NET_RAW，任何 CAP_* 都不许出现在这两行里。
	for _, line := range strings.Split(agentUnit, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "CapabilityBoundingSet=") && !strings.HasPrefix(trimmed, "AmbientCapabilities=") {
			continue
		}
		for _, cap := range strings.Fields(trimmed[strings.Index(trimmed, "=")+1:]) {
			if cap != "CAP_NET_RAW" {
				t.Errorf("Agent 单元不该授予 %s（只有 ICMP 需要的 CAP_NET_RAW 是允许的）", cap)
			}
		}
	}

	// 放宽权限必须写明理由，否则下一个人只会看到"多了一个能力"。
	if !strings.Contains(agentScript, "ICMP") || !strings.Contains(agentScript, "CAP_NET_RAW") {
		t.Error("install-agent.sh 必须说明为什么需要 CAP_NET_RAW（ICMP 原始套接字）")
	}
	security, err := os.ReadFile(filepath.Join("..", "docs", "SECURITY.md"))
	if err != nil {
		t.Fatalf("读取 docs/SECURITY.md: %v", err)
	}
	doc := string(security)
	for _, needle := range []string{"CAP_NET_RAW", "ICMP", "NoNewPrivileges"} {
		if !strings.Contains(doc, needle) {
			t.Errorf("docs/SECURITY.md 应当说明这次权限变化（缺少 %q）", needle)
		}
	}
}

// unitDirective 取单元里 `Key=value` 的值（返回最后一次的值与出现次数）。
func unitDirective(t *testing.T, name, unit, key string) (string, int) {
	t.Helper()
	value, count := "", 0
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key+"=") {
			continue
		}
		count++
		value = strings.TrimSpace(trimmed[len(key)+1:])
	}
	return value, count
}

// 服务端脚本要挡住"把 SQLite 放到网络文件系统上"这种会丢数据的部署。
func TestServerScriptRejectsNetworkFilesystem(t *testing.T) {
	content := readScript(t, "install-server.sh")
	for _, needle := range []string{"nfs", "cifs", "SQLite"} {
		if !strings.Contains(strings.ToLower(content), strings.ToLower(needle)) {
			t.Errorf("install-server.sh 应当检查/说明 %s", needle)
		}
	}
}

// install.sh 必须强制校验下载内容的哈希（或明确警告）。
func TestOneShotInstallerVerifiesChecksum(t *testing.T) {
	content := readScript(t, "install.sh")
	if !strings.Contains(content, "sha256sum") {
		t.Error("install.sh 应当用 sha256sum 校验下载的二进制")
	}
	if !strings.Contains(content, "哈希不匹配") {
		t.Error("install.sh 应当在哈希不匹配时明确报错")
	}
	if !strings.Contains(content, "--proto '=https'") {
		t.Error("install.sh 应当限制只走 https 下载")
	}
}

// 页面上给出的 Agent 安装命令，必须和 install-remote.sh 指向同一个仓库。
//
// 那条命令是 app.js 用 INSTALL_REPO / INSTALL_REF 拼出来的（见 showToken），
// 而安装脚本靠 install-remote.sh 里的 DEFAULT_GITHUB / DEFAULT_REF 找 release。
// 两处一旦漂移，用户拿到的就是一条 404 的命令 —— 偏偏 Token 只显示一次，
// 关掉对话框就只能重新生成，代价全在用户身上，所以在 CI 里钉死。
func TestFrontendInstallCommandMatchesRemoteInstaller(t *testing.T) {
	sh := readScript(t, "install-remote.sh")
	repo := assignmentValue(t, sh, "DEFAULT_GITHUB")
	ref := assignmentValue(t, sh, "DEFAULT_REF")

	data, err := os.ReadFile(filepath.Join("..", "web", "app.js"))
	if err != nil {
		t.Fatalf("读取 web/app.js: %v", err)
	}
	js := string(data)

	if got := assignmentValue(t, js, "INSTALL_REPO"); got != repo {
		t.Errorf("app.js 的 INSTALL_REPO = %q，install-remote.sh 的 DEFAULT_GITHUB = %q，两者必须一致", got, repo)
	}
	if got := assignmentValue(t, js, "INSTALL_REF"); got != ref {
		t.Errorf("app.js 的 INSTALL_REF = %q，install-remote.sh 的 DEFAULT_REF = %q，两者必须一致", got, ref)
	}
}

// assignmentValue 取 `NAME="值"`（sh）或 `var NAME = '值'`（js）里的值。
func assignmentValue(t *testing.T, content, name string) string {
	t.Helper()
	match := regexp.MustCompile(name + `\s*=\s*["']([^"']+)["']`).FindStringSubmatch(content)
	if match == nil {
		t.Fatalf("在文本里找不到 %s 的赋值", name)
	}
	return match[1]
}

// install-agent.sh 写进 systemd 单元的 --interval 必须是带单位的 duration。
//
// 这个值直接拼进 ExecStart，而 probe-agent 的 -interval 是 flag.Duration：
// 裸数字 "1" 会让服务每次启动都失败（invalid value "1" for flag -interval），
// 而安装脚本只会说"服务没有起来"，真因得翻 journalctl 才看得到 ——
// 曾经默认值就是裸数字 "1"，等于这个 Agent 从来没装成功过。
func TestAgentUnitIntervalIsDuration(t *testing.T) {
	content := readScript(t, "install-agent.sh")

	value := assignmentValue(t, content, "INTERVAL")
	duration := regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h)$`)
	if !duration.MatchString(value) {
		t.Errorf("install-agent.sh 的 INTERVAL 默认值 = %q，必须是带单位的 duration（如 1s）", value)
	}

	// 上面的值必须真的被拼进 ExecStart，否则查它没有意义。
	if !strings.Contains(content, `--interval ${INTERVAL}`) {
		t.Error("install-agent.sh 的单元里应当用 --interval ${INTERVAL} 引用这个值")
	}
	// 用户手抄 `--interval 5` 这种裸数字时要能自动补单位，否则装完还是个坏服务。
	if !strings.Contains(content, `INTERVAL="${INTERVAL}s"`) {
		t.Error("install-agent.sh 应当把裸数字的 --interval 自动补成 duration（5 → 5s）")
	}
}
