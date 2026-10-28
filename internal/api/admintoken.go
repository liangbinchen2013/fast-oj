package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// admintoken.go 把"运维令牌"接进请求路径：对答案 = 用掉一次，并当场生成下一枚。
//
// 这一层刻意只管三件事：核对、烧掉、把下一枚交到日志里一次。为什么要这么麻烦——
// 那串东西能随时在 /admin/claim 改掉 admin 的密码（改完就是超级管理员），也能以运维
// 身份走 bearer 建题、传用例、读判题数据。让它永久有效就等于：任何一次泄露（备份里的
// env、读得到 /proc 或 ps 的本地账号、误贴到聊天里的一次）都是永久交出整个站点，
// 而且事后无从知道有没有人动过。改成一次性之后，泄露最多换来一次动作，每次动作都有一行日志。
//
// 一次请求里"这是谁"可能被问好几遍（CSRF 闸门、限流、处理器各问一次），而一次性令牌只
// 值一次动作。所以核对与核销分开放：核对（authenticate 走的）是只读的，核销统一在响应
// 写完之后做一次（settleAdminToken），否则第二遍核对会把一个正常请求变成"令牌无效"。

// adminTokenWindow/adminTokenAttempts 沿用接管页那把闸门的形状：同一地址 15 分钟内
// 只给 claimAttempts 次机会。令牌本身是 192 位随机串，猜不出来；这把闸门防的是"运维自己
// 写了一个短值"那种情况。
const adminTokenPrefix = "admintoken:"

// adminTokenValid 只回答"这是不是当前那一枚可用的运维令牌"，不改动任何东西。
func (s *Server) adminTokenValid(ctx context.Context, presented string) bool {
	ok, err := s.db.AdminTokenMatches(ctx, presented)
	if err != nil {
		s.log.Error("operator token lookup failed", "err", err)
		return false
	}
	return ok
}

// settleAdminToken 在响应写完之后结算 bearer 这一路的运维令牌。
//
// 用错了：记一次失败（同一请求只记这一条，因为这里是唯一改状态的地方），回应形状和
// "这一串根本不是令牌"完全一样，不告诉对方已经摸到一个有效值。
// 用对了且这一笔真的做成（status < 400）：烧掉它、生成下一枚，并把新值只打印在这一行
// 日志里——那是它唯一的投递通道（journald 只有 root 与 adm 组读得到，和 0600 的 env 文件
// 同一档），run.sh 早就在这么把一个随机生成的令牌交给运维。
// 用对了但这一笔没做成（4xx）：**不烧**。管理员打错一个字段就该重新抄一串令牌，
// 那只会教会他绕开这套东西；而拿着真令牌的人本来就能发出成功的请求，多给这一条不漏权限。
func (s *Server) settleAdminToken(r *http.Request, status int) {
	token := bearerToken(r)
	if token == "" || (s.cfg.JudgeToken != "" && token == s.cfg.JudgeToken) {
		// 判机令牌是一台远端机器要预先知道的长期共享串，不参与这套轮换。
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	key := adminTokenPrefix + clientIP(r)
	ok, err := s.db.AdminTokenMatches(ctx, token)
	if err != nil {
		s.log.Error("operator token check failed", "err", err)
		return
	}
	if !ok {
		if _, err := s.db.RegisterLoginFailure(ctx, key, claimWindow); err != nil {
			s.log.Error("throttle", "err", err)
		}
		s.log.Warn("operator token refused", "ip", clientIP(r), "path", r.URL.Path)
		s.audit(r, actor{}, "admin.token.denied", r.Method+" "+r.URL.Path, "运维令牌不正确或已经用过（来自 "+clientIP(r)+"）")
		return
	}
	if status >= 400 {
		return
	}
	next, spent, err := s.db.SpendAdminToken(ctx, token, r.Method+" "+r.URL.Path)
	if err != nil || !spent {
		// 两个请求同时用同一枚时会有一个先烧掉：另一个的动作已经完成了，只能把这件事
		// 如实记下来——这一枚在此刻之后不再有效。
		s.log.Warn("operator token spent late", "err", err)
		return
	}
	if err := s.db.ClearLoginFailures(ctx, key); err != nil {
		s.log.Error("throttle reset", "err", err)
	}
	s.log.Warn("operator token used; one-time token rotated",
		"used_for", r.Method+" "+r.URL.Path, "ip", clientIP(r), "next_token", next,
		"note", "这一枚也用一次就作废；不想留着就把 env 文件里那一行删掉")
	// 成功的 bearer 动作**不写审计行**：这一路的每一次改动本身都已经按动作记过了（授予能力、
	// 封 IP、删题目各有一条），把"某人用令牌读了某页"也灌进这张表只会把真正的线索冲淡。
	// 用掉这件事记在日志里；接管那一路是例外，它记，因为"有人拿令牌接管了这个站"本身就是
	// 一条要留在库里的安全事件。
}

// adminTokenOverBudget 是 bearer 这一路的闸门读数：同一地址 15 分钟内试错的次数已经超了
// 预算，就连真的那一枚也先拒。核对本身是只读的，所以这道问在这里。
func (s *Server) adminTokenOverBudget(r *http.Request) bool {
	allowed, err := s.db.LoginAllowed(r.Context(), adminTokenPrefix+clientIP(r), claimWindow, claimAttempts)
	if err != nil {
		// 读不出闸门时按"没超"处理：这一路的凭据本来就是 192 位随机串，把一个正常的
		// 运维请求挡在临时故障前面，代价比放过一个猜测者大。
		s.log.Error("operator token throttle unreadable", "err", err)
		return false
	}
	if allowed {
		return false
	}
	s.log.Warn("operator token refused: too many failures", "ip", clientIP(r), "path", r.URL.Path)
	s.audit(r, actor{}, "admin.token.throttled", r.Method+" "+r.URL.Path,
		clientIP(r)+" 在 15 分钟内试得太多")
	return true
}

// spendOperatorToken 是接管页那一路的核对 + 核销：它必须**当场**用掉，因为"接管"就是
// 把 admin 的密码写下去这一件事，核对与写入之间不能留窗口。表单里的令牌不是 bearer 头，
// settleAdminToken 那一路管不到它，所以这条是唯一的另一个核销入口。
func (s *Server) spendOperatorToken(r *http.Request, presented string) bool {
	key := adminTokenPrefix + clientIP(r)
	allowed, err := s.db.LoginAllowed(r.Context(), key, claimWindow, claimAttempts)
	if err != nil {
		s.log.Error("throttle", "err", err)
		return false
	}
	if !allowed {
		s.log.Warn("operator token refused: too many failures", "ip", clientIP(r))
		s.audit(r, actor{}, "admin.token.throttled", "claim", clientIP(r)+" 在 15 分钟内试得太多")
		return false
	}
	next, ok, err := s.db.SpendAdminToken(r.Context(), presented, "claim")
	if err != nil {
		s.log.Error("operator token check failed", "err", err)
		return false
	}
	if !ok {
		s.log.Warn("operator token refused", "ip", clientIP(r), "for", "claim")
		s.audit(r, actor{}, "admin.token.denied", "claim", "接管时运维令牌不正确或已经用过（来自 "+clientIP(r)+"）")
		return false
	}
	if err := s.db.ClearLoginFailures(r.Context(), key); err != nil {
		s.log.Error("throttle reset", "err", err)
	}
	s.log.Warn("operator token used; one-time token rotated",
		"used_for", "claim", "ip", clientIP(r), "next_token", next,
		"note", "这一枚也用一次就作废；不想留着就把 env 文件里那一行删掉")
	s.audit(r, actor{}, "admin.token.use", "claim", "接管后台时用掉一枚运维令牌，已另生成下一枚")
	return true
}

// ArmAdminToken 是开机第一步：保证有一枚可用令牌，并如实说明它是哪来的。
//
// 只在"这个库从没引导过"时认启动参数那一个（全新安装照 README 走）；其余情况要么沿用
// 还没用过的那枚，要么现场生成一枚并打印一次。这里绝不打印 fromConfig 的值——运维是自己
// 写的它，日志里再出现一次只是多一个泄露面。
func (s *Server) ArmAdminToken(ctx context.Context) {
	armed, err := s.db.AdminTokenArmed(ctx)
	if err != nil {
		s.log.Error("operator token state unreadable", "err", err)
		return
	}
	token, fromFlag, err := s.db.EnsureAdminToken(ctx, strings.TrimSpace(s.cfg.AdminToken))
	if err != nil {
		s.log.Error("operator token setup failed", "err", err)
		return
	}
	switch {
	case armed:
		s.log.Info("operator token: 库里已有一枚没用过的，沿用（用一次即作废）")
	case fromFlag:
		s.log.Info("operator token: 用的是启动参数/环境变量里那一枚，只能用一次",
			"next", "用完后新令牌会打印在 one-time token rotated 那一行")
	default:
		s.log.Warn("operator token: 服务端生成了一枚一次性令牌，只在这里出现一次", "token", token)
	}
}
