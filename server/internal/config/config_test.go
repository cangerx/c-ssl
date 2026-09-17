package config

import (
	"strings"
	"testing"

	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
)

// TestMockProviderNameMatches 钉住配置层与支付包对「mock 渠道叫什么」的一致认识。
//
// config 刻意不导入 payment（那是上层业务包，让底层配置依赖它会把依赖方向倒过来），
// 因此两边各写了一份字面量。这个测试就是那份重复的唯一代价：
// 取值分叉时，PAYMENT_PROVIDER=mock 会被当成未实现的渠道而拒绝启动，
// 而报错信息只说「未实现的渠道 mock」——很难联想到是两处字面量不一致。
func TestMockProviderNameMatches(t *testing.T) {
	if paymentProviderMock != payment.NameMock {
		t.Fatalf("配置层的渠道名 %q 与支付包的 %q 不一致",
			paymentProviderMock, payment.NameMock)
	}
}

// TestFoxSSLProviderNameMatches 是上游版本的同一条。
//
// 这里要导入 foxssl 而 payment 那条也导入了，两者都只是拿一个常量，
// 不构成 config → 业务包的运行时依赖。取值分叉的后果与支付侧一样：
// FOXSSL_PROVIDER=mock 会被判成未实现的上游，而报错只说「未实现的上游 mock」。
func TestFoxSSLProviderNameMatches(t *testing.T) {
	if foxsslProviderMock != foxssl.NameMock {
		t.Fatalf("配置层的上游名 %q 与 foxssl 包的 %q 不一致",
			foxsslProviderMock, foxssl.NameMock)
	}
}

// setBaseEnv 铺一套最小可用的环境变量。
//
// 必须把 Load 会用到的键全部显式设置：.env 的加载规则是「不覆盖已存在的变量」，
// 所以显式设置之后，测试结果就不再取决于开发者本机的 .env 内容。
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MYSQL_DSN", "user:pass@tcp(127.0.0.1:3306)/c_ssl")
	t.Setenv("REDIS_ADDR", "127.0.0.1:6379")
	t.Setenv("REDIS_DB", "1")
	t.Setenv("JWT_SECRET", "test-jwt-secret-at-least-32-characters")
	t.Setenv("PAYMENT_WEBHOOK_SECRET", "test-payment-secret-at-least-32-chars")
	t.Setenv("APP_ENV", "development")
	t.Setenv("PAYMENT_PROVIDER", paymentProviderMock)
	t.Setenv("FOXSSL_API_KEY", "test-foxssl-api-key")
	t.Setenv("FOXSSL_WEBHOOK_SECRET", "test-foxssl-secret-at-least-32-chars")
	t.Setenv("FOXSSL_PROVIDER", foxsslProviderMock)
}

// setProductionEnv 铺一套「可以上生产」的环境：两个上游都是真实实现。
//
// 生产环境的两条校验（支付渠道、证书上游）互相独立，所以这里必须把
// 两者都设成真实值——只改其中一个的话，另一个仍是 mock，
// 测试会以「另一个 provider 不对」失败，而报错看起来像是被测的那条校验有问题。
func setProductionEnv(t *testing.T) {
	t.Helper()
	setBaseEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("PAYMENT_PROVIDER", "wechat")
	t.Setenv("FOXSSL_PROVIDER", "foxssl")
}

func TestLoadAcceptsDevelopmentDefaults(t *testing.T) {
	setBaseEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("开发环境的最小配置应当可用，实际报错: %v", err)
	}
	if cfg.PaymentProvider != paymentProviderMock {
		t.Errorf("默认渠道应为 %s，实际 %s", paymentProviderMock, cfg.PaymentProvider)
	}
	if !cfg.IsDevelopment() {
		t.Errorf("IsDevelopment 应为 true，实际 APP_ENV=%s", cfg.AppEnv)
	}
}

// TestLoadRequiresPaymentWebhookSecret 验证验签密钥缺失时拒绝启动。
//
// 这条不能只是「记个警告」：密钥为空时 HMAC 的签名是公开可算的，
// 服务会带着一道形同虚设的验签跑起来，任何人都能伪造回调给任意账户充值。
func TestLoadRequiresPaymentWebhookSecret(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("PAYMENT_WEBHOOK_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("缺少 PAYMENT_WEBHOOK_SECRET 时应拒绝启动")
	}
	if !strings.Contains(err.Error(), "PAYMENT_WEBHOOK_SECRET") {
		t.Errorf("报错应点名缺失的配置项，实际: %v", err)
	}
}

// TestLoadRequiresLongPaymentSecretOutsideDevelopment 验证非开发环境对密钥长度有要求。
//
// 短密钥可以被暴力枚举，攻击者算得出合法签名，验签就只是一道装饰。
func TestLoadRequiresLongPaymentSecretOutsideDevelopment(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("APP_ENV", "staging")
	t.Setenv("PAYMENT_WEBHOOK_SECRET", "short")

	_, err := Load()
	if err == nil {
		t.Fatal("非开发环境的短支付密钥应被拒绝")
	}
	if !strings.Contains(err.Error(), "PAYMENT_WEBHOOK_SECRET") {
		t.Errorf("报错应点名 PAYMENT_WEBHOOK_SECRET，实际: %v", err)
	}
}

// TestLoadRejectsMockProviderInProduction 验证生产环境不允许启用 Mock 渠道。
//
// Mock 渠道不会真的收钱：它生成的支付地址是本地页面，回调也由本服务自己发出。
// 在生产环境启用等于给所有用户免费充值，而且不会报任何错。
// 让它在启动阶段就失败，是唯一能保证没人漏掉这件事的方式。
func TestLoadRejectsMockProviderInProduction(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("PAYMENT_PROVIDER", paymentProviderMock)

	_, err := Load()
	if err == nil {
		t.Fatal("生产环境启用 Mock 支付渠道应被拒绝启动")
	}
	if !strings.Contains(err.Error(), paymentProviderMock) {
		t.Errorf("报错应点名有问题的渠道，实际: %v", err)
	}
}

// TestLoadAllowsRealProviderInProduction 是上一条的对照。
//
// 只有 mock 被禁，真实渠道在生产环境必须能正常启动——
// 否则这条校验就从「防呆」变成了「挡住上线」。
func TestLoadAllowsRealProviderInProduction(t *testing.T) {
	setProductionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("生产环境使用真实渠道应当可以启动，实际报错: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction 应为 true")
	}
}

// TestIsProductionIsNotNegationOfDevelopment 验证中间环境不被当成生产。
//
// 用 !IsDevelopment() 代替 IsProduction() 的话，staging 与预发环境
// 会一并命中「只在生产生效」的严格校验，于是那些校验会在部署时意外拦住发布。
func TestIsProductionIsNotNegationOfDevelopment(t *testing.T) {
	for _, env := range []string{"staging", "test", "preview"} {
		cfg := &Config{AppEnv: env}
		if cfg.IsDevelopment() {
			t.Errorf("%s 不应被当成开发环境", env)
		}
		if cfg.IsProduction() {
			t.Errorf("%s 不应被当成生产环境", env)
		}
	}
}

func TestLoadRejectsZeroRedisDB(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("REDIS_DB", "0")

	if _, err := Load(); err == nil {
		t.Fatal("REDIS_DB 为 0 时应拒绝启动")
	}
}

// TestLoadRequiresFoxSSLSecrets 验证上游的两项配置缺失时拒绝启动。
//
// API Key：适配层拒绝用空 Key 构造，因为「忘记配置」静默变成「用空 Key 调用」时，
// 上游返回的错误通常与凭证无关，排查方向会跑偏。
//
// Webhook 密钥：回调接口不套登录鉴权，密钥就是它唯一的身份凭证。
// 为空时任何人都算得出 HMAC 并伪造事件，把订单推进到已签发。
func TestLoadRequiresFoxSSLSecrets(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"缺 API Key", "FOXSSL_API_KEY"},
		{"缺 Webhook 密钥", "FOXSSL_WEBHOOK_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv(tc.key, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("缺少 %s 时应拒绝启动", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("报错应点名 %s，实际: %v", tc.key, err)
			}
		})
	}
}

// TestLoadRequiresLongFoxSSLSecretOutsideDevelopment 验证非开发环境的密钥长度要求。
func TestLoadRequiresLongFoxSSLSecretOutsideDevelopment(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("APP_ENV", "staging")
	t.Setenv("FOXSSL_WEBHOOK_SECRET", "short")

	_, err := Load()
	if err == nil {
		t.Fatal("非开发环境的短上游密钥应被拒绝")
	}
	if !strings.Contains(err.Error(), "FOXSSL_WEBHOOK_SECRET") {
		t.Errorf("报错应点名 FOXSSL_WEBHOOK_SECRET，实际: %v", err)
	}
}

// TestLoadRejectsMockUpstreamInProduction 验证生产环境不允许启用 Mock 上游。
//
// 后果比 Mock 支付渠道更隐蔽：Mock 上游不会真的向 CA 下单，
// 但订单会一路推进到「已签发」，平台把钱收了、证书也「发」了，
// 而那张证书在浏览器里不被信任。全程不报任何错。
func TestLoadRejectsMockUpstreamInProduction(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("FOXSSL_PROVIDER", foxsslProviderMock)

	_, err := Load()
	if err == nil {
		t.Fatal("生产环境启用 Mock 证书上游应被拒绝启动")
	}
	if !strings.Contains(err.Error(), foxsslProviderMock) {
		t.Errorf("报错应点名有问题的上游，实际: %v", err)
	}
}

// TestLoadAllowsRealUpstreamInProduction 是上一条的对照。
//
// 只有 mock 被禁，真实上游在生产环境必须能正常启动——
// 否则这条校验就从「防呆」变成了「挡住上线」。
func TestLoadAllowsRealUpstreamInProduction(t *testing.T) {
	setProductionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("生产环境使用真实上游应当可以启动，实际报错: %v", err)
	}
	if cfg.FoxSSLProvider != "foxssl" {
		t.Errorf("上游标识应为 foxssl，实际 %s", cfg.FoxSSLProvider)
	}
}

// TestLogSummaryRedactsSecrets 验证启动日志不会打印密钥。
//
// 启动摘要会被写进日志、日志会被收集与转发，一旦密钥出现在那里，
// 它就已经离开了「只有服务端知道」的范围。
func TestLogSummaryRedactsSecrets(t *testing.T) {
	setBaseEnv(t)
	const jwtSecret = "test-jwt-secret-at-least-32-characters"
	const paymentSecret = "test-payment-secret-at-least-32-chars"
	const foxsslSecret = "test-foxssl-secret-at-least-32-chars"
	const foxsslAPIKey = "test-foxssl-api-key"

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	summary := cfg.LogSummary()
	for i := 0; i+1 < len(summary); i += 2 {
		value, _ := summary[i+1].(string)
		if value == jwtSecret || value == paymentSecret ||
			value == foxsslSecret || value == foxsslAPIKey {
			t.Errorf("启动摘要里的 %v 泄露了明文密钥", summary[i])
		}
	}

	// 渠道标识不是敏感信息，应当正常出现，便于确认环境用的是哪个渠道
	for _, key := range []string{"payment_provider", "foxssl_provider"} {
		found := false
		for i := 0; i+1 < len(summary); i += 2 {
			if summary[i] == key {
				found = true
			}
		}
		if !found {
			t.Errorf("启动摘要应包含 %s，便于确认环境接的是哪个上游", key)
		}
	}
}
