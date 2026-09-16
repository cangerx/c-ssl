SHELL := /bin/bash
.DEFAULT_GOAL := help

GOBIN      := $(shell go env GOPATH 2>/dev/null)/bin
MIGRATE    := $(shell command -v migrate 2>/dev/null || echo $(GOBIN)/migrate)
MIGRATIONS := server/migrations

MYSQL_DSN   := $(shell [ -f .env ] && grep -E '^MYSQL_DSN=' .env | head -1 | cut -d= -f2-)
# 迁移连接串单独构造：golang-migrate 会把整个 .sql 文件当一条语句执行，
# 必须带 multiStatements=true。应用自身的 DSN 不加这个参数，避免放大注入风险。
MYSQL_DSN_BASE := $(firstword $(subst ?, ,$(MYSQL_DSN)))
MIGRATE_URL    := $(if $(MYSQL_DSN_BASE),mysql://$(MYSQL_DSN_BASE)?multiStatements=true,)
REDIS_DB    := $(shell [ -f .env ] && grep -E '^REDIS_DB=' .env | head -1 | cut -d= -f2-)

# 契约校验脚本需要 PyYAML，优先用项目虚拟环境
PYTHON := $(shell [ -x .venv/bin/python ] && echo .venv/bin/python || echo python3)

# ── 帮助 ──────────────────────────────────────────

.PHONY: help
help: ## 显示所有可用命令
	@printf '\n\033[1mc-ssl 开发命令\033[0m\n'
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@printf '\n'

# ── 环境 ──────────────────────────────────────────

.PHONY: env-check
env-check: ## 环境自检：工具链版本 + 数据库连通性
	@bash scripts/env-check.sh

.PHONY: db-create
db-create: ## 创建开发库、测试库与专用用户（幂等）
	@bash scripts/db-create.sh

.PHONY: db-drop
db-drop: ## 删除开发库与测试库（危险，需 CONFIRM=yes）
	@if [ "$(CONFIRM)" != "yes" ]; then \
	  echo "此操作会删除开发库与测试库，且不可恢复。"; \
	  echo "确认请执行：make db-drop CONFIRM=yes"; exit 1; fi
	@bash scripts/db-drop.sh

.PHONY: redis-flush
redis-flush: ## 清空本项目独占的 Redis 库（危险，需 CONFIRM=yes）
	@if [ "$(CONFIRM)" != "yes" ]; then \
	  echo "此操作会清空 Redis db$(REDIS_DB)。"; \
	  echo "确认请执行：make redis-flush CONFIRM=yes"; exit 1; fi
	@redis-cli -n $(REDIS_DB) FLUSHDB && echo "已清空 Redis db$(REDIS_DB)"

.PHONY: tooling-setup
tooling-setup: ## 创建 Python 工具虚拟环境（契约校验依赖）
	@python3 -m venv .venv && .venv/bin/pip -q install -r scripts/requirements.txt
	@echo "工具环境就绪：.venv/"

.PHONY: spec-check
spec-check: ## 校验 OpenAPI 契约（$ref 完整性 + operationId 唯一性）
	@$(PYTHON) scripts/check-openapi.py

# ── 数据库迁移 ────────────────────────────────────

.PHONY: check-migrations
check-migrations:
	@if [ -z "$(MIGRATE_URL)" ]; then \
	  echo "错误：.env 中 MYSQL_DSN 未配置。请先执行 make db-create。"; exit 1; fi
	@if [ ! -d "$(MIGRATIONS)" ]; then \
	  echo "错误：目录 $(MIGRATIONS) 不存在。Phase 0 尚未生成迁移文件。"; exit 1; fi

.PHONY: migrate-up
migrate-up: check-migrations ## 执行全部未应用的迁移
	@$(MIGRATE) -path $(MIGRATIONS) -database "$(MIGRATE_URL)" up

.PHONY: migrate-down
migrate-down: check-migrations ## 回滚最近一次迁移
	@$(MIGRATE) -path $(MIGRATIONS) -database "$(MIGRATE_URL)" down 1

.PHONY: migrate-version
migrate-version: check-migrations ## 查看当前迁移版本
	@$(MIGRATE) -path $(MIGRATIONS) -database "$(MIGRATE_URL)" version

.PHONY: migrate-force
migrate-force: check-migrations ## 强制设置迁移版本（脏状态修复，需 version=N）
	@if [ -z "$(version)" ]; then echo "用法：make migrate-force version=3"; exit 1; fi
	@$(MIGRATE) -path $(MIGRATIONS) -database "$(MIGRATE_URL)" force $(version)

.PHONY: migrate-create
migrate-create: ## 新建迁移文件（需 name=xxx）
	@if [ -z "$(name)" ]; then echo "用法：make migrate-create name=create_users"; exit 1; fi
	@mkdir -p $(MIGRATIONS)
	@$(MIGRATE) create -ext sql -dir $(MIGRATIONS) -seq $(name)

# ── Go 服务 ───────────────────────────────────────

.PHONY: check-server
check-server:
	@if [ ! -f server/go.mod ]; then \
	  echo "错误：server/go.mod 不存在。Phase 0 尚未初始化 Go 模块。"; exit 1; fi

.PHONY: tidy
tidy: check-server ## 整理 Go 依赖
	@cd server && go mod tidy

.PHONY: lint
lint: check-server ## 静态检查
	@cd server && golangci-lint run ./...

.PHONY: test
test: check-server ## 单元测试
	@cd server && go test ./... -race -count=1

.PHONY: build
build: check-server ## 构建 api / worker / cron 三个二进制
	@cd server && mkdir -p bin && \
	  go build -o bin/api    ./cmd/api && \
	  go build -o bin/worker ./cmd/worker && \
	  go build -o bin/cron   ./cmd/cron && \
	  echo "构建完成：server/bin/"

.PHONY: run-api
run-api: check-server ## 本地启动 API 服务
	@cd server && go run ./cmd/api

.PHONY: run-worker
run-worker: check-server ## 本地启动 Worker
	@cd server && go run ./cmd/worker

.PHONY: run-cron
run-cron: check-server ## 本地启动 Cron
	@cd server && go run ./cmd/cron

# ── 前端 ──────────────────────────────────────────

# 前端命令统一走 `env -u NODE_OPTIONS`：沙箱通过 NODE_OPTIONS 注入的 fs shim
# 只支持递归 mkdir，会让部分包管理操作失败。
# 两个框架都读 PORT 环境变量，本机默认端口被占用时用 PORT=xxx 覆盖。
PORT_ENV := $(if $(PORT),PORT=$(PORT),)

.PHONY: install
install: ## 安装前端 workspace 依赖
	@env -u NODE_OPTIONS pnpm install

.PHONY: gen
gen: ## 由 OpenAPI 契约生成 TypeScript 类型
	@bash scripts/gen-api-types.sh

.PHONY: gen-check
gen-check: ## 校验生成物与契约一致（CI 用）
	@bash scripts/check-generated.sh

.PHONY: web
web: ## 启动用户站（Next.js，默认 3000，PORT=3001 换端口）
	@env -u NODE_OPTIONS $(PORT_ENV) pnpm --filter web exec next dev

.PHONY: admin
admin: ## 启动运营后台（Ant Design Pro，默认 8000，PORT=8001 换端口）
	@env -u NODE_OPTIONS $(PORT_ENV) pnpm --filter admin exec umi dev

.PHONY: typecheck
typecheck: ## 前端类型检查（admin + web）
	@env -u NODE_OPTIONS pnpm --filter admin run lint
	@env -u NODE_OPTIONS pnpm --filter web run lint
	@echo "前端类型检查通过"
