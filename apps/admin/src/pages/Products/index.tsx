import { ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Tag, Typography } from 'antd';
import type { Product } from '@c-ssl/api-types';

import { apiGet } from '@/services/api';

const validationTypeMeta: Record<string, { text: string; color: string }> = {
  dv: { text: 'DV 域名验证', color: 'default' },
  ov: { text: 'OV 组织验证', color: 'blue' },
  ev: { text: 'EV 扩展验证', color: 'gold' },
};

const dcvMethodLabels: Record<string, string> = {
  dns_txt: 'DNS TXT',
  dns_cname: 'DNS CNAME',
  http_file: 'HTTP 文件',
  https_file: 'HTTPS 文件',
  email: '邮件',
};

/** 后端以「分」为单位返回金额，展示时再换算。 */
function formatPrice(cents: number): string {
  return cents === 0 ? '免费' : `¥${(cents / 100).toFixed(2)}`;
}

const columns: ProColumns<Product>[] = [
  {
    title: 'ID',
    dataIndex: 'id',
    width: 56,
  },
  {
    title: '产品名称',
    dataIndex: 'name',
    ellipsis: true,
  },
  {
    title: '品牌',
    dataIndex: 'brand',
    width: 130,
  },
  {
    title: '验证等级',
    dataIndex: 'validationType',
    width: 130,
    render: (_, record) => {
      const meta = validationTypeMeta[record.validationType];
      return <Tag color={meta?.color}>{meta?.text ?? record.validationType}</Tag>;
    },
  },
  {
    title: '支持能力',
    dataIndex: 'wildcardSupported',
    width: 220,
    render: (_, record) => (
      <>
        {record.wildcardSupported && <Tag>通配符</Tag>}
        {record.ipSupported && <Tag>IP</Tag>}
        {record.multiDomainSupported && <Tag>多域名</Tag>}
        {record.requireOrganizationInfo && <Tag color="purple">需企业信息</Tag>}
        {!record.reissueSupported && <Tag color="red">不可重签</Tag>}
        {!record.cancelSupported && <Tag color="red">不可取消</Tag>}
      </>
    ),
  },
  {
    title: '算法',
    dataIndex: 'keyAlgorithms',
    width: 100,
    render: (_, record) => record.keyAlgorithms.map((a) => a.toUpperCase()).join(' / '),
  },
  {
    title: '验证方式',
    dataIndex: 'dcvMethods',
    width: 240,
    render: (_, record) => (
      <Typography.Text type="secondary">
        {record.dcvMethods.map((m) => dcvMethodLabels[m] ?? m).join('、')}
      </Typography.Text>
    ),
  },
  {
    title: '年限',
    dataIndex: 'years',
    width: 80,
    render: (_, record) => record.years.map((y) => `${y} 年`).join('、'),
  },
  {
    title: '零售价',
    dataIndex: 'prices',
    width: 200,
    render: (_, record) => (
      <>
        {record.prices.map((p) => (
          <div key={p.years}>
            {p.years} 年：{formatPrice(p.retailPrice)}
          </div>
        ))}
      </>
    ),
  },
];

export default function ProductsPage() {
  return (
    <ProTable<Product>
      headerTitle="产品目录"
      rowKey="id"
      columns={columns}
      search={false}
      options={false}
      pagination={false}
      scroll={{ x: 1200 }}
      request={async () => {
        const data = await apiGet<{ items: Product[] }>('/products');
        return { data: data.items, success: true, total: data.items.length };
      }}
    />
  );
}
