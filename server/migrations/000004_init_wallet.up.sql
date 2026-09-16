-- 000004_init_wallet
-- 钱包账户与不可变账本。
--
-- 设计取舍：
--   * 余额拆成 available_balance（可用）与 frozen_balance（冻结）两列，
--     而不是每次由流水累加推算。读余额是最频繁的操作，必须 O(1)；
--     流水累加值只用于对账校验，不作为读取来源。
--   * wallet_ledger 只追加：写入后不允许 UPDATE / DELETE。
--     这里刻意没有用触发器强制——实测被权限挡住：开启 binary logging 的实例上
--     创建触发器需要 SUPER 或 log_bin_trust_function_creators=1，应用账号都没有。
--     而迁移必须在本地、CI、生产任何环境都能跑通，不能依赖 DBA 改全局变量。
--     因此只追加由应用层保证：wallet.Repository 不提供任何更新/删除方法，
--     并由 repository_test.go 的 TestLedgerNeverRewritesHistory 做行为兜底。
--   * 每笔流水同时记录 available_delta 与 frozen_delta。
--     只记一个带正负的 amount 无法表达「冻结」：可用减少、冻结增加，
--     总额不变，方向是二义的。
--   * 幂等键 entry_no 建唯一索引，重复请求由数据库拦下，
--     不依赖应用层「先查再插」——那中间存在竞态窗口。
--   * CHECK 约束是最后一道防线：即使应用层算错，也不可能写出负余额。

CREATE TABLE `wallet_accounts` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT            COMMENT '钱包账户 ID',
  `user_id`           BIGINT UNSIGNED NOT NULL                           COMMENT '所属用户',
  `available_balance` BIGINT          NOT NULL DEFAULT 0                 COMMENT '可用余额，单位分',
  `frozen_balance`    BIGINT          NOT NULL DEFAULT 0                 COMMENT '冻结余额，单位分',
  `version`           BIGINT UNSIGNED NOT NULL DEFAULT 0                 COMMENT '余额变更次数，每次写入 +1，仅用于观测并发写入是否如预期',
  `created_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_wallet_accounts_user_id` (`user_id`),
  -- 负余额在业务上不可能出现。应用层已做判断，这里是兜底：
  -- 一旦触发说明代码有 bug，宁可让事务失败也不要写坏数据。
  CONSTRAINT `ck_wallet_accounts_non_negative`
    CHECK (`available_balance` >= 0 AND `frozen_balance` >= 0),
  CONSTRAINT `fk_wallet_accounts_user_id`
    FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='钱包账户';

CREATE TABLE `wallet_ledger` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT             COMMENT '流水号，全局递增，也是分页游标',
  `account_id`       BIGINT UNSIGNED NOT NULL                            COMMENT '所属钱包账户',
  `user_id`          BIGINT UNSIGNED NOT NULL                            COMMENT '所属用户，冗余存储以便按用户直接查流水',
  `entry_no`         VARCHAR(96)     NOT NULL                            COMMENT '幂等键，全局唯一',
  `op`               VARCHAR(16)     NOT NULL                            COMMENT 'recharge 充值 / consume 扣款 / freeze 冻结 / settle 结算 / unfreeze 解冻 / refund 退款',
  `biz_type`         VARCHAR(32)     NOT NULL                            COMMENT '业务类型，如 recharge_order、cert_order',
  `biz_no`           VARCHAR(64)     NOT NULL                            COMMENT '业务单号',
  `available_delta`  BIGINT          NOT NULL                            COMMENT '可用余额变动，单位分，可为负',
  `frozen_delta`     BIGINT          NOT NULL                            COMMENT '冻结余额变动，单位分，可为负',
  `available_after`  BIGINT          NOT NULL                            COMMENT '记账后的可用余额，单位分',
  `frozen_after`     BIGINT          NOT NULL                            COMMENT '记账后的冻结余额，单位分',
  `remark`           VARCHAR(255)    NOT NULL DEFAULT ''                 COMMENT '备注',
  `created_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_wallet_ledger_entry_no` (`entry_no`),
  -- 按账户倒序翻页走这个索引
  KEY `idx_wallet_ledger_account_id_id` (`account_id`, `id`),
  KEY `idx_wallet_ledger_user_id_id` (`user_id`, `id`),
  -- 按业务单号追溯"这笔订单都动了哪些账"
  KEY `idx_wallet_ledger_biz` (`biz_type`, `biz_no`),
  -- 两个 delta 同时为 0 的流水没有任何意义，只会在对账时制造噪音
  CONSTRAINT `ck_wallet_ledger_not_empty`
    CHECK (`available_delta` <> 0 OR `frozen_delta` <> 0),
  CONSTRAINT `fk_wallet_ledger_account_id`
    -- 显式写出 RESTRICT（也是默认行为）：有账本历史的账户不允许被删除。
    -- 账户对用户是 CASCADE，两者叠加的实际效果是
    -- 「没有资金往来的用户可以被彻底删除，有过的则删不掉」——这是刻意的，
    -- 资金数据不该因为一次误删用户而消失。
    FOREIGN KEY (`account_id`) REFERENCES `wallet_accounts` (`id`) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='钱包账本（只追加）';
