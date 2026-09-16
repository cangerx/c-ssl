-- 000003_init_user_sessions
-- 用户会话表：承载刷新令牌的状态。
--
-- 设计取舍：
--   * 刷新令牌必须可撤销（退出登录、改密、管理员封禁），因此状态放服务端，
--     不用无状态 JWT。代价是每次刷新要查一次库，这个开销可以接受。
--   * 只存密钥的 SHA-256，不存原始令牌。库被读走也无法直接冒用。
--     这里不用 Argon2：密钥是 32 字节高熵随机串，不存在字典攻击，
--     而刷新是高频操作，慢哈希只会白白增加延迟。
--   * session_id 是公开标识，明文存储并建唯一索引，用于查库定位会话；
--     它不是秘密，泄露它无法伪造令牌。
--   * 过期会话由 cron 清理，但业务侧仍必须校验 expires_at——
--     不能假设清理任务一定跑过。

CREATE TABLE `user_sessions` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT             COMMENT '会话 ID',
  `user_id`        BIGINT UNSIGNED NOT NULL                            COMMENT '所属用户',
  `session_id`     VARCHAR(32)     NOT NULL                            COMMENT '刷新令牌的公开标识，用于定位会话',
  `secret_hash`    BINARY(32)      NOT NULL                            COMMENT '刷新令牌密钥的 SHA-256，绝不存原始令牌',
  `device_label`   VARCHAR(128)    NOT NULL DEFAULT ''                 COMMENT '设备标识，取自 User-Agent 摘要',
  `client_ip`      VARCHAR(45)     NOT NULL DEFAULT ''                 COMMENT '签发时的客户端 IP，长度兼容 IPv6',
  `issued_at`      DATETIME        NOT NULL                            COMMENT '签发时间',
  `expires_at`     DATETIME        NOT NULL                            COMMENT '过期时间',
  `last_used_at`   DATETIME        NULL                                COMMENT '最近一次成功刷新的时间',
  `revoked_at`     DATETIME        NULL                                COMMENT '撤销时间，NULL 表示仍然有效',
  `revoked_reason` VARCHAR(32)     NOT NULL DEFAULT ''                 COMMENT 'logout 退出 / rotated 轮换 / password_changed 改密 / admin 管理员操作',
  `created_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_sessions_session_id` (`session_id`),
  -- 查"某用户当前有效会话"走这个索引
  KEY `idx_user_sessions_user_revoked_expires` (`user_id`, `revoked_at`, `expires_at`),
  -- 供 cron 清理过期会话
  KEY `idx_user_sessions_expires_at` (`expires_at`),
  CONSTRAINT `fk_user_sessions_user_id`
    FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='用户会话与刷新令牌';
