// Package version 记录构建版本信息。
//
// 通过 ldflags 注入：
//
//	go build -ldflags "-X github.com/cangerx/c-ssl/server/internal/version.Version=1.0.0"
package version

// 这些变量在构建时被覆盖，未覆盖时保留开发默认值。
var (
	// Version 是语义化版本号。
	Version = "0.1.0"
	// Commit 是构建时的 git 提交短哈希。
	Commit = "dev"
	// BuildTime 是构建时间，RFC3339 格式。
	BuildTime = "unknown"
)

// String 返回用于日志与健康检查的版本描述。
func String() string {
	if Commit == "dev" {
		return Version + "-dev"
	}
	return Version + "+" + Commit
}
