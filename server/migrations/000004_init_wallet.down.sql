-- 000004_init_wallet 回滚
--
-- 先删账本再删账户：wallet_ledger 对 wallet_accounts 有外键。
-- 触发器随表一起被删除，无需单独 DROP TRIGGER。

DROP TABLE IF EXISTS `wallet_ledger`;
DROP TABLE IF EXISTS `wallet_accounts`;
