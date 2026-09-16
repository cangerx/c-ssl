import type { ApiEnvelope, ErrorCodeValue } from '@c-ssl/api-types';

const API_BASE = '/api/v1';

/** 后端返回的业务错误。code 取值见契约中的业务码说明。 */
export class ApiError extends Error {
  readonly code: ErrorCodeValue;

  constructor(code: ErrorCodeValue, message: string) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
  }
}

/**
 * 后端统一响应外壳。契约里的 ApiEnvelope 只声明 code 与 message，
 * data 由各接口通过 allOf 补齐类型，这里在调用侧把它接上。
 */
type Envelope<T> = ApiEnvelope & { data: T };

/**
 * 发起 GET 请求并拆开统一响应外壳。
 *
 * 后端所有接口都返回 { code, message, data }。业务结果只认 code：
 * code 非 0 即失败，即使 HTTP 状态码是 200；反过来 HTTP 4xx 时
 * 响应体里也一定有可展示的 message。
 */
export async function apiGet<T>(
  path: string,
  params?: Record<string, string | undefined>,
): Promise<T> {
  const url = new URL(`${API_BASE}${path}`, window.location.origin);

  if (params) {
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== '') {
        url.searchParams.set(key, value);
      }
    }
  }

  const response = await fetch(url.toString(), {
    headers: { Accept: 'application/json' },
  });

  let body: Envelope<T>;
  try {
    body = (await response.json()) as Envelope<T>;
  } catch {
    throw new ApiError(5000, `响应不是合法 JSON（HTTP ${response.status}）`);
  }

  if (body.code !== 0) {
    throw new ApiError(body.code as ErrorCodeValue, body.message);
  }

  return body.data;
}
