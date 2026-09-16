package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/token"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
)

const testSecret = "test-secret-at-least-32-bytes-long!!"

// captureIdentity 是测试用的下游处理器，把上下文中的身份取出来供断言。
func captureIdentity(t *testing.T, got *httpx.Identity, called *bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		if id, ok := httpx.IdentityFromContext(r.Context()); ok {
			*got = id
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthAcceptsValidToken(t *testing.T) {
	signer := token.NewSigner(testSecret, 15*time.Minute)
	raw, jti, _, err := signer.IssueAccess(10001, "sess-abc")
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	var (
		got    httpx.Identity
		called bool
	)
	handler := Auth(signer)(captureIdentity(t, &got, &called))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if !called {
		t.Fatal("下游处理器未被调用")
	}
	if got.UserID != 10001 {
		t.Errorf("用户 ID 期望 10001，实际 %d", got.UserID)
	}

	// 这一条是回归防护：曾经误把 jti 当作会话标识写进上下文，
	// 导致退出登录撤销不到任何会话。
	if got.SessionID != "sess-abc" {
		t.Errorf("会话标识期望 sess-abc（sid 声明），实际 %q；"+
			"若实际值等于 jti=%q，说明中间件取错了声明", got.SessionID, jti)
	}
}

func TestAuthRejectsMissingOrBadToken(t *testing.T) {
	signer := token.NewSigner(testSecret, 15*time.Minute)

	cases := map[string]string{
		"未携带请求头":    "",
		"只有 Bearer": "Bearer",
		"空令牌":       "Bearer ",
		"非法令牌":      "Bearer not-a-jwt",
		"缺少 Bearer": "not-a-jwt",
		"其他认证方案":    "Basic dXNlcjpwYXNz",
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			var (
				got    httpx.Identity
				called bool
			)
			handler := Auth(signer)(captureIdentity(t, &got, &called))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("期望 401，实际 %d", rec.Code)
			}
			if called {
				t.Error("鉴权失败时不应调用下游处理器")
			}
		})
	}
}

func TestBearerTokenIsCaseInsensitive(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc",
		"BEARER abc":   "abc",
		"BeArEr abc":   "abc",
		"Bearer  abc ": "abc",
	}
	for header, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", header)
		if got := BearerToken(req); got != want {
			t.Errorf("BearerToken(%q) = %q，期望 %q", header, got, want)
		}
	}
}
