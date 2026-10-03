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

// doctor.sh 的 ok/bad/warn/info 是"printf 单槽 + $*"的包装：
//
//	bad() { printf '  \033[31m[!!]\033[0m %s\n' "$*"; }
//
// 所以调用处写成 `bad "……%s =%s" "$a" "$b"` 时，%s 会被**原样打印**出来，a/b 反而被
// $* 拼到句尾——真实出现过：`Release 资产不齐：SHA256SUMS=%s probe-server-linux-amd64=%s 200 404`，
// 用户读不出"哪个资产是 200、哪个是 404"，而这恰好是 doctor 最该说清的一句话。
//
// 因此调用处的第一个参数必须是"成品句子"：要插值就用 ${var} 让 shell 展开，不留 printf 动词。
// （这里只认这四个包装函数；脚本里直接用 printf 的地方不在此列，它们自带格式串是正常的。）
func TestDoctorMessagesArePlainSentences(t *testing.T) {
	content := readScript(t, "doctor.sh")
	callRe := regexp.MustCompile(`^[ \t]*(?:ok|bad|warn|info)[ \t]+"([^"]*)"`)
	verbRe := regexp.MustCompile(`%[-+ #0-9.*]*[a-zA-Z]`)

	checked := 0
	for i, line := range strings.Split(content, "\n") {
		m := callRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		checked++
		if verb := verbRe.FindString(m[1]); verb != "" {
			t.Errorf("doctor.sh:%d 的提示里带了 printf 动词 %q —— ok/bad/warn/info 是 printf 单槽包装，"+
				"动词会原样打印、后面的实参被拼到句尾；请写成一句人话（用 ${var} 插值）：%s",
				i+1, verb, strings.TrimSpace(line))
		}
	}
	// 判别力自检：文案全被删光/改名时这条用例不该"永远绿"。
	if checked < 10 {
		t.Errorf("只认出 %d 条 ok/bad/warn/info 提示，正则或脚本结构变了，本用例已失去判别力", checked)
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

// systemd 的 StateDirectory= 默认模式是 0755，而 systemd **每次启动**都会把目录
// chmod 回该值（已存在的目录也一样，-EEXIST 不豁免）⇒ 只写 StateDirectory= 的话，
// 安装脚本设好的 0700/0750 在第一次重启后就被抹掉。目录里是 SQLite 的 -wal/-shm
// （最近的提交页：密码哈希、会话、审计），所以模式必须显式钉在单元里。
//
// 兼容性：StateDirectory= 与 StateDirectoryMode= 都是 systemd v235 引入的
// （v234 的 systemd.exec 里两者都不存在），而本项目本来就在用 StateDirectory=，
// 所以这一行不会抬高最低 systemd 版本。
func TestUnitsPinStateDirectoryMode(t *testing.T) {
	cases := []struct {
		script string
		want   string
	}{
		{"install-server.sh", "0700"},
		{"install-agent.sh", "0750"},
	}
	for _, tc := range cases {
		unit := extractHeredoc(t, readScript(t, tc.script))
		got, count := unitDirective(t, tc.script, unit, "StateDirectoryMode")
		if count != 1 {
			t.Errorf("%s 的单元里 StateDirectoryMode= 出现 %d 次，期望恰好 1 次", tc.script, count)
		}
		if got != tc.want {
			t.Errorf("%s 的 StateDirectoryMode = %q，期望 %q", tc.script, got, tc.want)
		}
		if dir, n := unitDirective(t, tc.script, unit, "StateDirectory"); n != 1 || dir == "" {
			t.Errorf("%s 的单元应当有且只有一条 StateDirectory=（StateDirectoryMode 才有作用对象）", tc.script)
		}
	}
}

// 走 --url 下载可执行文件时，--sha256 必须是强制的：头注释写"强制"而实现是
// "警告后继续"，正是最容易被误当成已加固的那种不一致。
// 本地文件（--file）保留宽容（它没经过网络），但要显式开关 --allow-no-hash 才能跳过。
func TestOneShotInstallerRequiresChecksumWhenDownloading(t *testing.T) {
	content := readScript(t, "install.sh")
	for _, needle := range []string{"--allow-no-hash", "ALLOW_NO_HASH", "走 --url 时必须提供 --sha256"} {
		if !strings.Contains(content, needle) {
			t.Errorf("install.sh 缺少 %q", needle)
		}
	}
	// 拒绝必须发生在 chmod / 执行之前，否则文件已经可能被用上了。
	dieAt := strings.Index(content, "走 --url 时必须提供 --sha256")
	chmodAt := strings.Index(content, `chmod 0755 "${TARGET}"`)
	if dieAt < 0 || chmodAt < 0 || dieAt > chmodAt {
		t.Error("install.sh 必须在 chmod / 执行之前拒绝没给哈希的下载")
	}
}

// install.sh 只能从"可信目录"里取并执行角色脚本：目录对同组/其他用户可写时，
// 别人可以先放一个同名 install-<角色>.sh，等 root 来执行（信任链断点）。
//
// 下载/拷贝也必须落到 mktemp 新建的文件上：目标名可预测，而 cp 与 curl -o 会跟随
// 符号链接 —— 在他人可写的目录里预置同名符号链接，就能让 root 覆写任意路径。
func TestOneShotInstallerUsesTrustedRoleScript(t *testing.T) {
	content := readScript(t, "install.sh")
	must := []string{
		`mktemp "./.${BIN}-linux-${HOST_ARCH}.XXXXXX"`, // 暂存文件必须自己新建
		`mv -f "${STAGE}" "${TARGET}"`,                 // rename 替换链接本身
		`?????w*|????????w*`,                           // 同组/其他用户可写的目录直接拒绝
		`sh "./${ROLE_SCRIPT}"`,                        // 只按解析出来的名字执行
	}
	for _, needle := range must {
		if !strings.Contains(content, needle) {
			t.Errorf("install.sh 缺少 %q", needle)
		}
	}
	if strings.Contains(content, "sh ./install-") {
		t.Error("install.sh 不该用裸相对路径执行角色脚本（当前目录可能是别人可写的）")
	}
}

// 操作员传进来的值会被原样写进 root 拥有的 systemd 单元：systemd 按**行**解析单元
// 文件 ⇒ 值里带换行就能插入任意指令；Exec 行还会按空白切词、认引号分组、展开 $VAR
// ⇒ 值里出现空白/引号/反引号就能改动这一行的 argv（例如追加 --insecure-skip-verify）。
// 两个角色脚本都必须在写单元**之前**把这些值挡掉。
func TestInstallerScriptsValidateOperatorInput(t *testing.T) {
	server := readScript(t, "install-server.sh")
	agent := readScript(t, "install-agent.sh")

	checks := []struct{ name, content, needle string }{
		{"install-server.sh", server, "*[!0-9A-Fa-f:.,/]*"},                                        // trusted-proxy 的字符集（换行/引号/非 ASCII 都在外面）
		{"install-server.sh", server, `*[!0-9A-Fa-f:.,/]*) die "--trusted-proxy 只接受 IP / CIDR 列表`}, // 而且真的 die，不是"匹配了就放过"
		{"install-agent.sh", agent, "*[!0-9a-zA-Z.]*"},                                             // interval 只许 duration 字符
		{"install-agent.sh", agent, "tr -d 'A-Za-z0-9:/?#@!+,;=%._~&-'"},                           // server 的 URL 白名单
		{"install-agent.sh", agent, `[ "${leftover}" = "0" ] || die`},                              // 数剩下的字节（见下面的注释）
	}
	for _, c := range checks {
		if !strings.Contains(c.content, c.needle) {
			t.Errorf("%s 缺少输入校验 %q", c.name, c.needle)
		}
	}
	// 必须用 `wc -c` 数"还剩几个字节"，不能用 `[ -n "$(...)" ]`：
	// 命令替换会吃掉结尾的换行，`--server $'https://x\nExecStartPre=…'` 那种值
	// 在 -n 判断下会变成空串、被误判成合法，然后换行照样进单元。
	if strings.Contains(agent, `if [ -n "$(printf '%s' "${SERVER}"`) {
		t.Error("install-agent.sh 的 --server 校验不能用命令替换的 -n 判断（结尾换行会被吃掉）")
	}
	// 校验必须发生在写单元（heredoc）之前，否则等于没校验。
	beforeUnit := []struct{ name, needle string }{
		{"install-server.sh", "*[!0-9A-Fa-f:.,/]*"},
		{"install-agent.sh", "tr -d 'A-Za-z0-9:/?#@!+,;=%._~&-'"},
	}
	for _, c := range beforeUnit {
		content := readScript(t, c.name)
		check := strings.Index(content, c.needle)
		unit := strings.Index(content, "<<EOF")
		if check < 0 || unit < 0 || check > unit {
			t.Errorf("%s 的输入校验必须写在 systemd 单元之前（否则等于没校验）", c.name)
		}
	}
	// trusted-proxy 里的空白也要处理：Environment= 按空白切分赋值，
	// 带空格的列表会让后面的 CIDR 被静默丢掉。
	if !strings.Contains(server, `tr -d ' '`) {
		t.Error("install-server.sh 应当去掉 --trusted-proxy 值里的空白（否则第二个 CIDR 会被 systemd 静默丢掉）")
	}
}

// http 明文源与仓库 raw 兜底这两条"降低保证"的路径默认必须关着：
//   - http 源里 SHA256SUMS 与二进制同源 ⇒ 中间人可以同时替换两者，校验形同虚设；
//   - raw 兜底执行的安装脚本没有哈希校验（只有 TLS + shebang 检查）却以 root 执行。
func TestRemoteInstallerInsecurePathsRequireOptIn(t *testing.T) {
	content := readScript(t, "install-remote.sh")
	must := []string{
		"--allow-insecure-base-url",
		"--allow-raw-installer",
		`--proto "=$_proto"`, // curl：禁止 http，也禁止 https 被跳转到 http
		"--https-only",       // wget：同上（BusyBox 不认时自动退回）
		// 两条"默认关闭"必须是这个方向的判断（把 -n 改成 -z 就等于默认放行）
		`[ -n "$ALLOW_INSECURE_BASE_URL" ] || die`,
		`[ -n "$ALLOW_RAW_INSTALLER" ] || die`,
	}
	for _, needle := range must {
		if !strings.Contains(content, needle) {
			t.Errorf("install-remote.sh 缺少 %q", needle)
		}
	}
	// 协议白名单必须在任何下载之前生效（BASE 算出来就查，不能等下载完）。
	allowAt := strings.Index(content, "--allow-insecure-base-url")
	fetchAt := strings.Index(content, `info "下载 SHA256SUMS"`)
	if allowAt < 0 || fetchAt < 0 || allowAt > fetchAt {
		t.Error("install-remote.sh 必须在下载之前就校验 --base-url 的协议")
	}
}

// 发布清单的三个产出点（Makefile / package.sh / CI）必须口径一致，并且都要自校验：
// 清单与文件不一致时拒绝发布 —— dist/SHA256SUMS 与二进制对不上过一次，
// 而客户端是 fail-closed（用户装不上），反过来"手工重算清单"会把可核对的凭据
// 变成对当前目录的背书。
func TestReleaseManifestIsSelfChecked(t *testing.T) {
	mk, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatalf("读取 Makefile: %v", err)
	}
	mkText := string(mk)
	for _, needle := range []string{
		"install-server.sh install-agent.sh", // 清单要带上安装脚本（glob 盖不到）
		"&& sha256sum -c SHA256SUMS",         // 生成后当场自校验（必须是真的命令，不是注释）
		`rm -rf -- "$(DIST)"`,                // 目标名带 -- 结尾，避免被当成开关
		`/*|*/../*`,                          // 拒绝绝对路径与上级目录
	} {
		if !strings.Contains(mkText, needle) {
			t.Errorf("Makefile 缺少 %q", needle)
		}
	}

	pkg := readScript(t, "package.sh")
	for _, needle := range []string{"sha256sum -c SHA256SUMS ||", "-X probe/internal/version.Version="} {
		if !strings.Contains(pkg, needle) {
			t.Errorf("package.sh 缺少 %q（版本要注入、清单要自校验）", needle)
		}
	}

	rel, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("读取 release.yml: %v", err)
	}
	relText := string(rel)
	for _, needle := range []string{"sha256sum -c SHA256SUMS", "--version"} {
		if !strings.Contains(relText, needle) {
			t.Errorf("release.yml 缺少发布自检 %q", needle)
		}
	}
}

// tag 名会进 make 变量 → -ldflags 的配方文本 → /bin/sh -c：含引号/反引号/分号
// 就能在 runner 上执行任意命令。所以 CI 必须**在构建之前**校验 tag 名的形状与字符集。
// 顺带把权限收紧写进测试：workflow 级只读，写权限只给需要创建 Release 的作业。
func TestReleaseWorkflowValidatesTagName(t *testing.T) {
	rel, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("读取 release.yml: %v", err)
	}
	text := string(rel)
	for _, needle := range []string{
		`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$`, // 形状（完整正则，写成 .* 就等于没校验）
		`*[!0-9A-Za-z.+-]*`, // 字符集兜底
		"persist-credentials: false",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("release.yml 缺少 %q", needle)
		}
	}
	checkAt := strings.Index(text, "校验 tag 名")
	buildAt := strings.Index(text, "make release")
	if checkAt < 0 || buildAt < 0 || checkAt > buildAt {
		t.Error("release.yml 必须在构建之前校验 tag 名")
	}
	if !strings.Contains(text, "permissions:\n  contents: read") {
		t.Error("release.yml 的 workflow 级权限应当是 contents: read")
	}
	if !strings.Contains(text, "\n      contents: write") {
		t.Error("release.yml 应当在作业级给 contents: write（而不是整个 workflow）")
	}
}

// ciStepRun 取出 ci.yml 里 id=<id> 那个步骤的 run: 脚本正文 —— 也就是真正会被
// shell 执行的那一段文本（整行注释已去掉）。
//
// 为什么必须按步骤取，而不是在整个 ci.yml 上做子串匹配：`go test -race` 这几个字
// 在**步骤名**（「竞态检查（go test -race，排除 internal/e2e）」）与文件头注释里
// 都出现过。对整份文件 Contains 的话，把真正的命令删掉、只在名字或注释里留一句话，
// 断言照样是绿的 —— 那等于把红灯藏起来，正是这条守卫测试最该防住的写法。
func ciStepRun(t *testing.T, yml, id string) string {
	t.Helper()
	// 步骤之间以 jobs.<job>.steps[] 那一级缩进（6 个空格 + "- "）分隔。
	for _, step := range strings.Split(yml, "\n      - ") {
		// id 可能在 "- " 那一行（如 checkout），也可能缩进在步骤内的第一行之后。
		if !strings.HasPrefix(step, "id: "+id+"\n") && !strings.Contains(step, "\n        id: "+id+"\n") {
			continue
		}
		at := strings.Index(step, "\n        run: |\n")
		if at < 0 {
			t.Fatalf("ci.yml 的步骤 %s 里找不到 run: | 脚本（步骤形状变了？）", id)
		}
		return codeOnly(step[at:])
	}
	t.Fatalf("ci.yml 里找不到 id=%s 的步骤（它被删了，或步骤形状变了）", id)
	return ""
}

// CI 的质量闸门必须有漏洞扫描与竞态检查，仓库里也要有依赖更新机器人。
// ci.yml 额外要求：只读权限、不引用任何凭据（它会跑 PR 里的代码）。
func TestCIHasVulnerabilityAndRaceGates(t *testing.T) {
	rel, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("读取 release.yml: %v", err)
	}
	if !strings.Contains(string(rel), "govulncheck@v1.8.0") {
		t.Error("release.yml 的质量闸门应当跑 govulncheck（漏洞扫描）")
	}

	ci, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("读取 ci.yml: %v", err)
	}
	ciText := string(ci)
	// 竞态闸门。断言落在 run: 脚本（会被 shell 执行的那段文本）上，不钉死整行命令：
	// 2026-10 的 ci.yml 把 `go test` 拆成了两条闸门（-race 排除 internal/e2e，
	// e2e 单独跑且不带 -race，原因见 ci.yml 文件头的那段说明），原来那句
	// `go test -race ./... -count=1` 因此不再存在 —— 但闸门本身必须还在，
	// 而且必须还是"排除 e2e 的竞态检查"。
	raceRun := ciStepRun(t, ciText, "race")
	for _, needle := range []string{
		"go test -race",    // 命令本身：把它删掉、只在步骤名里留一句话必须让这条红
		"-count=1",         // 关掉测试缓存（否则"跑过了"可能只是缓存命中）
		"go list ./...",    // 包列表现算，而不是写死在 workflow 里
		"'/internal/e2e$'", // internal/e2e 必须被排除：-race 下它必然撞单包 600 秒默认超时
	} {
		if !strings.Contains(raceRun, needle) {
			t.Errorf("ci.yml 的竞态检查步骤里缺少 %q", needle)
		}
	}
	// 拆出去 ≠ 删掉：e2e 必须仍然在 CI 里单独跑（拆分的目的是把 -race 用在该用的
	// 地方，不是把浏览器用例跳过去）。
	if e2eRun := ciStepRun(t, ciText, "e2e"); !strings.Contains(e2eRun, "go test ./internal/e2e/") {
		t.Error("ci.yml 必须单独跑 internal/e2e，而不是把它从 CI 里删掉")
	}
	for _, needle := range []string{
		"govulncheck@v1.8.0",
		"contents: read",
		"persist-credentials: false",
	} {
		if !strings.Contains(ciText, needle) {
			t.Errorf("ci.yml 缺少 %q", needle)
		}
	}
	if strings.Contains(ciText, "secrets.") {
		t.Error("ci.yml 里不该引用任何 secrets（它会跑 PR 里的代码）")
	}

	db, err := os.ReadFile(filepath.Join("..", ".github", "dependabot.yml"))
	if err != nil {
		t.Fatalf("读取 dependabot.yml: %v", err)
	}
	dbText := string(db)
	for _, needle := range []string{"package-ecosystem: gomod", "package-ecosystem: github-actions"} {
		if !strings.Contains(dbText, needle) {
			t.Errorf("dependabot.yml 缺少 %q", needle)
		}
	}
}

// .gitignore 必须盖住私钥/证书/环境文件与 SQLite 回滚日志：
// 一次 `git add -A` 就能把 TLS 私钥永久写进 git 历史（历史流量可解密 + 可冒充）。
func TestGitignoreCoversSecrets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".gitignore"))
	if err != nil {
		t.Fatalf("读取 .gitignore: %v", err)
	}
	text := string(data)
	for _, needle := range []string{"*.pem", "*.key", "*.crt", "*.p12", ".env", ".env.*", "*.token", "*-journal"} {
		if !strings.Contains(text, needle) {
			t.Errorf(".gitignore 缺少 %q", needle)
		}
	}
}

// arm64 机器上不许把 -linux-amd64 顶上来：装得上去，之后每次启动都是
// "Exec format error"（Agent 还是 Restart=always，反复重启刷日志），
// 而安装脚本只会说"服务没有起来"，排查成本极高。
func TestInstallersRefuseWrongArchFallback(t *testing.T) {
	for _, name := range []string{"install-server.sh", "install-agent.sh"} {
		content := readScript(t, name)
		if !strings.Contains(content, `*"-linux-amd64") [ "${HOST_ARCH}" = "amd64" ] || continue ;;`) {
			t.Errorf("%s 应当只在 HOST_ARCH=amd64 时才使用 -linux-amd64 候选文件", name)
		}
	}
}

// `stat -f -c %T` 是 GNU coreutils 专有语法：BusyBox/BSD 上会失败。
// 失败必须显式说出来（"已跳过检查"），不能像原来那样 `|| echo unknown` 让它看起来
// 像"检查通过" —— 那等于静默跳过"SQLite 不能放网络盘"这条数据完整性检查。
func TestServerScriptWarnsWhenFSTypeUnknown(t *testing.T) {
	content := readScript(t, "install-server.sh")
	// 只看代码：注释里会提到这个写法（说明"为什么不再这么写"）。
	if strings.Contains(codeOnly(content), "|| echo unknown") {
		t.Error("install-server.sh 不该把 stat 的失败吞成 unknown")
	}
	for _, needle := range []string{"已跳过网络盘检查", "stat 不支持 -f -c"} {
		if !strings.Contains(content, needle) {
			t.Errorf("install-server.sh 缺少 %q", needle)
		}
	}
}

// Agent 的 Token 也要能"不进命令行"：面板给的一键命令把长期 Token 写在 argv 上，
// 安装期间同机用户 ps / 读 /proc/*/cmdline 就能拿走，命令还会进 shell 历史。
func TestAgentInstallerAcceptsTokenWithoutArgv(t *testing.T) {
	content := readScript(t, "install-agent.sh")
	for _, needle := range []string{
		// 解析分支本身（不是注释/用法里提到的那两个词）
		`--from-file) TOKEN_SRC="${2:-}"; shift 2 ;;`,
		"PROBE_TOKEN",
		`tr -d ' \t\r\n' < "${TOKEN_SRC}"`,
	} {
		if !strings.Contains(content, needle) {
			t.Errorf("install-agent.sh 缺少 %q（Token 应该有命令行之外的两条给法）", needle)
		}
	}
	// install.sh 只透传路径，自己不读 Token。
	if one := readScript(t, "install.sh"); !strings.Contains(one, `--from-file) TOKEN_FILE_SRC="${2:-}"; shift 2 ;;`) {
		t.Error("install.sh 应当支持 --from-file 并把它透传给 install-agent.sh")
	}
}
