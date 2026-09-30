package server

import (
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// 本机地址（Agent 自报）与服务端观测来源地址必须**各自**透传到 DTO：
// 前端靠 local_ip / local_ip6 显示「本机地址」，靠 observed_ip 显示「来源 IP」。
// 三者含义不同，谁也不能覆盖谁。
func TestBuildNodeDTOPassesThroughLocalIPs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	node := store.Node{ID: 7, Name: "hk-01", IntervalSec: 1, Enabled: true}
	st := state.Node{
		NodeID:     7,
		Connected:  true,
		LastSeen:   now,
		ObservedIP: "127.0.0.1",
		LocalIP:    "203.0.113.5",
		LocalIP6:   "2001:db8::1",
		Info:       protocol.Info{AgentVersion: "0.1.0", Hostname: "hk-01"},
	}

	dto := buildNodeDTO(node, st, true, now, time.Minute, 2*time.Minute)
	if dto.ObservedIP != "127.0.0.1" {
		t.Errorf("observed_ip = %q，期望 127.0.0.1", dto.ObservedIP)
	}
	if dto.LocalIP != "203.0.113.5" {
		t.Errorf("local_ip = %q，期望 203.0.113.5", dto.LocalIP)
	}
	if dto.LocalIP6 != "2001:db8::1" {
		t.Errorf("local_ip6 = %q，期望 2001:db8::1", dto.LocalIP6)
	}

	// 没有内存状态时（节点从未连接）本机地址留空，而不是报错或填占位符：
	// 占位符是前端的事，服务端只传事实。
	empty := buildNodeDTO(node, state.Node{}, false, now, time.Minute, 2*time.Minute)
	if empty.LocalIP != "" || empty.LocalIP6 != "" || empty.ObservedIP != "" {
		t.Errorf("无状态时地址应当为空: %+v", empty)
	}
}
