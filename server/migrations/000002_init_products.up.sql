-- 000002_init_products
-- 产品目录与价格表。
--
-- 设计要点：
--   * 产品「能力」（是否支持通配符、IP、重签等）存在 products 表；
--     校验规则（EV 不支持通配符等）在 server/internal/product/rules 中强制，
--     两者互补：表里的值是运营可维护的配置，rules 是无论怎么配都不能违反的硬约束。
--   * cost_price 是上游成本价，属于内部数据，任何面向用户的接口都不得返回。
--   * key_algorithms / dcv_methods 用逗号分隔的枚举串存储，取值受 rules 约束。

CREATE TABLE `products` (
  `id`                       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '产品 ID',
  `name`                     VARCHAR(128)    NOT NULL                COMMENT '产品名称',
  `brand`                    VARCHAR(64)     NOT NULL                COMMENT '证书品牌',
  `validation_type`          VARCHAR(8)      NOT NULL                COMMENT 'dv / ov / ev',
  `wildcard_supported`       TINYINT(1)      NOT NULL DEFAULT 0      COMMENT '是否支持通配符域名',
  `ip_supported`             TINYINT(1)      NOT NULL DEFAULT 0      COMMENT '是否支持 IP 地址',
  `multi_domain_supported`   TINYINT(1)      NOT NULL DEFAULT 0      COMMENT '是否支持多域名 SAN',
  `min_sans`                 INT UNSIGNED    NULL                    COMMENT '包含域名数下限',
  `max_sans`                 INT UNSIGNED    NULL                    COMMENT '可添加域名数上限',
  `key_algorithms`           VARCHAR(64)     NOT NULL                COMMENT '逗号分隔：rsa,ecc',
  `dcv_methods`              VARCHAR(255)    NOT NULL                COMMENT '逗号分隔：dns_txt,dns_cname,http_file,https_file,email',
  `reissue_supported`        TINYINT(1)      NOT NULL DEFAULT 1      COMMENT '是否支持重签',
  `cancel_supported`         TINYINT(1)      NOT NULL DEFAULT 1      COMMENT '是否支持取消',
  `require_organization_info` TINYINT(1)     NOT NULL DEFAULT 0      COMMENT '下单是否必须提交企业信息',
  `recommend_tag`            VARCHAR(32)     NULL                    COMMENT '推荐标签文案',
  `upstream_product_id`      INT UNSIGNED    NULL                    COMMENT 'FoxSSL 侧产品 ID，仅内部使用',
  `status`                   VARCHAR(16)     NOT NULL DEFAULT 'active' COMMENT 'active 上架 / off_shelf 下架',
  `sort_order`               INT             NOT NULL DEFAULT 0      COMMENT '列表排序，值小的在前',
  `created_at`               DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`               DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_products_status_sort` (`status`, `sort_order`),
  KEY `idx_products_validation_type` (`validation_type`),
  KEY `idx_products_brand` (`brand`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='SSL 产品目录';

CREATE TABLE `product_prices` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `product_id`     BIGINT UNSIGNED NOT NULL COMMENT '所属产品',
  `years`          INT UNSIGNED    NOT NULL COMMENT '证书年限',
  `cost_price`     BIGINT          NOT NULL COMMENT '上游成本价，单位分。禁止对外暴露',
  `retail_price`   BIGINT          NOT NULL COMMENT '零售价，单位分',
  `original_price` BIGINT          NULL     COMMENT '划线价，单位分',
  `created_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_product_prices_product_years` (`product_id`, `years`),
  CONSTRAINT `fk_product_prices_product` FOREIGN KEY (`product_id`) REFERENCES `products` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='产品价格，按年限';

-- 初始产品目录。
-- cost_price 为占位值，接入 FoxSSL 真实报价后由运营在后台修正。
INSERT INTO `products`
  (`id`, `name`, `brand`, `validation_type`, `wildcard_supported`, `ip_supported`,
   `multi_domain_supported`, `min_sans`, `max_sans`, `key_algorithms`, `dcv_methods`,
   `reissue_supported`, `cancel_supported`, `require_organization_info`,
   `recommend_tag`, `upstream_product_id`, `status`, `sort_order`)
VALUES
  (1, 'AlphaSSL DV 单域名', 'AlphaSSL', 'dv', 0, 0, 0, NULL, NULL,
   'rsa,ecc', 'dns_txt,dns_cname,http_file,https_file',
   1, 1, 0, NULL, 101, 'active', 10),

  (2, 'GlobalSign DV 通配符', 'GlobalSign', 'dv', 1, 0, 1, 1, 1,
   'rsa,ecc', 'dns_txt,dns_cname,http_file,https_file',
   1, 1, 0, '热销', 102, 'active', 20),

  (3, 'DigiCert Secure Site OV', 'DigiCert', 'ov', 1, 1, 1, 1, 3,
   'rsa,ecc', 'dns_txt,dns_cname,http_file,https_file,email',
   1, 1, 1, '推荐', 103, 'active', 30),

  (4, 'DigiCert Secure Site EV', 'DigiCert', 'ev', 0, 0, 1, 1, 3,
   'rsa', 'dns_txt,dns_cname,http_file,https_file,email',
   1, 1, 1, '高信任', 104, 'active', 40),

  (5, 'Certum DV 通配符', 'Certum', 'dv', 1, 0, 1, 1, 1,
   'rsa,ecc', 'dns_txt,dns_cname',
   1, 1, 0, NULL, 105, 'active', 50),

  (6, '免费 DV 证书', 'Let''s Encrypt', 'dv', 0, 0, 0, NULL, NULL,
   'rsa,ecc', 'dns_txt,http_file',
   0, 0, 0, '免费', 106, 'active', 60);

INSERT INTO `product_prices` (`product_id`, `years`, `cost_price`, `retail_price`, `original_price`)
VALUES
  (1, 1,  12800,  29800,  39900),
  (2, 1,  39800,  89800, 108000),
  (2, 2,  69800, 158000,  NULL),
  (3, 1,  98000, 218000, 268000),
  (3, 2, 178000, 398000,  NULL),
  (4, 1, 268000, 598000, 698000),
  (4, 2, 498000, 998000,  NULL),
  (5, 1,  15800,  36800,  45800),
  (6, 1,      0,      0,   NULL);
