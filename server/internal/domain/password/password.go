// Package password 提供口令的 Argon2id 哈希与校验。
//
// 存储格式为 PHC 字符串（RFC 9106 推荐写法）：
//
//	$argon2id$v=19$m=65536,t=3,p=4$<base64盐>$<base64哈希>
//
// 把算法与参数一并写进哈希串，好处是日后调参时旧口令仍可校验：
// 校验时用串里记录的参数，命中后再按新参数重算（见 NeedsRehash）。
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 当前使用的参数。取 OWASP 对 Argon2id 的首选建议值。
const (
	argonMemoryKiB  = 64 * 1024 // 64 MiB
	argonIterations = 3
	argonThreads    = 4
	argonSaltLen    = 16
	argonKeyLen     = 32
)

// MaxPlainLength 是允许的口令最大长度。
//
// 契约（RegisterRequest.password.maxLength）为 72。Argon2 本身没有长度上限，
// 这里沿用 72 是为了给未来可能切换到 bcrypt 留出兼容空间——
// bcrypt 会静默截断 72 字节之后的输入，若不在这里拦住，
// 换算法时会出现"同样的口令校验不过"的诡异问题。
const MaxPlainLength = 72

// MinPlainLength 是允许的口令最小长度。
const MinPlainLength = 8

// 解析 PHC 串时可能出现的错误。
var (
	ErrInvalidHash    = errors.New("口令哈希格式非法")
	ErrIncompatible   = errors.New("口令哈希算法或版本不受支持")
	ErrPlainTooLong   = fmt.Errorf("口令长度不能超过 %d 字节", MaxPlainLength)
	ErrPlainTooShort  = fmt.Errorf("口令长度不能少于 %d 字节", MinPlainLength)
	ErrPasswordNeeded = errors.New("口令不能为空")
)

// params 记录一条哈希所用的参数。
type params struct {
	memory     uint32
	iterations uint32
	threads    uint8
}

// currentParams 返回当前应当使用的参数。
func currentParams() params {
	return params{memory: argonMemoryKiB, iterations: argonIterations, threads: argonThreads}
}

// ValidatePlain 校验明文口令是否满足长度要求。
func ValidatePlain(plain string) error {
	if plain == "" {
		return ErrPasswordNeeded
	}
	if len(plain) < MinPlainLength {
		return ErrPlainTooShort
	}
	// 按字节长度判断，与 bcrypt 的 72 字节限制口径一致
	if len(plain) > MaxPlainLength {
		return ErrPlainTooLong
	}
	return nil
}

// Hash 计算口令的 Argon2id 哈希，返回可直接入库的 PHC 字符串。
func Hash(plain string) (string, error) {
	if err := ValidatePlain(plain); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成口令盐失败: %w", err)
	}

	p := currentParams()
	key := argon2.IDKey([]byte(plain), salt, p.iterations, p.memory, p.threads, argonKeyLen)

	return encode(p, salt, key), nil
}

// Verify 校验明文口令是否匹配哈希串。
//
// 返回 error 表示哈希串本身无法解析（数据损坏或算法不受支持），
// 与"口令不匹配"是两回事：前者应记日志并视为服务端问题，
// 后者是正常的业务失败。
func Verify(plain, encoded string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(plain), salt, p.iterations, p.memory, p.threads, uint32(len(want)))

	// 必须用恒定时间比较，避免通过响应时间差反推口令
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash 判断已存的哈希是否需要用当前参数重算。
//
// 调高 argonMemoryKiB 等参数后，用户下次登录成功时即可静默升级哈希，
// 不需要强制所有人改密。
func NeedsRehash(encoded string) bool {
	p, _, _, err := decode(encoded)
	if err != nil {
		return true
	}
	return p != currentParams()
}

// ── PHC 编解码 ────────────────────────────────────

var b64 = base64.RawStdEncoding

func encode(p params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.iterations, p.threads,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decode(encoded string) (params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// 形如 ["", "argon2id", "v=19", "m=65536,t=3,p=4", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[0] != "" {
		return params{}, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return params{}, nil, nil, fmt.Errorf("%w: 算法 %q", ErrIncompatible, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return params{}, nil, nil, fmt.Errorf("%w: 版本 %d", ErrIncompatible, version)
	}

	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.threads); err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	if p.memory == 0 || p.iterations == 0 || p.threads == 0 {
		return params{}, nil, nil, ErrInvalidHash
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return params{}, nil, nil, ErrInvalidHash
	}

	return p, salt, key, nil
}
