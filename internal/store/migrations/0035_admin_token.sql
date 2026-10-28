-- 0035: 运维令牌（Admin Token）改成一次性的，并让服务端自己再生成。
--
-- 改之前它是启动参数里的一个固定串，永久有效、可以无限次使用：拿着它的人能在
-- /admin/claim 随时把 admin 账号的密码改掉（改完就是超级管理员），也能走 bearer
-- 那条路以运维身份建题、传用例、读判题数据。谁可能拿到它？备份里的 env 文件、能读
-- /proc/<pid>/environ 或 ps 的本地账号、日志、以及任何一次误贴。也就是说，泄露一次
-- 就等于永久交出这个站，而且事后看不出来有没有人用过。
--
-- 这张表把它变成"一个串 = 一次特权动作"：
--
--   hash        令牌的 sha256（十六进制）。库里**绝不存明文**：这张表会被备份、被
--               .dump、被人随手读到，比 0600 的 env 文件暴露面大得多；而验证只需要
--               "拿对方给的串算一次哈希再比"。
--   created_at  生成时刻
--   used_at     用掉的时刻；0 表示这枚还没被用过，也就是当前有效的那一枚
--   used_by     用它干了什么（claim / bearer POST /problems …），供事后追查
--
-- "用掉之后服务端当场生成下一枚"是这条记录存在的理由：表里始终留着一行 used_at=0，
-- 下一枚的明文只在轮换那一行日志里出现一次。启动参数那一个只在全表为空（全新的库）
-- 时才武装进来——否则重启就会把用掉的串复活，那正是这次要堵的洞。
CREATE TABLE admin_tokens (
    id         INTEGER PRIMARY KEY,
    hash       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    used_at    INTEGER NOT NULL DEFAULT 0,
    used_by    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_admin_tokens_armed ON admin_tokens(used_at, created_at);
