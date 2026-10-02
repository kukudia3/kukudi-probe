package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// 这个文件是 TOTP 算法本身（RFC 6238，基于 RFC 4226 的 HOTP）与配套的密钥/恢复码。
//
// 参数刻意选成**与 Google Authenticator / 1Password / Authy 的默认值一致**：
// SHA-1、6 位、30 秒。这三家（以及绝大多数验证器 App）都支持别的组合，
// 但"默认值"是唯一不需要用户在 App 里额外选一遍的东西，选错了的表现是
// "扫进去之后码永远不对"，而用户根本看不出是哪里不对。
//
// 常量时间比较（crypto/subtle）在两条路上都必须有：
//   - 6 位码只有 100 万种，逐位短路比较能把猜测次数从 10^6 降到 10^1 量级；
//   - 恢复码同理（虽然它更长）。
//
// 本文件不碰任何存储：密钥怎么存、谁能读，见 internal/store/twofa.go。
const (
	// totpPeriod 是时间步长（秒）。30 秒是 RFC 6238 的推荐值，也是所有
	// 验证器 App 的默认值。
	totpPeriod = 30
	// totpDigits 是动态码位数。
	totpDigits = 6
	// totpSkew 是容许的时间窗偏移（±1 个 30 秒）。
	//
	// 为什么是 1 而不是更大：手机与服务器的时钟总会差几秒，±1 窗能容忍
	// 最多 30 秒的漂移（覆盖了绝大多数没同步 NTP 的手机）；而每放宽一格，
	// 一个码的有效期就多 30 秒，暴力破解的窗口也同比变宽 —— 6 位码只有
	// 100 万种，窗口越宽越要靠限流兜住（见 auth.go 的 attemptLimiter）。
	totpSkew = 1
	// totpSecretBytes 是种子长度：160 位。RFC 4226 §4 要求 HMAC-SHA1 的密钥
	// 至少 128 位（这样 HMAC 的密钥块才被填满），推荐 160 位。
	totpSecretBytes = 20
	// recoveryCodeCount 是一次生成多少个恢复码。
	recoveryCodeCount = 10
	// recoveryCodeChars 是恢复码长度（base32 字符表，5 位/字符 → 50 位熵）。
	recoveryCodeChars = 10
)

// base32NoPad 是验证器 App 认识的 base32 形式（大写、无填充）。
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrBadSecret 表示种子不是合法的 base32。
var ErrBadSecret = errors.New("两步验证密钥不是合法的 base32")

// newTOTPSecret 生成一个新的 TOTP 种子（base32 文本，无空格）。
//
// 必须用 crypto/rand：种子就是第二因素的**全部**秘密，可预测的种子等于没有
// 第二因素（math/rand 的默认源在进程启动时是确定性的）。
func newTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成两步验证密钥失败: %w", err)
	}
	return base32NoPad.EncodeToString(raw), nil
}

// normalizeSecret 把用户/数据库里的种子文本归一化成可解码的形式。
//
// 用户手动输入时会带上分组空格或小写，数据库里的值则永远是大写无空格 ——
// 两条路都要能解开，所以归一化只有这一处。
func normalizeSecret(secret string) ([]byte, error) {
	var b strings.Builder
	for _, r := range secret {
		switch {
		case r == ' ' || r == '-' || r == '\t' || r == '=':
			// 分隔符与填充：丢掉
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToUpper(r))
		default:
			return nil, ErrBadSecret
		}
	}
	raw, err := base32NoPad.DecodeString(b.String())
	if err != nil {
		return nil, ErrBadSecret
	}
	if len(raw) == 0 {
		return nil, ErrBadSecret
	}
	return raw, nil
}

// formatSecret 把种子按 4 个字符一组分开，方便用户手动抄写。
func formatSecret(secret string) string {
	var parts []string
	for len(secret) > 4 {
		parts = append(parts, secret[:4])
		secret = secret[4:]
	}
	if secret != "" {
		parts = append(parts, secret)
	}
	return strings.Join(parts, " ")
}

// hotpCode 是 RFC 4226 的 HOTP：HMAC-SHA1(counter) → 动态截断 → 取 digits 位。
func hotpCode(key []byte, counter uint64, digits int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// 动态截断（RFC 4226 §5.3）：用最后一个字节的低 4 位当偏移，
	// 取 4 个字节并抹掉最高位，得到 31 位整数。
	offset := sum[len(sum)-1] & 0x0f
	value := uint32(sum[offset]&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// totpCounter 是某个时刻对应的时间计数器。
func totpCounter(t time.Time) int64 { return t.Unix() / totpPeriod }

// totpCodeAt 算出某个时刻的 6 位动态码（测试与"当前码"回显都用它）。
func totpCodeAt(secret string, t time.Time) (string, error) {
	key, err := normalizeSecret(secret)
	if err != nil {
		return "", err
	}
	return hotpCode(key, uint64(totpCounter(t)), totpDigits), nil
}

// verifyTOTP 校验一个 6 位码，返回它命中的时间计数器。
//
// 两个要点，缺一个这套东西就有洞：
//
//  1. **时间窗是 ±totpSkew 格**（默认 ±1 个 30 秒），不是"无限往前找" ——
//     否则一个截获的旧码可以永远用下去。
//  2. **同一个计数器只能用一次**：counter <= lastCounter 的窗直接跳过。
//     这条是防重放的关键（截获了当前码的人在同一个 30 秒里不能再登一次，
//     而 30 秒后那个码本身就过期了）。
//
// 比较走 subtle.ConstantTimeCompare：6 位码只有 100 万种，逐位短路的比较
// 能把在线爆破的尝试次数压到 10 次量级。
func verifyTOTP(secret, code string, now time.Time, lastCounter int64) (int64, bool) {
	key, err := normalizeSecret(secret)
	if err != nil {
		return 0, false
	}
	code = normalizeDigits(code)
	if len(code) != totpDigits {
		return 0, false
	}
	base := totpCounter(now)
	var matched int64
	found := false
	for delta := -totpSkew; delta <= totpSkew; delta++ {
		counter := base + int64(delta)
		if counter < 0 || counter <= lastCounter {
			continue
		}
		want := hotpCode(key, uint64(counter), totpDigits)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			// 不 break：把三个窗口都算完，用时与"第几个窗口命中"无关。
			if !found {
				matched, found = counter, true
			}
		}
	}
	return matched, found
}

// normalizeDigits 去掉用户输入里的空格与连字符（"123 456"、"123-456" 都收）。
func normalizeDigits(code string) string {
	var b strings.Builder
	for _, r := range code {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// usedTOTPCode 报告这个码是不是"上一次已经用过的那个"。
//
// 它只影响**错误消息**（防重放的判定在 verifyTOTP 里）：用户在一个 30 秒窗口里
// 刚用某个码登录过，紧接着又拿同一个码去关闭两步验证，这时如果只说"动态码不正确"，
// 他会以为是 App 或时钟出了问题 —— 而真实原因是"同一个码不能重复使用"。
func usedTOTPCode(secret string, lastCounter int64, code string) bool {
	if lastCounter < 0 {
		return false
	}
	key, err := normalizeSecret(secret)
	if err != nil {
		return false
	}
	normalized := normalizeDigits(code)
	if len(normalized) != totpDigits {
		return false
	}
	want := hotpCode(key, uint64(lastCounter), totpDigits)
	return subtle.ConstantTimeCompare([]byte(want), []byte(normalized)) == 1
}

// ---------------------------------------------------------------- 恢复码

// newRecoveryCodes 生成 recoveryCodeCount 个一次性恢复码（返回的是**明文**，
// 只在生成的那一刻存在：库里只留 SHA-256）。
func newRecoveryCodes() ([]string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567" // base32 字符表
	codes := make([]string, 0, recoveryCodeCount)
	buf := make([]byte, recoveryCodeChars)
	for i := 0; i < recoveryCodeCount; i++ {
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("生成恢复码失败: %w", err)
		}
		var b strings.Builder
		for j, v := range buf {
			// 32 个字符表整除 256，取模不会引入偏置。
			if j == recoveryCodeChars/2 {
				b.WriteByte('-') // 中间加一个连字符，抄写时不容易串行
			}
			b.WriteByte(alphabet[int(v)%len(alphabet)])
		}
		codes = append(codes, b.String())
	}
	return codes, nil
}

// normalizeRecoveryCode 把用户输入的恢复码归一化（大写、去掉连字符与空格）。
func normalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// hashRecoveryCode 是恢复码的存储形式。
//
// 为什么可以用一个"快"哈希（而不是像密码那样用 Argon2id）：
// 恢复码是 50 位熵的**随机**字符串，不是人选的口令，字典攻击没有着力点；
// 而每个恢复码都只可能被在线尝试一次（用过即废 + 登录限流）。
// 用 Argon2id 的话，每次登录失败都要多花 100ms 与 64MiB，收益为零。
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

// hashRecoveryCodes 批量算哈希（入库用）。
func hashRecoveryCodes(codes []string) []string {
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		out = append(out, hashRecoveryCode(code))
	}
	return out
}

// ---------------------------------------------------------------- otpauth 链接

// otpauthURL 拼出验证器 App 认识的 otpauth:// 链接（Google Authenticator 的
// "手动输入"与二维码扫的都是它）。
//
// 格式（Key URI Format，各 App 的事实标准）：
//
//	otpauth://totp/{issuer}:{account}?secret=...&issuer=...&algorithm=SHA1&digits=6&period=30
//
// 参数全部显式写出来（不依赖 App 的默认值）：默认值一旦哪个 App 取得不一样，
// 表现就是"扫进去之后码永远不对"，而排查它需要用户去翻 App 的高级设置。
func otpauthURL(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", totpDigits))
	q.Set("period", fmt.Sprintf("%d", totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
