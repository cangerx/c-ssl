/**
 * 由 openapi/openapi.yaml 生成的接口类型。
 *
 * 本文件为生成物，请勿手工修改。修改接口请改 openapi/ 下的契约文件，
 * 然后执行 `make gen`。
 *
 * 用法：
 *
 * ```ts
 * import type { Product, ProductListResponse } from '@c-ssl/api-types';
 *
 * const res: ProductListResponse = await fetch('/api/v1/products').then((r) => r.json());
 * const items: Product[] = res.data.items;
 * ```
 */

export type { components, paths, operations } from './generated/schema';

import type { components } from './generated/schema';

type Schemas = components['schemas'];

// 通用外壳
export type ApiEnvelope = Schemas['ApiEnvelope'];
export type ErrorResponse = Schemas['ErrorResponse'];
export type HealthStatus = Schemas['HealthStatus'];
export type DependencyStatus = Schemas['DependencyStatus'];

// 会员与认证
export type User = Schemas['User'];
export type RegisterRequest = Schemas['RegisterRequest'];
export type LoginRequest = Schemas['LoginRequest'];
export type RefreshRequest = Schemas['RefreshRequest'];
export type AuthTokens = Schemas['AuthTokens'];
export type AuthTokensResponse = Schemas['AuthTokensResponse'];
export type MeResponse = Schemas['MeResponse'];

// 产品
export type Product = Schemas['Product'];
export type ProductPrice = Schemas['ProductPrice'];
export type DcvMethod = Schemas['DcvMethod'];
export type KeyAlgorithm = Schemas['KeyAlgorithm'];
export type ProductListResponse = Schemas['ProductListResponse'];
export type ProductDetailResponse = Schemas['ProductDetailResponse'];

/**
 * 业务错误码取值。与 openapi/components/schemas/common.yaml 的说明保持一致，
 * 也与 server/internal/domain/errs 的 Code 常量对应。
 *
 * 契约里的错误码是文档描述而非 enum（便于后续扩充），这里登记为字面量联合类型，
 * 好处是纯类型、无运行时代码——前端打包器不需要为本包做额外转译配置。
 */
export type ErrorCodeValue =
  | 0 // 成功
  | 1000 // 参数校验失败
  | 1001 // 未认证
  | 1002 // 无权限
  | 1003 // 资源不存在
  | 1004 // 请求过于频繁
  | 2000 // 余额不足
  | 2001 // 订单状态不允许该操作
  | 3000 // 上游服务错误
  | 5000; // 服务内部错误
