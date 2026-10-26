-- 0033: 积分。四个维度的活跃度合成一个数，写回 users 这一行。
--
-- 为什么是一列算好的数，而不是每次渲染现算：这个数要从 submissions、posts、topics、
-- topic_comments 里聚合出来，挂在每个请求上算等于把排行榜的代价付在每一页。所以后台按
-- 管理员设的间隔重算一次（internal/api/points.go），写进这三列，读的人只是读一个整数。
--
--   points        当前生效的分值（已经乘过衰减）
--   points_parts  四个维度的明细（未衰减），形如 ac=120,sub=40,cts=30,com=20
--   points_at     上一次重算的时刻（毫秒）
--
-- 为什么不建一张 user_points 表：它和 users 是严格一对一，而且每一处显示名字的地方本来就
-- 在读 users。多一张表就是多一次 JOIN，换来的只是把同一行数据放在两个地方、从此要担心它们
-- 不一致。
--
-- 衰减为什么也算在重算里而不是读的时候用指数函数现算：SQLite 没有可靠的 pow，而读的时候
-- 现算意味着每个视图都要知道"上次活跃是什么时候"——那又是一次聚合。乘在重算那一步，代价
-- 每 N 分钟付一次，读的时候是零。
--
-- 新账号、以及从没活动过的人是 0 分，不是 NULL：0 就是"看不出来"，颜色那一路（0034）
-- 会把他们归到最低一档。
ALTER TABLE users ADD COLUMN points INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN points_parts TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN points_at INTEGER NOT NULL DEFAULT 0;

-- 重算积分要按作者数评论。评论一直只有"按帖子查"这一条索引：以前没人按人算过。
CREATE INDEX idx_topic_comments_author ON topic_comments(author_id, id);
