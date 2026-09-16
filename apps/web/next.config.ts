import type { NextConfig } from 'next';

const API_BASE = process.env.API_BASE_URL ?? 'http://127.0.0.1:8080';

const nextConfig: NextConfig = {
  // 开发期把 /api 转发到本地 Go 服务，避免浏览器跨域。
  // 生产由 nginx 承担同样职责。
  async rewrites() {
    return [
      {
        source: '/api/:path*',
        destination: `${API_BASE}/api/:path*`,
      },
    ];
  },
};

export default nextConfig;
