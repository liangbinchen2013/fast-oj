-- 0036: 重新判题的入口权限。
--
-- 判题口径是会变的（这一条就是活例：全局数组把镜像撑过上限的崩溃，2026-11-01 起判 MLE
-- 而不是 RE），而**旧提交不会因为判机升级就自己改判**——库里的判定是当时那次运行的结果。
-- 所以管理员要能把任意一条提交退回队列重跑一次。
--
-- 这一把钥匙单独设，不跟着 submission.view.any 走：那一项给的是"看得到什么"，而重判改的是
-- "结果是什么"——它会清掉逐用例行、覆盖已有判定，被重判的人的得分、榜单与积分都跟着动。
-- 能看别人源码的人不该顺手改掉别人的成绩。

INSERT INTO permissions (key, label, staff) VALUES ('submission.rejudge', '重新判题（任意提交）', 1);
INSERT INTO role_permissions (role, key) VALUES ('admin', 'submission.rejudge');
