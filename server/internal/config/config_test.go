package config

import (
	"strings"
	"testing"

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
	setBaseEnv(t)
	t.Setenv("APP_ENV", "production")
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
	setBaseEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("PAYMENT_PROVIDER", "wechat")

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

// TestLogSummaryRedactsSecrets 验证启动日志不会打印密钥。
//
// 启动摘要会被写进日志、日志会被收集与转发，一旦密钥出现在那里，
// 它就已经离开了「只有服务端知道」的范围。
func TestLogSummaryRedactsSecrets(t *testing.T) {
	setBaseEnv(t)
	const jwtSecret = "test-jwt-secret-at-least-32-characters"
	const paymentSecret = "test-payment-secret-at-least-32-chars"

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	summary := cfg.LogSummary()
	for i := 0; i+1 < len(summary); i += 2 {
		value, _ := summary[i+1].(string)
		if value == jwtSecret || value == paymentSecret {
			t.Errorf("启动摘要里的 %v 泄露了明文密钥", summary[i])
		}
	}

	// 渠道标识不是敏感信息，应当正常出现，便于确认环境用的是哪个渠道
	found := false
	for i := 0; i+1 < len(summary); i += 2 {
		if summary[i] == "payment_provider" {
			found = true
		}
	}
	if !found {
		t.Error("启动摘要应包含 payment_provider，便于确认环境用的是哪个渠道")
	}
}
