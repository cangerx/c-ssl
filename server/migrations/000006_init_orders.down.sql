-- 000006_init_orders 回滚
--
-- 顺序：先删全部子表，再删 certificate_orders。
-- 五个子表都对 certificate_orders(order_no) 有外键，反过来会失败。
-- webhook_events 没有外键，位置随意，放在最后以保持「无依赖者靠后」的可读性。

DROP TABLE IF EXISTS `certificates`;
DROP TABLE IF EXISTS `order_orgs`;
DROP TABLE IF EXISTS `order_contacts`;
DROP TABLE IF EXISTS `order_domains`;
DROP TABLE IF EXISTS `certificate_orders`;
DROP TABLE IF EXISTS `webhook_events`;
