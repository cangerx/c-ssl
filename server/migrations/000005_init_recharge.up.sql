-- 000005_init_recharge
-- 充值订单与支付渠道流水。
--
-- 设计取舍：
--   * 幂等键落在 payment_transactions 的 (channel, channel_trade_no) 唯一索引上，
--     而不是 recharge_orders。理由：渠道交易号标识的是「渠道侧的一次支付」，
--     订单标识的是「平台上的一次充值意图」，两者不是一对一——
--     同一张订单可能先有一次失败支付、再有一次成功支付，
--     若把唯一索引建在订单表上就没地方放那个失败的交易号。
--   * recharge_orders.channel_trade_no 因此只记录「成功那笔」的交易号，
--     不加唯一约束，仅用于排查与对账。
--   * 唯一索引是幂等闸门，不是「先查再插」：先查再插中间有竞态窗口，
--     并发的两次重复回调会双双通过检查，然后重复加款。
--   * payment_transactions 只记录验签通过的回调。验签失败的请求不落库——
--     攻击者可以拿伪造签名的报文把任意交易号写进这张表，
--     后续真实回调就会撞上唯一索引而被判成「重放」，白白丢掉一笔真实入账。
--     验签失败只记日志（含来源 IP），用于安全监控。
--   * payment_transactions.order_no 指向 recharge_orders.order_no 而不是 id：
--     回调报文里带的是单号，不是自增 ID，用它做外键可以直接被数据库校验。
--   * 两个金额列都有 CHECK (amount > 0)：0 元或负金额的充值单没有业务含义，
--     一旦出现说明调用方算错了，宁可让插入失败。
--   * 两个表对 users 都是 RESTRICT：充值订单是财务记录，
--     不该因为一次误删用户而消失。

CREATE TABLE `recharge_orders` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT            COMMENT '自增 ID，同时作为分页游标',
  `order_no`          VARCHAR(32)     NOT NULL                           COMMENT '平台充值单号，形如 RC20260916191234A7K3M9',
  `user_id`           BIGINT UNSIGNED NOT NULL                           COMMENT '下单用户',
  `amount`            BIGINT          NOT NULL                           COMMENT '充值金额，单位分',
  `status`            VARCHAR(16)     NOT NULL                           COMMENT 'pending 待支付 / paid 已支付 / failed 支付失败 / closed 已关闭',
  `channel`           VARCHAR(16)     NOT NULL                           COMMENT '支付渠道标识',
  `channel_order_no`  VARCHAR(64)     NOT NULL DEFAULT ''                COMMENT '渠道侧订单号',
  `channel_trade_no`  VARCHAR(64)     NOT NULL DEFAULT ''                COMMENT '成功支付的渠道交易号，仅用于排查与对账',
  `pay_url`           VARCHAR(512)    NOT NULL DEFAULT ''                COMMENT '支付跳转地址',
  `paid_at`           DATETIME        NULL                               COMMENT '支付成功时间',
  `expires_at`        DATETIME        NOT NULL                           COMMENT '过期时间，过期后回调不再入账',
  `remark`            VARCHAR(255)    NOT NULL DEFAULT ''                COMMENT '备注',
  `created_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_recharge_orders_order_no` (`order_no`),
  -- 按用户倒序翻页走这个索引
  KEY `idx_recharge_orders_user_id_id` (`user_id`, `id`),
  -- 供后续的「关闭超时未支付订单」任务使用；也让「待支付订单有多少」这类
  -- 运营查询不必全表扫描
  KEY `idx_recharge_orders_status_expires_at` (`status`, `expires_at`),
  CONSTRAINT `ck_recharge_orders_amount_positive` CHECK (`amount` > 0),
  CONSTRAINT `fk_recharge_orders_user_id`
    FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='用户充值订单';

CREATE TABLE `payment_transactions` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT             COMMENT '自增 ID',
  `channel`          VARCHAR(16)     NOT NULL                            COMMENT '支付渠道标识',
  `channel_trade_no` VARCHAR(64)     NOT NULL                            COMMENT '渠道交易号',
  `order_no`         VARCHAR(32)     NOT NULL                            COMMENT '平台充值单号',
  `user_id`          BIGINT UNSIGNED NOT NULL                            COMMENT '所属用户，冗余存储以便按用户直接查',
  `amount`           BIGINT          NOT NULL                            COMMENT '渠道回执金额，单位分',
  `status`           VARCHAR(16)     NOT NULL                            COMMENT 'success 支付成功 / failed 支付失败',
  `paid_at`          DATETIME        NULL                                COMMENT '渠道记录的支付时间',
  `raw_payload`      TEXT            NULL                                COMMENT '渠道原始报文，应用层截断后写入，供对账与争议排查',
  `created_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  -- 幂等闸门。同一个渠道的同一个交易号只能被记录一次，
  -- 重复回调在这里被数据库拦下，不依赖应用层的检查。
  UNIQUE KEY `uk_payment_transactions_channel_trade` (`channel`, `channel_trade_no`),
  KEY `idx_payment_transactions_order_no` (`order_no`),
  CONSTRAINT `ck_payment_transactions_amount_positive` CHECK (`amount` > 0),
  -- 指向单号而不是 id：回调报文里带的是单号，用它做外键可以直接被数据库校验。
  -- 效果是「不存在订单的渠道流水写不进来」，应用层的订单存在性检查是第二道。
  CONSTRAINT `fk_payment_transactions_order_no`
    FOREIGN KEY (`order_no`) REFERENCES `recharge_orders` (`order_no`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='支付渠道流水';
