package token

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-at-least-32-bytes-long!!"

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	return NewSigner(testSecret, 15*time.Minute)
}

func TestIssueAndParseAccess(t *testing.T) {
	signer := newTestSigner(t)

	raw, jti, expiresAt, err := signer.IssueAccess(10001, "sess-abc")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if jti == "" {
		t.Error("jti 不应为空")
	}
	if !expiresAt.After(time.Now()) {
		t.Error("过期时间应当在未来")
	}

	claims, err := signer.ParseAccess(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if got := claims.UserID(); got != 10001 {
		t.Errorf("用户 ID 期望 10001，实际 %d", got)
	}
	if claims.ID != jti {
		t.Errorf("jti 期望 %q，实际 %q", jti, claims.ID)
	}
	// 会话标识必须带上，否则退出登录无法定位单条会话
	if claims.SessionID != "sess-abc" {
		t.Errorf("会话标识期望 sess-abc，实际 %q", claims.SessionID)
	}
	if claims.Type != TypeAccess {
		t.Errorf("令牌用途期望 %q，实际 %q", TypeAccess, claims.Type)
	}
}

func TestParseAccessRejectsWrongSecret(t *testing.T) {
	raw, _, _, err := newTestSigner(t).IssueAccess(10001, "sess-abc")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	other := NewSigner("another-secret-at-least-32-bytes!!!", 15*time.Minute)
	if _, err := other.ParseAccess(raw); err == nil {
		t.Error("用其他密钥签发的令牌不应校验通过")
	}
}

func TestParseAccessRejectsExpired(t *testing.T) {
	// 不能靠"签发一个负有效期的令牌"来构造：NewSigner 会直接 panic，
	// 那是配置错误而非过期场景。这里手工签一条已过期的令牌。
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerName,
			Subject:   "10001",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		Type: TypeAccess,
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造过期令牌失败: %v", err)
	}

	_, err = newTestSigner(t).ParseAccess(raw)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("期望 ErrTokenExpired，实际 %v", err)
	}
}

// 有效期非正属于配置错误，应当启动即失败，而不是签出立刻过期的令牌。
func TestNewSignerRejectsNonPositiveTTL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("非正有效期应当直接 panic")
		}
	}()
	NewSigner(testSecret, 0)
}

func TestParseAccessRejectsTampered(t *testing.T) {
	raw, _, _, err := newTestSigner(t).IssueAccess(10001, "sess-abc")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 应有 3 段，实际 %d", len(parts))
	}

	cases := map[string]string{
		"篡改载荷": parts[0] + "." + parts[1] + "x." + parts[2],
		"篡改签名": parts[0] + "." + parts[1] + "." + parts[2] + "x",
		"段数不对": "not-a-jwt",
		"空串":   "",
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newTestSigner(t).ParseAccess(bad); err == nil {
				t.Error("被篡改的令牌不应校验通过")
			}
		})
	}
}

// alg=none 是经典的算法混淆攻击：攻击者把签名算法改成 none，
// 期望服务端跳过签名校验。
func TestParseAccessRejectsNoneAlgorithm(t *testing.T) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerName,
			Subject:   "10001",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Type: TypeAccess,
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("构造 alg=none 令牌失败: %v", err)
	}

	if _, err := newTestSigner(t).ParseAccess(raw); err == nil {
		t.Error("alg=none 的令牌不应被接受")
	}
}

// 令牌用途不符必须拒绝，防止其他用途的令牌被当作访问令牌使用。
func TestParseAccessRejectsWrongType(t *testing.T) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerName,
			Subject:   "10001",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Type: "refresh",
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造令牌失败: %v", err)
	}

	_, err = newTestSigner(t).ParseAccess(raw)
	if !errors.Is(err, ErrWrongTokenType) {
		t.Errorf("期望 ErrWrongTokenType，实际 %v", err)
	}
}

func TestParseAccessRejectsForeignIssuer(t *testing.T) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "some-other-service",
			Subject:   "10001",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Type: TypeAccess,
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造令牌失败: %v", err)
	}

	if _, err := newTestSigner(t).ParseAccess(raw); err == nil {
		t.Error("其他签发方的令牌不应被接受")
	}
}

// ── 刷新令牌 ──────────────────────────────────────

func TestRefreshTokenRoundTrip(t *testing.T) {
	rt, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("生成刷新令牌失败: %v", err)
	}

	if rt.SessionID == "" || rt.Secret == "" {
		t.Fatal("会话标识与密钥都不应为空")
	}

	parsed, err := ParseRefreshToken(rt.String())
	if err != nil {
		t.Fatalf("解析刷新令牌失败: %v", err)
	}
	if parsed.SessionID != rt.SessionID {
		t.Errorf("会话标识不匹配: %q vs %q", parsed.SessionID, rt.SessionID)
	}
	if parsed.Secret != rt.Secret {
		t.Errorf("密钥不匹配: %q vs %q", parsed.Secret, rt.Secret)
	}
}

func TestRefreshTokensAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		rt, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("生成刷新令牌失败: %v", err)
		}
		if _, dup := seen[rt.String()]; dup {
			t.Fatal("生成的刷新令牌出现重复")
		}
		seen[rt.String()] = struct{}{}
	}
}

func TestParseRefreshTokenRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "no-dot", ".only-secret", "only-id.", "a.b.c"} {
		if _, err := ParseRefreshToken(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestVerifyRefreshSecret(t *testing.T) {
	rt, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("生成刷新令牌失败: %v", err)
	}
	hash := HashRefreshSecret(rt.Secret)

	if !VerifyRefreshSecret(rt.Secret, hash) {
		t.Error("正确密钥应当校验通过")
	}
	if VerifyRefreshSecret("wrong-secret", hash) {
		t.Error("错误密钥不应校验通过")
	}
	if VerifyRefreshSecret(rt.Secret, []byte("too-short")) {
		t.Error("长度不符的哈希不应校验通过")
	}
}

// 哈希串里绝不能出现原始密钥，否则库被读走就等于令牌泄露。
func TestRefreshSecretHashHidesSecret(t *testing.T) {
	rt, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("生成刷新令牌失败: %v", err)
	}

	hash := HashRefreshSecret(rt.Secret)
	if len(hash) != 32 {
		t.Errorf("SHA-256 摘要应为 32 字节，实际 %d", len(hash))
	}
	if string(hash) == rt.Secret {
		t.Error("哈希结果与原始密钥相同")
	}
}

func TestNewSignerRejectsEmptySecret(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("空密钥应当直接 panic，避免签发出可伪造的令牌")
		}
	}()
	NewSigner("", time.Minute)
}
