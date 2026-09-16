-- 000005_init_recharge 回滚
--
-- 先删渠道流水再删充值订单：payment_transactions 对 recharge_orders 有外键，
-- 反过来的顺序会失败。

DROP TABLE IF EXISTS `payment_transactions`;
DROP TABLE IF EXISTS `recharge_orders`;
