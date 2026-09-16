-- 000001_init_users
-- 平台会员表。
--
-- 全库约定：
--   * 金额一律 BIGINT 存「分」，不使用 DECIMAL/FLOAT（见 server/internal/domain/money）
--   * 时间统一 DATETIME，避免 TIMESTAMP 的 2038 上限
--   * 口令只存 Argon2id 哈希，永不存明文
--   * 字符集固定 utf8mb4 / utf8mb4_0900_ai_ci，与 MySQL 8.4 生产环境一致

CREATE TABLE `users` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '用户 ID',
  `email`              VARCHAR(254)    NOT NULL                COMMENT '登录邮箱，全局唯一，比较不区分大小写',
  `password_hash`      VARCHAR(255)    NOT NULL                COMMENT 'Argon2id 哈希，含盐与参数',
  `nickname`           VARCHAR(32)     NOT NULL DEFAULT ''     COMMENT '昵称',
  `status`             VARCHAR(16)     NOT NULL DEFAULT 'active' COMMENT 'active 正常 / disabled 禁用',
  `failed_login_count` INT UNSIGNED    NOT NULL DEFAULT 0      COMMENT '连续登录失败次数，成功登录后归零',
  `locked_until`       DATETIME        NULL                    COMMENT '锁定截止时间，NULL 表示未锁定',
  `last_login_at`      DATETIME        NULL                    COMMENT '最近成功登录时间',
  `created_at`         DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_users_email` (`email`),
  KEY `idx_users_status_created_at` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='平台会员';
