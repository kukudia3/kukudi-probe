package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSetTwoFactorLastCounterOnlyAdvances 钉住防重放的落库语义：计数器只能被
// "消费"一次、且只能往前推。服务端的次序是"读状态 → 算码 → 写计数器"，中间没有
// 原子性，所以"同一个码不能用两次"这条保证必须落在这个条件更新上。
func TestSetTwoFactorLastCounterOnlyAdvances(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// 首次写入（老库里连这一行都没有）必须生效。
	if ok, err := db.SetTwoFactorLastCounter(ctx, 100); err != nil || !ok {
		t.Fatalf("首次写入应当生效: ok=%v err=%v", ok, err)
	}
	// 同一个计数器再写一次 → 拒绝：这就是"同一个码不能用两次"。
	if ok, err := db.SetTwoFactorLastCounter(ctx, 100); err != nil || ok {
		t.Fatalf("重复写入同一个计数器应当被拒: ok=%v err=%v", ok, err)
	}
	// 回退 → 拒绝：重放一个更早窗口的码。
	if ok, err := db.SetTwoFactorLastCounter(ctx, 99); err != nil || ok {
		t.Fatalf("回退写入应当被拒: ok=%v err=%v", ok, err)
	}
	// 前进 → 生效：下一个 30 秒窗口的码照旧能登录。
	if ok, err := db.SetTwoFactorLastCounter(ctx, 101); err != nil || !ok {
		t.Fatalf("前进写入应当生效: ok=%v err=%v", ok, err)
	}

	// 并发：10 个请求各自读到同一个 LastCounter、各自算出同一个计数器 C，
	// 只能有一个消费成功（其余都是重放）。
	const c = 500
	var wg sync.WaitGroup
	var wins atomic.Int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := db.SetTwoFactorLastCounter(ctx, c); err == nil && ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("同一个计数器被 %d 个请求消费成功，期望 1", wins.Load())
	}
	st, err := db.TwoFactorState(ctx)
	if err != nil {
		t.Fatalf("TwoFactorState: %v", err)
	}
	if st.LastCounter != c {
		t.Fatalf("LastCounter = %d，期望 %d", st.LastCounter, c)
	}
}

// TestTwoFactorStateIsSingleSnapshot 守住「三块状态必须来自同一个快照」。
//
// 三个键在写侧是**同一个事务**里写的（EnableTwoFactor），但读侧一旦是三条独立
// SELECT，中间就能插进一次提交 —— 于是"重新生成恢复码 / 关闭再开启"的瞬间会读到
// 「gen1 的密钥 + gen2 的恢复码」：旧恢复码在这个窗口里仍然能用，而"重新生成后
// 旧的立即作废"正是恢复码的全部意义（LastCounter 读到旧值还会放宽防重放窗口）。
//
// 不变量：密钥、恢复码、计数器三者必须来自同一次写入。写侧在事务里成对翻动，
// 读侧持续核对这个配对关系。
func TestTwoFactorStateIsSingleSnapshot(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	const (
		secretA  = "JBSWY3DPEHPK3PXP"
		secretB  = "KRSXG5CTMVRXEZLU"
		hashA    = "aaaa"
		hashB    = "bbbb"
		wantRead = 2000 // 反向验证时"读到混合态"是概率事件，读够多次才能稳定复现
		wantWr   = 200  // 写侧至少要真的提交过这么多次，否则读窗口里根本没有提交可插
	)

	stop := make(chan struct{})
	var commits atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			flip = !flip
			secret, hash, counter := secretA, hashA, int64(1)
			if flip {
				secret, hash, counter = secretB, hashB, int64(2)
			}
			if err := db.EnableTwoFactor(ctx, secret, []string{hash}, counter); err != nil {
				return // 用例结束时数据库正在关闭
			}
			commits.Add(1)
		}
	}()
	var stopOnce sync.Once
	stopWriter := func() {
		stopOnce.Do(func() {
			close(stop)
			wg.Wait()
		})
	}
	defer stopWriter()

	// 等第一次写入落库：在那之前三个键都还不存在，读到的是"还没开两步验证"，
	// 那是合法状态，不是半新半旧。
	for commits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	deadline := time.Now().Add(10 * time.Second)
	reads := 0
	for reads < wantRead || commits.Load() < wantWr {
		if time.Now().After(deadline) {
			break
		}
		st, err := db.TwoFactorState(ctx)
		if err != nil {
			t.Fatalf("TwoFactorState: %v", err)
		}
		reads++
		if len(st.RecoveryHashes) != 1 {
			t.Fatalf("恢复码条目 = %d，期望 1（读到一半的状态）", len(st.RecoveryHashes))
		}
		wantHash, wantCounter := hashA, int64(1)
		switch st.Secret {
		case secretA:
		case secretB:
			wantHash, wantCounter = hashB, int64(2)
		default:
			t.Fatalf("读到了未知密钥 %q", st.Secret)
		}
		if st.RecoveryHashes[0] != wantHash || st.LastCounter != wantCounter {
			stopWriter()
			t.Fatalf("读到了半新半旧的状态：密钥 %q 配 %q/%d，期望 %q/%d",
				st.Secret, st.RecoveryHashes[0], st.LastCounter, wantHash, wantCounter)
		}
	}
	if reads < 100 || commits.Load() < 10 {
		t.Fatalf("读 %d 次 / 写 %d 次，样本太少（用例没有真正跑到并发）", reads, commits.Load())
	}
}
