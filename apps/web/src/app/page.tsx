import type { Product, ProductListResponse } from '@c-ssl/api-types';

// 服务端组件直接请求 Go 服务；浏览器侧请求走 next.config.ts 里的 rewrites 代理。
const API_BASE = process.env.API_BASE_URL ?? 'http://127.0.0.1:8080';

const dcvMethodLabels: Record<string, string> = {
  dns_txt: 'DNS TXT',
  dns_cname: 'DNS CNAME',
  http_file: 'HTTP 文件',
  https_file: 'HTTPS 文件',
  email: '邮件',
};

const validationTypeLabels: Record<string, string> = {
  dv: 'DV 域名验证',
  ov: 'OV 组织验证',
  ev: 'EV 扩展验证',
};

async function fetchProducts(): Promise<Product[]> {
  const response = await fetch(`${API_BASE}/api/v1/products`, { cache: 'no-store' });
  if (!response.ok) {
    throw new Error(`接口返回 HTTP ${response.status}`);
  }

  const body = (await response.json()) as ProductListResponse;
  if (body.code !== 0) {
    throw new Error(body.message);
  }
  return body.data.items;
}

function formatPrice(cents: number): string {
  return cents === 0 ? '免费' : `¥${(cents / 100).toFixed(2)}`;
}

export default async function HomePage() {
  let products: Product[] = [];
  let error: string | null = null;

  try {
    products = await fetchProducts();
  } catch (cause) {
    error = cause instanceof Error ? cause.message : String(cause);
  }

  return (
    <main style={{ maxWidth: 1120, margin: '0 auto', padding: '48px 24px' }}>
      <h1 style={{ fontSize: 28, fontWeight: 600, margin: '0 0 8px' }}>SSL 证书</h1>
      <p style={{ color: '#666', margin: '0 0 32px' }}>
        产品与可用规则由后端下发，前端不做规则推导。
      </p>

      {error && (
        <div
          style={{
            padding: 16,
            borderRadius: 8,
            background: '#fff2f0',
            border: '1px solid #ffccc7',
            color: '#a8071a',
          }}
        >
          加载失败：{error}
          <div style={{ fontSize: 12, color: '#8c8c8c', marginTop: 8 }}>
            请确认 Go 服务已启动（make run-api）。
          </div>
        </div>
      )}

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fill, minmax(320px, 1fr))',
          gap: 16,
        }}
      >
        {products.map((product) => (
          <article
            key={product.id}
            style={{
              background: '#fff',
              borderRadius: 8,
              padding: 20,
              border: '1px solid #f0f0f0',
            }}
          >
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
              <h2 style={{ fontSize: 16, fontWeight: 600, margin: 0 }}>{product.name}</h2>
              {product.recommendTag && (
                <span
                  style={{
                    fontSize: 12,
                    padding: '2px 8px',
                    borderRadius: 10,
                    background: '#e6f4ff',
                    color: '#0958d9',
                  }}
                >
                  {product.recommendTag}
                </span>
              )}
            </div>

            <div style={{ fontSize: 13, color: '#8c8c8c', margin: '6px 0 12px' }}>
              {product.brand} · {validationTypeLabels[product.validationType] ?? product.validationType}
            </div>

            <div style={{ fontSize: 13, color: '#595959', lineHeight: 1.9 }}>
              <div>验证方式：{product.dcvMethods.map((m) => dcvMethodLabels[m] ?? m).join('、')}</div>
              <div>密钥算法：{product.keyAlgorithms.map((a) => a.toUpperCase()).join(' / ')}</div>
              <div>
                支持：
                {[
                  product.wildcardSupported && '通配符',
                  product.ipSupported && 'IP',
                  product.multiDomainSupported && '多域名',
                ]
                  .filter(Boolean)
                  .join('、') || '单域名'}
              </div>
            </div>

            <div style={{ marginTop: 16, borderTop: '1px solid #f0f0f0', paddingTop: 12 }}>
              {product.prices.map((price) => (
                <div
                  key={price.years}
                  style={{ display: 'flex', justifyContent: 'space-between', fontSize: 14 }}
                >
                  <span style={{ color: '#8c8c8c' }}>{price.years} 年</span>
                  <span style={{ fontWeight: 600 }}>{formatPrice(price.retailPrice)}</span>
                </div>
              ))}
            </div>
          </article>
        ))}
      </div>

      {!error && products.length === 0 && (
        <p style={{ color: '#8c8c8c' }}>暂无上架产品。</p>
      )}
    </main>
  );
}
