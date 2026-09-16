-- MySQL 容器首次初始化时执行。
--
-- 只在数据卷为空时运行一次（Docker 官方镜像的行为）。
-- 本地已有卷想重新初始化：docker compose down -v 后再 up。
--
-- 为什么需要这个脚本：MYSQL_DATABASE 环境变量只能创建一个库，
-- 而本项目需要两个——开发库 c_ssl_dev 与测试库 c_ssl_test。
-- 需要真实数据库的测试（钱包域的并发、幂等、对账）跑在测试库上，
-- 它们会写数据、会绕过应用直接改库构造脏数据，绝不能跑在开发库上。

CREATE DATABASE IF NOT EXISTS `c_ssl_test`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- MYSQL_USER 的授权默认只覆盖 MYSQL_DATABASE，测试库需要显式补上
GRANT ALL PRIVILEGES ON `c_ssl_test`.* TO 'c_ssl'@'%';
FLUSH PRIVILEGES;
