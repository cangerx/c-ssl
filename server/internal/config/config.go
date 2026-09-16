// Package config 加载并校验运行配置。
//
// 配置来源优先级：进程环境变量 > 仓库根目录的 .env 文件。
// 也就是说，CI 与生产通过环境变量注入的值不会被 .env 覆盖。
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/logger"
)

// Config 是服务运行所需的全部配置。
type Config struct {
	AppEnv     string
	AppPort    string
	AppBaseURL string

	MySQLDSN     string
	MySQLTestDSN string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	JWTSecret     string
	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	// PaymentProvider 是默认支付渠道标识，用户未指定渠道时使用。
	PaymentProvider string
	// PaymentWebhookSecret 是支付渠道回调的验签密钥。
	//
	// 与 FoxSSL 的 Webhook 密钥分开配置：两个上游的密钥轮换节奏不同，
	// 共用一个意味着轮换其中一个会同时打断另一个。
	PaymentWebhookSecret string

	// TrustProxy 决定是否采信 X-Forwarded-For 等转发头来判定客户端 IP。
	//
	// 默认关闭：这些头是客户端可以随意伪造的，直接采信会让限流形同虚设。
	// 只有当服务确实部署在可信反向代理之后时才应打开。
	TrustProxy bool

	LogLevel string

	// EnvFile 记录实际加载的 .env 路径，为空表示未加载文件。仅用于启动日志。
	EnvFile string
}

// paymentProviderMock 与 upstream/payment 的 NameMock 一致。
//
// 这里刻意写字面量而不是导入 payment 包：config 是底层包，
// 让配置去依赖业务上游包会把依赖方向倒过来。
// 两边取值不一致由 TestMockProviderNameMatches 兜住。
const paymentProviderMock = "mock"

// IsDevelopment 判断是否运行在开发环境。
func (c *Config) IsDevelopment() bool {
	return strings.EqualFold(c.AppEnv, "development")
}

// IsProduction 判断是否运行在生产环境。
//
// 与 IsDevelopment 分开而不是用 !IsDevelopment：中间的过渡环境（staging、
// 预发）既不是开发也不是生产，用取反会把它们一并当成生产，
// 于是「只在生产生效的严格校验」会在预发环境意外拦住部署。
func (c *Config) IsProduction() bool {
	return strings.EqualFold(c.AppEnv, "production")
}

// Addr 返回 HTTP 服务监听地址。
func (c *Config) Addr() string { return ":" + c.AppPort }

// Load 读取配置。若进程环境变量缺失，会尝试从最近的 .env 补齐。
func Load() (*Config, error) {
	envFile, err := loadDotEnv()
	if err != nil {
		return nil, fmt.Errorf("加载 .env 失败: %w", err)
	}

	cfg := &Config{
		AppEnv:     getEnv("APP_ENV", "development"),
		AppPort:    getEnv("APP_PORT", "8080"),
		AppBaseURL: getEnv("APP_BASE_URL", "http://localhost:8080"),

		MySQLDSN:     getEnv("MYSQL_DSN", ""),
		MySQLTestDSN: getEnv("MYSQL_TEST_DSN", ""),

		RedisAddr:     getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),

		JWTSecret: getEnv("JWT_SECRET", ""),

		PaymentProvider:      getEnv("PAYMENT_PROVIDER", paymentProviderMock),
		PaymentWebhookSecret: getEnv("PAYMENT_WEBHOOK_SECRET", ""),

		TrustProxy: getEnvBool("TRUST_PROXY", false),

		LogLevel: getEnv("LOG_LEVEL", "info"),
		EnvFile:  envFile,
	}

	if cfg.RedisDB, err = getEnvInt("REDIS_DB", 1); err != nil {
		return nil, err
	}
	if cfg.JWTAccessTTL, err = getEnvDuration("JWT_ACCESS_TTL", 15*time.Minute); err != nil {
		return nil, err
	}
	if cfg.JWTRefreshTTL, err = getEnvDuration("JWT_REFRESH_TTL", 168*time.Hour); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var missing []string

	if c.MySQLDSN == "" {
		missing = append(missing, "MYSQL_DSN")
	}
	if c.RedisAddr == "" {
		missing = append(missing, "REDIS_ADDR")
	}
	if c.JWTSecret == "" {
		missing = append(missing, "JWT_SECRET")
	}
	if c.PaymentWebhookSecret == "" {
		missing = append(missing, "PAYMENT_WEBHOOK_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需配置: %s（检查 .env 或进程环境变量）", strings.Join(missing, ", "))
	}

	// Redis db0 通常被同实例上的其他项目占用，这里主动拦住
	if c.RedisDB == 0 {
		return fmt.Errorf("REDIS_DB 为 0，本地实例的 db0 常被其他项目共用，请改用独立 db index（如 1）")
	}

	if !c.IsDevelopment() {
		if len(c.JWTSecret) < 32 {
			return fmt.Errorf("非开发环境的 JWT_SECRET 长度不足 32 位")
		}
		// 密钥短到可以被暴力枚举时，验签就只是一道装饰：
		// 攻击者能自己算出合法签名，然后伪造回调给任意账户充值。
		if len(c.PaymentWebhookSecret) < 32 {
			return fmt.Errorf("非开发环境的 PAYMENT_WEBHOOK_SECRET 长度不足 32 位")
		}
	}

	// Mock 渠道不会真的收钱：它生成的支付地址是本地页面，回调也由本服务自己发出。
	// 在生产环境启用等于给所有用户免费充值，因此直接拒绝启动。
	if c.IsProduction() && c.PaymentProvider == paymentProviderMock {
		return fmt.Errorf("生产环境不能启用 %s 支付渠道，请配置真实的 PAYMENT_PROVIDER", paymentProviderMock)
	}

	return nil
}

// LogSummary 返回可安全写入日志的配置摘要，敏感字段已脱敏。
func (c *Config) LogSummary() []any {
	return []any{
		"app_env", c.AppEnv,
		"app_port", c.AppPort,
		"app_base_url", c.AppBaseURL,
		"redis_addr", c.RedisAddr,
		"redis_db", c.RedisDB,
		"jwt_secret", logger.Redact(c.JWTSecret),
		"payment_provider", c.PaymentProvider,
		"payment_webhook_secret", logger.Redact(c.PaymentWebhookSecret),
		"log_level", c.LogLevel,
		"env_file", c.EnvFile,
	}
}

// ── .env 加载 ──────────────────────────────────────

// LoadDotEnv 把最近的 .env 加载进进程环境变量（已存在的变量不覆盖），
// 返回实际加载的文件路径，未找到时返回空串。
//
// 供测试与脚本使用：测试进程的工作目录是各自的包目录，需要向上查找仓库根目录。
// 服务启动走 Load，它内部也会调用本函数。
func LoadDotEnv() (string, error) { return loadDotEnv() }

// loadDotEnv 从当前目录向上查找 .env，最多 4 层。
// 返回找到的文件路径；未找到返回空串且不报错。
func loadDotEnv() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for range 4 {
		path := filepath.Join(dir, ".env")
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			if err := applyEnvFile(path); err != nil {
				return path, err
			}
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", nil
}

// applyEnvFile 逐行解析 .env。已存在的环境变量不会被覆盖。
//
// 这里刻意不引入 godotenv：解析规则简单，且必须避免对值做 shell 展开——
// DSN 里含有 & 等字符，交给 shell 解析会被当成命令分隔符。
func applyEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("设置 %s 失败: %w", key, err)
			}
		}
	}
	return scanner.Err()
}

// ── 取值辅助 ──────────────────────────────────────

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，当前值 %q", key, raw)
	}
	return v, nil
}

func getEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是合法时长（如 15m、168h），当前值 %q", key, raw)
	}
	return v, nil
}

// getEnvBool 解析布尔配置。接受 1/true/yes/on 与 0/false/no/off，不区分大小写。
func getEnvBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
