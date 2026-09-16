import { defineConfig } from 'umi';

export default defineConfig({
  title: 'c-ssl 运营后台',
  npmClient: 'pnpm',

  routes: [
    { path: '/', redirect: '/products' },
    { path: '/products', component: 'Products' },
  ],

  // 开发期把 /api 转发到本地 Go 服务，避免浏览器跨域。
  // 生产由 nginx 承担同样职责。
  proxy: {
    '/api': {
      target: 'http://127.0.0.1:8080',
      changeOrigin: true,
    },
  },
});
