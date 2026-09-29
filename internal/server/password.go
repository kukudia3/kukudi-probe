package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id 参数。按 docs/DESIGN.md §13：m=64MiB, t=3, p=1。
//
// 这是"慢哈希"：一次约 100ms 并占用 64MiB 内存。两道防线：
//   - attemptLimiter（login / setup 两个实例）：限制每个来源的尝试次数；
//   - Auth.verify 信号量（容量 2）：限制**同时**进行的哈希计算，避免内存被打爆。
//
// 两者都在 auth.go 里；这里的函数本身不做限流，调用方必须走 hashWithLimit /
// verifyWithLimit（它们才会占用信号量，并且尊重 ctx 取消）。
const (
	argonMemory      = 64 * 1024 // KiB
	argonIterations  = 3
	argonParallelism = 1
	argonSaltLen     = 16
	argonKeyLen      = 32
	argonVersion     = argon2.Version
)

// ErrInvalidHashFormat 表示数据库里的密码哈希不是本程序认识的格式。
var ErrInvalidHashFormat = errors.New("密码哈希格式不合法")

// hashPassword 生成 PHC 格式的 Argon2id 哈希：
//
//	$argon2id$v=19$m=65536,t=3,p=1$<salt>$<hash>
func hashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("密码不能为空")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword 校验密码是否与 PHC 哈希匹配（恒定时间比较）。
func verifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrInvalidHashFormat
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argonVersion {
		return false, ErrInvalidHashFormat
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, ErrInvalidHashFormat
	}
	if memory == 0 || iterations == 0 || parallelism == 0 {
		return false, ErrInvalidHashFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, ErrInvalidHashFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrInvalidHashFormat
	}

	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
