-- 000006_init_orders
-- 证书订单、域名验证材料、联系人、企业主体、证书与上游事件。
--
-- 设计取舍：
--
--   * 订单快照产品信息（product_name / brand / validation_type）。
--     产品会改价、会下架、能力字段也会调整，而订单是历史事实。
--     只存 product_id 再联表读，会让三个月前的老订单显示成今天的名字与价格。
--
--   * upstream_order_no 建唯一索引，且允许 NULL。
--     MySQL 的唯一索引允许多个 NULL，所以「尚未提交上游」的订单不会互相冲突，
--     而「同一个上游订单号被两条本地订单引用」会被数据库直接拦住。
--     这是本地侧的第二道闸门；第一道是向上游传本地 order_no 做幂等键。
--
--   * amount 与 cost_price 的 CHECK 是 >= 0 而不是 > 0。
--     免费证书的零售价就是 0，这条路径是真实存在的业务
--     （免费证书不支持重签与取消，见 product/rules）。写成 > 0 会让免费证书
--     根本无法下单，而且报的是数据库约束错误，排查起来很绕。
--
--   * cost_price 是平台付给上游的成本，只用于运营对账与毛利分析。
--     用户扣款依据的是 amount（零售价），两者不做任何联动——
--     把采购成本透传给用户定价会让定价体系失去意义。
--
--   * order_domains 只存**替换过占位符**的 file_path。
--     上游返回的模板含 {FQDN}，平台在写入前就按域名展开。
--     存模板再在读取时替换的话，任何一条忘了替换的读取路径都会把
--     `/.well-known/pki-validation/{FQDN}.txt` 直接展示给用户。
--
--   * certificates 不建在 order_no 上的唯一索引：重签会产生新的证书
--     （新的 cert_id、序列号与有效期），挂在同一个订单下。
--     唯一的是 cert_id。订单表上的 cert_id 是「当前有效那张」的冗余，
--     供列表展示，避免每次翻页都去证书表查一次。
--
--   * webhook_events 刻意不对 certificate_orders 建外键。
--     上游可能推送平台不知道的订单号（订单还没落库、或已被归档），
--     此时事件仍然必须记下来——丢了就再也查不到上游到底推过什么。
--     加了外键会让这类事件插入失败，把排查线索直接丢掉。
--
--   * 所有对 users / products / certificate_orders 的外键都是 RESTRICT：
--     订单与证书是财务与法律凭证，不该因为一次误删而消失。

CREATE TABLE `certificate_orders` (
  `id`                       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT          COMMENT '自增 ID，同时作为分页游标',
  `order_no`                 VARCHAR(32)     NOT NULL                         COMMENT '平台订单号，形如 CS20260917120000A7K3M9',
  `user_id`                  BIGINT UNSIGNED NOT NULL                         COMMENT '下单用户',
  `product_id`               BIGINT UNSIGNED NOT NULL                         COMMENT '产品 ID',
  `product_name`             VARCHAR(128)    NOT NULL                         COMMENT '下单时的产品名快照',
  `brand`                    VARCHAR(64)     NOT NULL                         COMMENT '下单时的品牌快照',
  `validation_type`          VARCHAR(8)      NOT NULL                         COMMENT 'dv / ov / ev，下单时的快照',
  `years`                    INT             NOT NULL                         COMMENT '购买年限',
  `key_algorithm`            VARCHAR(8)      NOT NULL                         COMMENT 'rsa / ecc',
  `amount`                   BIGINT          NOT NULL                         COMMENT '订单金额（分），等于下单时冻结并实扣的零售价；免费证书为 0',
  `cost_price`               BIGINT          NOT NULL DEFAULT 0               COMMENT '上游返回的成本价（分），仅用于运营对账，不参与用户余额计算',
  `status`                   VARCHAR(16)     NOT NULL                         COMMENT '订单状态，取值见 domain/orderstate',
  `upstream_order_no`        VARCHAR(64)     NULL                             COMMENT '上游 FoxSSL 订单号，提交成功前为 NULL',
  `cert_id`                  VARCHAR(64)     NOT NULL DEFAULT ''              COMMENT '当前有效证书编号，签发前为空',
  `upstream_order_status`    VARCHAR(32)     NOT NULL DEFAULT ''              COMMENT '上游订单状态原文',
  `upstream_cert_status`     VARCHAR(32)     NOT NULL DEFAULT ''              COMMENT '上游证书状态原文',
  `upstream_prepare_status`  VARCHAR(32)     NOT NULL DEFAULT ''              COMMENT '上游准备状态原文',
  `upstream_reissue_status`  VARCHAR(32)     NOT NULL DEFAULT ''              COMMENT '上游重签状态原文',
  `csr`                      TEXT            NULL                             COMMENT '证书签名请求。推荐由客户端生成，私钥不上传',
  `failure_reason`           VARCHAR(255)    NOT NULL DEFAULT ''              COMMENT '失败原因，仅 failed 状态有值',
  `cancel_reason`            VARCHAR(255)    NOT NULL DEFAULT ''              COMMENT '取消原因',
  `submitted_at`             DATETIME        NULL                             COMMENT '提交上游的时间',
  `issued_at`                DATETIME        NULL                             COMMENT '签发时间',
  `expires_at`               DATETIME        NULL                             COMMENT '证书到期时间',
  `created_at`               DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`               DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_certificate_orders_order_no` (`order_no`),
  -- 允许 NULL 的唯一索引：未提交的订单不冲突，重复引用同一个上游订单会被拦住
  UNIQUE KEY `uk_certificate_orders_upstream_order_no` (`upstream_order_no`),
  -- 按用户倒序翻页走这个索引
  KEY `idx_certificate_orders_user_id_id` (`user_id`, `id`),
  -- 运营后台按状态筛选，以及「待验证订单有多少」这类查询
  KEY `idx_certificate_orders_status_id` (`status`, `id`),
  CONSTRAINT `ck_certificate_orders_amount_non_negative` CHECK (`amount` >= 0),
  CONSTRAINT `ck_certificate_orders_cost_non_negative` CHECK (`cost_price` >= 0),
  CONSTRAINT `fk_certificate_orders_user_id`
    FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `fk_certificate_orders_product_id`
    FOREIGN KEY (`product_id`) REFERENCES `products` (`id`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='证书订单';

CREATE TABLE `order_domains` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT               COMMENT '自增 ID',
  `order_no`          VARCHAR(32)     NOT NULL                              COMMENT '平台订单号',
  `domain`            VARCHAR(253)    NOT NULL                              COMMENT '域名，通配符形如 *.example.com',
  `is_wildcard`       TINYINT(1)      NOT NULL DEFAULT 0                    COMMENT '是否通配符域名，冗余存储以便按形态查询',
  `is_primary`        TINYINT(1)      NOT NULL DEFAULT 0                    COMMENT '是否主域名（CN）',
  `status`            VARCHAR(16)     NOT NULL                              COMMENT 'pending / verifying / verified / failed / expired',
  `dcv_method`        VARCHAR(16)     NOT NULL DEFAULT ''                   COMMENT '选定的验证方式，未选择时为空',
  `dns_record_type`   VARCHAR(8)      NOT NULL DEFAULT ''                   COMMENT 'TXT / CNAME',
  `dns_record_name`   VARCHAR(255)    NOT NULL DEFAULT ''                   COMMENT 'DNS 记录主机名，由上游指定',
  `dns_record_value`  VARCHAR(512)    NOT NULL DEFAULT ''                   COMMENT 'DNS 记录值',
  -- 只存展开后的路径，不存上游模板。见文件头注释。
  `file_path`         VARCHAR(512)    NOT NULL DEFAULT ''                   COMMENT '文件验证路径，{FQDN} 已替换',
  `file_content`      TEXT            NULL                                  COMMENT '文件验证需要放置的内容',
  `email_addresses`   JSON            NULL                                  COMMENT '邮件验证可用地址列表',
  `verified_at`       DATETIME        NULL                                  COMMENT '验证通过时间',
  `dcv_expires_at`    DATETIME        NULL                                  COMMENT '验证失效时间，过期需重新验证',
  `created_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  -- 同一订单里同一个域名只能出现一次；重复添加是调用方的 bug，不是可以容忍的输入
  UNIQUE KEY `uk_order_domains_order_domain` (`order_no`, `domain`),
  KEY `idx_order_domains_status` (`status`),
  CONSTRAINT `fk_order_domains_order_no`
    FOREIGN KEY (`order_no`) REFERENCES `certificate_orders` (`order_no`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='订单域名与验证材料';

-- 联系人单独成表而不是挂在订单表上：这些是个人信息，
-- 单独一张表才能在需要时按范围清理或脱敏，而订单本身必须长期保留。
CREATE TABLE `order_contacts` (
  `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT                     COMMENT '自增 ID',
  `order_no`    VARCHAR(32)     NOT NULL                                    COMMENT '平台订单号',
  `name`        VARCHAR(64)     NOT NULL                                    COMMENT '联系人姓名',
  `email`       VARCHAR(128)    NOT NULL                                    COMMENT '联系人邮箱，CA 审核会用它联系',
  `phone`       VARCHAR(32)     NOT NULL                                    COMMENT '联系电话，含国家码',
  `title`       VARCHAR(64)     NOT NULL DEFAULT ''                         COMMENT '职位',
  `created_at`  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_order_contacts_order_no` (`order_no`),
  CONSTRAINT `fk_order_contacts_order_no`
    FOREIGN KEY (`order_no`) REFERENCES `certificate_orders` (`order_no`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='订单联系人';

-- 同样单独成表。只有 OV / EV 才有记录，DV 订单不写这一行。
CREATE TABLE `order_orgs` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT                COMMENT '自增 ID',
  `order_no`         VARCHAR(32)     NOT NULL                               COMMENT '平台订单号',
  `name`             VARCHAR(128)    NOT NULL                               COMMENT '企业注册全称',
  `registration_no`  VARCHAR(64)     NOT NULL                               COMMENT '统一社会信用代码或等效注册号',
  `country`          VARCHAR(2)      NOT NULL                               COMMENT 'ISO 3166-1 两位国家代码',
  `province`         VARCHAR(64)     NOT NULL DEFAULT ''                    COMMENT '省 / 州',
  `city`             VARCHAR(64)     NOT NULL DEFAULT ''                    COMMENT '城市',
  `address`          VARCHAR(255)    NOT NULL DEFAULT ''                    COMMENT '注册地址',
  `postal_code`      VARCHAR(16)     NOT NULL DEFAULT ''                    COMMENT '邮编',
  `phone`            VARCHAR(32)     NOT NULL DEFAULT ''                    COMMENT '企业联系电话',
  `created_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_order_orgs_order_no` (`order_no`),
  CONSTRAINT `fk_order_orgs_order_no`
    FOREIGN KEY (`order_no`) REFERENCES `certificate_orders` (`order_no`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='订单企业主体信息';

CREATE TABLE `certificates` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT                  COMMENT '自增 ID',
  `order_no`       VARCHAR(32)     NOT NULL                                 COMMENT '平台订单号',
  `cert_id`        VARCHAR(64)     NOT NULL                                 COMMENT '上游证书编号',
  `status`         VARCHAR(32)     NOT NULL                                 COMMENT '上游证书状态原文，平台不翻译',
  `common_name`    VARCHAR(253)    NOT NULL                                 COMMENT '证书主域名',
  `domains`        JSON            NOT NULL                                 COMMENT '证书覆盖的全部域名，含 SAN',
  `key_algorithm`  VARCHAR(8)      NOT NULL                                 COMMENT 'rsa / ecc',
  `serial_number`  VARCHAR(128)    NOT NULL DEFAULT ''                      COMMENT '证书序列号',
  `certificate`    MEDIUMTEXT      NULL                                     COMMENT '服务器证书 PEM',
  `ca_bundle`      MEDIUMTEXT      NULL                                     COMMENT 'CA 根证书链 PEM',
  `issued_at`      DATETIME        NULL                                     COMMENT '签发时间',
  `expires_at`     DATETIME        NULL                                     COMMENT '到期时间',
  `created_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  -- 唯一的是证书编号，不是订单号：重签会在同一订单下产生第二张证书
  UNIQUE KEY `uk_certificates_cert_id` (`cert_id`),
  KEY `idx_certificates_order_no_id` (`order_no`, `id`),
  -- 供「即将到期的证书」提醒任务使用
  KEY `idx_certificates_expires_at` (`expires_at`),
  CONSTRAINT `fk_certificates_order_no`
    FOREIGN KEY (`order_no`) REFERENCES `certificate_orders` (`order_no`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='已签发证书';

CREATE TABLE `webhook_events` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT              COMMENT '自增 ID',
  `provider`           VARCHAR(16)     NOT NULL                             COMMENT '上游标识，当前只有 foxssl',
  `event_key`          VARCHAR(200)    NOT NULL                             COMMENT '幂等键：provider:event:上游单号:payload 哈希',
  `event_type`         VARCHAR(32)     NOT NULL                             COMMENT '事件类型原文',
  `upstream_order_no`  VARCHAR(64)     NOT NULL                             COMMENT '上游订单号。刻意不加外键，见文件头注释',
  `upstream_status`    VARCHAR(32)     NOT NULL DEFAULT ''                  COMMENT '事件携带的上游状态原文',
  `payload_hash`       CHAR(64)        NOT NULL                             COMMENT '原始报文 SHA-256（十六进制）',
  `payload`            TEXT            NULL                                 COMMENT '原始报文，截断后存储',
  `occurred_at`        DATETIME        NULL                                 COMMENT '事件在上游产生的时间',
  `received_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP   COMMENT '平台接收时间',
  `process_status`     VARCHAR(16)     NOT NULL DEFAULT 'pending'           COMMENT 'pending / done / failed',
  `process_error`      VARCHAR(255)    NOT NULL DEFAULT ''                  COMMENT '处理失败原因',
  `attempts`           INT             NOT NULL DEFAULT 0                   COMMENT '处理尝试次数',
  `processed_at`       DATETIME        NULL                                 COMMENT '处理完成时间',
  PRIMARY KEY (`id`),
  -- 幂等闸门。上游会重推，同一条事件只能生效一次。
  UNIQUE KEY `uk_webhook_events_event_key` (`event_key`),
  -- 供 Worker 拉取待处理事件
  KEY `idx_webhook_events_process_status_id` (`process_status`, `id`),
  KEY `idx_webhook_events_upstream_order_no` (`upstream_order_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='上游事件回调记录';
