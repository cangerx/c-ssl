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

	// TrustProxy 决定是否采信 X-Forwarded-For 等转发头来判定客户端 IP。
	//
	// 默认关闭：这些头是客户端可以随意伪造的，直接采信会让限流形同虚设。
	// 只有当服务确实部署在可信反向代理之后时才应打开。
	TrustProxy bool

	LogLevel string

	// EnvFile 记录实际加载的 .env 路径，为空表示未加载文件。仅用于启动日志。
	EnvFile string
}

// IsDevelopment 判断是否运行在开发环境。
func (c *Config) IsDevelopment() bool {
	return strings.EqualFold(c.AppEnv, "development")
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
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需配置: %s（检查 .env 或进程环境变量）", strings.Join(missing, ", "))
	}

	// Redis db0 通常被同实例上的其他项目占用，这里主动拦住
	if c.RedisDB == 0 {
		return fmt.Errorf("REDIS_DB 为 0，本地实例的 db0 常被其他项目共用，请改用独立 db index（如 1）")
	}

	if !c.IsDevelopment() && len(c.JWTSecret) < 32 {
		return fmt.Errorf("非开发环境的 JWT_SECRET 长度不足 32 位")
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
