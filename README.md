# c-ssl

SSL 证书签发与销售平台。面向个人站长、企业客户和运营人员，覆盖会员、钱包充值、产品目录、证书下单、DCV 域名验证、FoxSSL 上游签发、订单管理与运营后台。

## 技术栈

| 层级 | 选型 |
|---|---|
| 用户站 | Next.js + React + TypeScript |
| 运营后台 | Ant Design Pro + Umi + TypeScript |
| 后端 | Go + chi + database/sql + sqlc |
| 数据 | MySQL 8 + Redis 7 |
| 迁移 | golang-migrate |
| 接口契约 | OpenAPI（唯一真源） |

## 文档

- [Ant Design Pro 与 Go 后端开发实施规划](docs/06-AntDesignPro与Go后端开发实施规划.md) — 项目目标、业务域、分阶段交付与验收标准
- [仓库与目录结构设计](docs/仓库与目录结构设计.md) — 目录树、Go 包规范、契约生成链路、Phase 0 落地清单
- [本地开发环境](docs/本地开发环境.md) — 工具链版本、数据库配置、常用命令、故障排查

## 仓库结构

```text
c-ssl/
├── openapi/    接口契约唯一真源
├── apps/       web（用户站）、admin（运营后台）
├── packages/   api-types、product-rules、ui
├── server/     Go 服务：api、worker、cron
├── scripts/    环境自检、建库、契约生成与校验
├── deploy/     生产编排与镜像
└── docs/       规划与设计文档
```

## 快速开始

```bash
cp .env.example .env     # 首次使用，按需填写
make env-check           # 环境自检：工具链 + 数据库连通性
make help                # 查看全部命令
```

本地依赖使用服务化实例（本机 MySQL 与 Redis），不需要 Docker。`docker-compose.yml` 仅供 CI 使用。

## 当前状态

Phase 0 进行中。环境准备与根配置文件已完成，`openapi/` 契约、`server/` Go 模块与前端应用尚未初始化。
