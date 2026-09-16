package user

import (
	"strings"
	"time"
)

// Status 是用户账号状态。
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// 登录失败达到该次数后锁定账号。
const (
	MaxFailedLogins = 5
	LockDuration    = 15 * time.Minute
)

// User 是平台会员。
//
// PasswordHash 属于敏感字段，handler 转换为 DTO 时不得带出。
type User struct {
	ID               int64
	Email            string
	PasswordHash     string
	Nickname         string
	Status           Status
	FailedLoginCount int
	LockedUntil      *time.Time
	LastLoginAt      *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IsActive 判断账号是否可用。禁用账号不允许登录与下单。
func (u *User) IsActive() bool { return u.Status == StatusActive }

// IsLocked 判断账号在当前时刻是否处于锁定状态。
func (u *User) IsLocked(now time.Time) bool {
	return u.LockedUntil != nil && u.LockedUntil.After(now)
}

// LockRemaining 返回锁定剩余时长，未锁定时返回 0。
func (u *User) LockRemaining(now time.Time) time.Duration {
	if !u.IsLocked(now) {
		return 0
	}
	return u.LockedUntil.Sub(now)
}

// Session 是一条用户会话，承载刷新令牌的状态。
type Session struct {
	ID            int64
	UserID        int64
	SessionID     string
	SecretHash    []byte
	DeviceLabel   string
	ClientIP      string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	LastUsedAt    *time.Time
	RevokedAt     *time.Time
	RevokedReason string
}

// IsUsable 判断会话在给定时刻是否可用于刷新。
func (s *Session) IsUsable(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	return s.ExpiresAt.After(now)
}

// 会话撤销原因。
const (
	RevokeLogout          = "logout"
	RevokeRotated         = "rotated"
	RevokePasswordChanged = "password_changed"
	RevokeAdmin           = "admin"
)

// ── 输入归一化 ────────────────────────────────────

// NormalizeEmail 归一化邮箱：去掉首尾空白并转小写。
//
// 库里的排序规则是 utf8mb4_0900_ai_ci（大小写不敏感），唯一索引本身
// 就能拦住 Abc@x.com 与 abc@x.com 的重复注册。这里仍统一转小写，
// 是为了让存储的值保持规范形态，避免同一邮箱出现多种写法。
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// NormalizeNickname 归一化昵称：去掉首尾空白。
func NormalizeNickname(raw string) string {
	return strings.TrimSpace(raw)
}

// ValidateEmail 做基本的邮箱格式校验。
//
// 刻意不用完整 RFC 5322 正则：那套规则极其复杂，且会拒绝一些合法地址。
// 这里只拦明显错误的输入，真实有效性由"能否收到邮件"决定。
func ValidateEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}

	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return false
	}
	// 只允许一个 @，且域名部分必须有 . 且不以 . 或 - 开头结尾
	if strings.Contains(domain, "@") {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.HasPrefix(domain, "-") || strings.HasSuffix(domain, "-") {
		return false
	}
	if strings.Contains(domain, "..") || strings.Contains(local, "..") {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	return true
}

// DeviceLabel 把 User-Agent 压成不超过 maxLen 字节的短标签，用于会话列表展示。
//
// 按 rune 截断而非字节，避免把多字节字符切成乱码。
func DeviceLabel(userAgent string, maxLen int) string {
	trimmed := strings.TrimSpace(userAgent)
	if trimmed == "" {
		return ""
	}

	runes := []rune(trimmed)
	if len(runes) <= maxLen {
		return string(runes)
	}
	return string(runes[:maxLen])
}
