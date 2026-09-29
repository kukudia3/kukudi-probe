package protocol

import (
	"fmt"
	"math"
	"strings"
)

// 校验上限。取值原则：够用即可，宁可拒绝可疑数据，也不让它进入数据库。
const (
	maxUint53        = uint64(1) << 53
	maxVersionLen    = 64
	maxHostnameLen   = 255
	maxOSFieldLen    = 128
	maxModelLen      = 128
	maxBootIDLen     = 64
	maxIfaceLen      = 32
	maxMACLen        = 32
	maxMountLen      = 128
	maxFilesystemLen = 32
	maxDiskEntries   = 8
	maxLoadValue     = 100000
	maxAgentCores    = 4096
)

// ValidateHello 校验 hello 负载。
func ValidateHello(h Hello) error {
	if err := checkLen("agent_version", h.AgentVersion, maxVersionLen, true); err != nil {
		return err
	}
	if err := checkLen("hostname", h.Hostname, maxHostnameLen, false); err != nil {
		return err
	}
	if err := checkLen("os.name", h.OS.Name, maxOSFieldLen, false); err != nil {
		return err
	}
	if err := checkLen("os.kernel", h.OS.Kernel, maxOSFieldLen, false); err != nil {
		return err
	}
	if err := checkLen("os.arch", h.OS.Arch, maxOSFieldLen, false); err != nil {
		return err
	}
	if err := checkLen("cpu.model", h.CPU.Model, maxModelLen, false); err != nil {
		return err
	}
	if h.CPU.Cores < 0 || h.CPU.Cores > maxAgentCores {
		return fmt.Errorf("cpu.cores %d 超出范围", h.CPU.Cores)
	}
	if err := checkLen("boot_id", h.BootID, maxBootIDLen, false); err != nil {
		return err
	}
	if h.UptimeSec >= maxUint53 {
		return fmt.Errorf("uptime_sec 过大")
	}
	if err := checkLen("iface.name", h.Iface.Name, maxIfaceLen, false); err != nil {
		return err
	}
	if h.Iface.IfIndex < 0 {
		return fmt.Errorf("iface.ifindex 不能为负")
	}
	if err := checkLen("iface.mac", h.Iface.MAC, maxMACLen, false); err != nil {
		return err
	}
	if h.IntervalSec != 0 && (h.IntervalSec < MinIntervalSec || h.IntervalSec > MaxIntervalSec) {
		return fmt.Errorf("interval_sec %d 超出 %d-%d", h.IntervalSec, MinIntervalSec, MaxIntervalSec)
	}
	return nil
}

// ValidateMetrics 校验实时指标。
//
// 原则：任何一项不合法就整帧拒绝——半真半假的数据比没有数据更危险
// （例如把 0 当成真实的流量累计值，会让服务端重设流量基线）。
func ValidateMetrics(m Metrics) error {
	if err := checkPct("cpu_pct", m.CPUPct); err != nil {
		return err
	}
	if m.CPUCores < 0 || m.CPUCores > maxAgentCores {
		return fmt.Errorf("cpu_cores %d 超出范围", m.CPUCores)
	}
	if err := validateMem("mem", m.Mem); err != nil {
		return err
	}
	if err := validateMem("swap", m.Swap); err != nil {
		return err
	}
	if len(m.Disk) > maxDiskEntries {
		return fmt.Errorf("disk 条目 %d 超过上限 %d", len(m.Disk), maxDiskEntries)
	}
	for i, d := range m.Disk {
		if err := checkLen(fmt.Sprintf("disk[%d].mount", i), d.Mount, maxMountLen, true); err != nil {
			return err
		}
		if err := checkLen(fmt.Sprintf("disk[%d].fs", i), d.FS, maxFilesystemLen, false); err != nil {
			return err
		}
		if d.Used > d.Total && d.Total > 0 {
			return fmt.Errorf("disk[%d] 已用 %d 大于总量 %d", i, d.Used, d.Total)
		}
		if err := checkPct(fmt.Sprintf("disk[%d].pct", i), d.Pct); err != nil {
			return err
		}
	}
	if err := validateLoad(m.Load); err != nil {
		return err
	}
	if err := validateNet(m.Net); err != nil {
		return err
	}
	if err := checkFinite("lat_ms", m.LatMS, 0, 600000); err != nil {
		return err
	}
	if m.UptimeSec >= maxUint53 {
		return fmt.Errorf("uptime_sec 过大")
	}
	if m.Dropped >= maxUint53 || m.Gap >= maxUint53 {
		return fmt.Errorf("dropped/gap 过大")
	}
	return nil
}

func validateMem(name string, m Mem) error {
	if m.Total >= maxUint53 || m.Used >= maxUint53 {
		return fmt.Errorf("%s 数值过大", name)
	}
	if m.Used > m.Total {
		return fmt.Errorf("%s 已用 %d 大于总量 %d", name, m.Used, m.Total)
	}
	return checkPct(name+".pct", m.Pct)
}

func validateLoad(l Load) error {
	for _, v := range []struct {
		name string
		val  float64
	}{{"load.l1", l.L1}, {"load.l5", l.L5}, {"load.l15", l.L15}} {
		if err := checkFinite(v.name, v.val, 0, maxLoadValue); err != nil {
			return err
		}
	}
	return nil
}

func validateNet(n Net) error {
	if err := checkLen("net.iface", n.Iface, maxIfaceLen, true); err != nil {
		return err
	}
	if err := checkLen("net.boot_id", n.BootID, maxBootIDLen, false); err != nil {
		return err
	}
	if n.RxTotal >= maxUint53 || n.TxTotal >= maxUint53 || n.RxRaw >= maxUint53 || n.TxRaw >= maxUint53 {
		return fmt.Errorf("net 累计字节数过大")
	}
	if err := checkFinite("net.rx_rate", n.RxRate, 0, float64(maxUint53)); err != nil {
		return err
	}
	if err := checkFinite("net.tx_rate", n.TxRate, 0, float64(maxUint53)); err != nil {
		return err
	}
	if n.CkptAgeS < -1 || n.CkptAgeS > 86400*365 {
		return fmt.Errorf("net.ckpt_age_s %d 超出范围", n.CkptAgeS)
	}
	return nil
}

func checkLen(name, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s 长度 %d 超过上限 %d", name, len(value), limit)
	}
	return nil
}

func checkPct(name string, v float64) error {
	return checkFinite(name, v, 0, 100)
}

// checkFinite 拒绝 NaN / ±Inf 与越界值。
func checkFinite(name string, v, min, max float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s 不是有限数值", name)
	}
	if v < min || v > max {
		return fmt.Errorf("%s = %v 超出 [%v, %v]", name, v, min, max)
	}
	return nil
}
