package user

import (
	"testing"
	"time"
)

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  Alice@Example.COM  ": "alice@example.com",
		"alice@example.com":     "alice@example.com",
		"\talice@example.com\n": "alice@example.com",
		"":                      "",
	}
	for input, want := range cases {
		if got := NormalizeEmail(input); got != want {
			t.Errorf("NormalizeEmail(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{
		"alice@example.com",
		"a.b+tag@sub.example.co.uk",
		"user_name@example.io",
		"x@y.zz",
	}
	for _, email := range valid {
		if !ValidateEmail(email) {
			t.Errorf("%q 应当被判定为合法", email)
		}
	}

	invalid := []string{
		"",
		"no-at-sign",
		"@example.com",
		"alice@",
		"alice@localhost",     // 域名没有点
		"alice@@example.com",  // 两个 @
		"alice@.example.com",  // 域名以点开头
		"alice@example.com.",  // 域名以点结尾
		"alice@-example.com",  // 域名以连字符开头
		"alice@example..com",  // 连续点
		"alice example@x.com", // 含空格
		"alice@exam ple.com",
	}
	for _, email := range invalid {
		if ValidateEmail(email) {
			t.Errorf("%q 应当被判定为非法", email)
		}
	}
}

func TestValidateEmailLengthLimit(t *testing.T) {
	// 254 是 RFC 5321 对邮箱总长的上限
	local := make([]byte, 250)
	for i := range local {
		local[i] = 'a'
	}
	long := string(local) + "@x.com"
	if len(long) <= 254 {
		t.Fatalf("测试数据构造有误，长度为 %d", len(long))
	}
	if ValidateEmail(long) {
		t.Error("超过 254 字节的邮箱应当被拒绝")
	}
}

func TestUserIsLocked(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	future := now.Add(10 * time.Minute)
	past := now.Add(-10 * time.Minute)

	cases := []struct {
		name   string
		locked *time.Time
		want   bool
	}{
		{"未锁定", nil, false},
		{"锁定中", &future, true},
		{"锁定期已过", &past, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := User{LockedUntil: tc.locked}
			if got := u.IsLocked(now); got != tc.want {
				t.Errorf("IsLocked = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestUserLockRemaining(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	u := User{}
	if got := u.LockRemaining(now); got != 0 {
		t.Errorf("未锁定时剩余时长应为 0，实际 %v", got)
	}

	until := now.Add(10 * time.Minute)
	u.LockedUntil = &until
	if got := u.LockRemaining(now); got != 10*time.Minute {
		t.Errorf("剩余时长期望 10m，实际 %v", got)
	}
}

func TestSessionIsUsable(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Minute)

	cases := []struct {
		name  string
		build func() Session
		want  bool
	}{
		{
			name:  "正常有效",
			build: func() Session { return Session{ExpiresAt: now.Add(time.Hour)} },
			want:  true,
		},
		{
			name:  "已过期",
			build: func() Session { return Session{ExpiresAt: now.Add(-time.Hour)} },
			want:  false,
		},
		{
			name: "已撤销",
			build: func() Session {
				return Session{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}
			},
			want: false,
		},
		{
			name: "已撤销且已过期",
			build: func() Session {
				return Session{ExpiresAt: now.Add(-time.Hour), RevokedAt: &revoked}
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build()
			if got := s.IsUsable(now); got != tc.want {
				t.Errorf("IsUsable = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 按 rune 截断，不能把多字节字符切成乱码。
func TestDeviceLabel(t *testing.T) {
	if got := DeviceLabel("  Mozilla/5.0  ", 128); got != "Mozilla/5.0" {
		t.Errorf("应当去掉首尾空白，实际 %q", got)
	}
	if got := DeviceLabel("", 128); got != "" {
		t.Errorf("空输入应返回空串，实际 %q", got)
	}

	// 5 个汉字截到 3 个字符，必须仍是完整汉字
	got := DeviceLabel("微信浏览器测试", 3)
	if got != "微信浏" {
		t.Errorf("截断结果期望 %q，实际 %q", "微信浏", got)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Error("截断产生了乱码字符")
		}
	}

	// 刚好等于上限时不截断
	if got := DeviceLabel("abc", 3); got != "abc" {
		t.Errorf("长度等于上限时不应截断，实际 %q", got)
	}
}

func TestNormalizeNickname(t *testing.T) {
	if got := NormalizeNickname("  苍洱  "); got != "苍洱" {
		t.Errorf("期望 %q，实际 %q", "苍洱", got)
	}
}

func TestUserIsActive(t *testing.T) {
	if !(&User{Status: StatusActive}).IsActive() {
		t.Error("active 状态应当可用")
	}
	if (&User{Status: StatusDisabled}).IsActive() {
		t.Error("disabled 状态不应可用")
	}
	if (&User{Status: ""}).IsActive() {
		t.Error("空状态不应可用")
	}
}
