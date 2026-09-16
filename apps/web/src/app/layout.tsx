import type { Metadata } from 'next';
import type { ReactNode } from 'react';

export const metadata: Metadata = {
  title: 'c-ssl 证书平台',
  description: 'SSL 证书选购与签发',
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="zh-CN">
      <body
        style={{
          margin: 0,
          fontFamily:
            '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif',
          background: '#f5f5f5',
          color: '#1f1f1f',
        }}
      >
        {children}
      </body>
    </html>
  );
}
