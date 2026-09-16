# c-ssl

SSL 证书签发与销售平台。面向个人站长、企业客户和运营人员，覆盖会员、钱包充值、产品目录、证书下单、DCV 域名验证、FoxSSL 上游签发、订单管理与运营后台。

## 技术栈

| 层级 | 选型 |
|---|---|
| 用户站 | Next.js 15 + React 18 + TypeScript |
| 运营后台 | Ant Design Pro + Umi 4 + TypeScript |
| 后端 | Go 1.26 + chi v5 + `database/sql` |
| 数据 | MySQL（本地 9.6 / CI 8.4）+ Redis 7 |
| 迁移 | golang-migrate |
| 接口契约 | OpenAPI 3.0.3（唯一真源，`openapi-typescript` 生成 TS 类型） |

> 暂未引入 ORM 与代码生成式 SQL 工具，仓储层为手写 `database/sql`。是否引入 sqlc 留待
> 查询复杂度上升后再评估。

## 文档

- [Ant Design Pro 与 Go 后端开发实施规划](docs/06-AntDesignPro与Go后端开发实施规划.md) — 项目目标、业务域、分阶段交付与验收标准
- [仓库与目录结构设计](docs/仓库与目录结构设计.md) — 目录树、Go 包规范、契约生成链路、Phase 0 落地清单
- [本地开发环境](docs/本地开发环境.md) — 工具链版本、数据库配置、常用命令、故障排查

## 仓库结构

目标形态（`product-rules`、`ui`、`deploy` 为后续阶段规划，尚未创建）：

```text
c-ssl/
├── openapi/    接口契约唯一真源
├── apps/       web（用户站）、admin（运营后台）
├── packages/   api-types（已建）、product-rules、ui（规划中）
├── server/     Go 服务：api、worker、cron
├── scripts/    环境自检、建库、契约生成与校验
├── deploy/     生产编排与镜像（规划中）
└── docs/       规划与设计文档
```

## 快速开始

```bash
cp .env.example .env     # 首次使用，按需填写
make env-check           # 环境自检：工具链 + 数据库连通性
make tooling-setup       # 创建 Python 工具环境（契约校验用）
make db-create           # 建库建用户
make migrate-up          # 执行数据库迁移
make run-api             # 启动 API，访问 /health 验证
make help                # 查看全部命令
```

本地依赖使用服务化实例（本机 MySQL 与 Redis），不需要 Docker。`docker-compose.yml` 仅供 CI 使用。

跑测试：

```bash
make test                # 单元测试；需要数据库的用例在测试库不可用时自动跳过
make test-db             # 完整测试：先迁移测试库，保证数据库用例真的执行
```

> 钱包域的并发、幂等、对账用例打真实数据库——行锁、唯一索引与事务回滚的行为
> 用 mock 验证不了。它们跑在独立的 `c_ssl_test` 上，不碰开发数据。

启动前端（另开终端）：

```bash
make admin                # 运营后台 → http://localhost:8000/products
make web                  # 用户站   → http://localhost:3000
make web PORT=3001        # 3000 被占用时换端口
```

> 两个前端都已配置 `/api` 反向代理到 `127.0.0.1:8080`，开发期不需要处理跨域。
> 本机 3000 端口常被其它项目占用，用 `PORT=` 覆盖即可，不要动别人的进程。

## 当前状态

**Phase 0 已完成，Phase 1 进行中。**

已完成：

| 阶段 | 内容 |
|---|---|
| 环境 | 工具链自检、MySQL 建库建用户、Redis 独立 db1、迁移与 lint 工具安装 |
| 配置 | 根配置文件、`Makefile`、CI 工作流（含 MySQL/Redis 依赖服务）、环境变量样例 |
| 契约 | OpenAPI 3.0.3 多文件契约（10 接口、37 处 `$ref`）、`packages/api-types` 类型生成与漂移检测 |
| 后端 | 三入口（api/worker/cron）、配置加载、日志脱敏、MySQL/Redis 探活、统一响应外壳、Trace 透传 |
| 业务 | 产品域垂直切片 + `internal/product/rules` 规则真源；**认证域**（注册/登录/刷新/退出/me）；**钱包域**（余额、不可变账本、冻结/结算/解冻） |
| 前端 | `apps/admin`（ProTable 产品列表）、`apps/web`（产品卡片） |

Phase 1 剩余部分：充值订单与支付回调幂等。

两处关键设计的取舍记录：

- 认证域（Argon2id、刷新令牌轮换与重放检测、双重限流、账号锁定）见
  [仓库与目录结构设计 §11](docs/仓库与目录结构设计.md)
- 钱包域（余额与流水分离、六个操作的方向、幂等键、行锁与间隙锁、
  以及"测试自身也要被验证"的方法）见
  [仓库与目录结构设计 §12](docs/仓库与目录结构设计.md)

接口契约在 `openapi/openapi.yaml`，是前后端字段的唯一真源。
