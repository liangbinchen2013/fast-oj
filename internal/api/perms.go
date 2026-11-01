package api

import (
	"net/http"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// Capability names. They are stored in the database, so changing one is a data
// migration, not a code edit that leaves stale rows behind.
const (
	permProblemCreate     = "problem.create"
	permProblemEditAny    = "problem.edit.any"
	permProblemPublish    = "problem.publish"
	permTestcaseManage    = "testcase.manage"
	permContestCreate     = "contest.create"
	permContestManageAny  = "contest.manage.any"
	permUserManage        = "user.manage"
	permPermissionManage  = "permission.manage"
	permJudgeView         = "judge.view"
	permFeedModerate      = "feed.moderate"
	permSiteSettings      = "site.settings"
	permSubmissionViewAny = "submission.view.any"
	permTopicModerate     = "topic.moderate"
	permTicketHandle      = "ticket.handle"
	// The three revocable everyday capabilities. Everyone holds them by default;
	// an admin takes one away from one account in the permission console. They are
	// not staff rights, so granting them to someone does not make them staff.
	permFeedPost    = "feed.post"
	permTopicPost   = "topic.post"
	permContestJoin = "contest.join"
	// permStatsView 是统计页的键：能改首页文案的人不必看到运营数据。
	permStatsView = "stats.view"
	// permIPManage 同时管两件事：封人与看见谁在打。不能封人的权限不该拿到访客地址。
	permIPManage = "ip.manage"
	// permJudgeProgram 单独管"给题目写判题器/交互器"。它不该跟着 testcase.manage 或
	// problem.edit 走：那两项给的是改题面和改数据的权力，而这一项等于把"在判机上执行
	// 一段别人写的代码"的权力发出去。
	permJudgeProgram = "judge_program.edit"
	// permRejudge 单独管"把别人的提交退回队列重判"。它不该跟着 submission.view.any 走：
	// 那一项给的是"看得到什么"，而这一项改的是"结果是什么"——重判会清掉逐用例行、覆盖
	// 已有判定，被重判的人的得分、榜单与积分都跟着动。
	permRejudge = "submission.rejudge"
)

// bannedFrom is the refusal for those three: the account is still active and can
// still do everything else, so the message says which door closed and where to ask
// for it back, rather than the generic missing-capability text.
func (a actor) bannedFrom(key, what string) (string, bool) {
	if a.can(key) {
		return "", false
	}
	return "你被禁止" + what + "。想恢复请提交一张工单说明情况。", true
}

func (a actor) can(key string) bool { return a.perms[key] }

// canEditProblem is ownership-scoped authoring: whoever created a problem keeps
// editing it, and problem.edit.any is only needed to touch someone else's.
func (a actor) canEditProblem(p store.Problem) bool {
	if a.can(permProblemEditAny) {
		return true
	}
	return p.OwnerID != 0 && p.OwnerID == a.User.ID
}

// canSeeTestcases covers the non-public view of a problem's test set.
func (a actor) canSeeTestcases(p store.Problem) bool {
	return a.can(permTestcaseManage) || a.canEditProblem(p)
}

// canManageContest is the same ownership rule as authoring, one level up: whoever
// created a contest keeps editing it, and contest.manage.any is only needed to touch
// somebody else's.
func (a actor) canManageContest(c store.Contest) bool {
	if a.can(permContestManageAny) {
		return true
	}
	return c.OwnerID != 0 && c.OwnerID == a.User.ID
}

// canHostContests is the weaker question the list page asks: may this reader see the
// contests that are hidden from the public list.
func (a actor) canHostContests() bool {
	return a.can(permContestCreate) || a.can(permContestManageAny)
}

// requirePerm authenticates and enforces a single capability. The message names
// the key rather than a label so it can never drift from what the database holds.
func (s *Server) requirePerm(w http.ResponseWriter, r *http.Request, key string) (actor, bool) {
	a, err := s.authenticate(r)
	if err != nil {
		s.writeAuthError(w, r, err)
		return actor{}, false
	}
	if !a.can(key) {
		s.respondError(w, r, http.StatusForbidden, "缺少权限："+key)
		return a, false
	}
	return a, true
}

// requireAnyPerm 是"拿着这几把钥匙里的任意一把就行"那一种闸门：身份确认这一页同时服务于
// permission.manage（逐项权限）与 user.manage（角色、状态、注销），两个都得进得去。
// 被拒时只报第一把键：在页面上交代"你其实握着另一把"没有意义，那是在替人探权限表。
func (s *Server) requireAnyPerm(w http.ResponseWriter, r *http.Request, keys ...string) (actor, bool) {
	a, err := s.authenticate(r)
	if err != nil {
		s.writeAuthError(w, r, err)
		return actor{}, false
	}
	for _, key := range keys {
		if a.can(key) {
			return a, true
		}
	}
	s.respondError(w, r, http.StatusForbidden, "缺少权限："+keys[0])
	return a, false
}

// requireProblemEdit loads a problem and checks authoring rights. Owning it is
// enough; problem.edit.any is only needed to touch someone else's.
func (s *Server) requireProblemEdit(w http.ResponseWriter, r *http.Request, key string) (actor, store.Problem, bool) {
	a, err := s.authenticate(r)
	if err != nil {
		s.writeAuthError(w, r, err)
		return actor{}, store.Problem{}, false
	}
	p, err := s.db.FindProblem(r.Context(), key)
	if err != nil {
		s.writeStoreError(w, r, err)
		return actor{}, store.Problem{}, false
	}
	if !a.canEditProblem(p) {
		s.respondError(w, r, http.StatusForbidden,
			"这道题由他人创建，需要 "+permProblemEditAny+" 才能修改")
		return a, p, false
	}
	return a, p, true
}

// requireCaseAccess admits the problem's author and, separately, a test-data
// maintainer — so someone can be allowed to upload cases to problems they did
// not write without being able to rewrite the statement.
// requireJudgeProgram 是用例页上"判题器"那一块的门：既要管得了这道题的用例，也要单独
// 有发判题器的权力。两道门而不是一个，是因为委托别人改题面很常见，而委托别人在判机上
// 跑代码不该跟着一起发生。
func (s *Server) requireJudgeProgram(w http.ResponseWriter, r *http.Request, key string) (actor, store.Problem, bool) {
	a, p, ok := s.requireCaseAccess(w, r, key)
	if !ok {
		return a, p, false
	}
	if !a.can(permJudgeProgram) {
		s.respondError(w, r, http.StatusForbidden,
			"判题器与交互器是「在判机上执行作者写的代码」，需要 "+permJudgeProgram+" 才能改。"+
				"要配 SPJ 请让站主或管理员把这一项发给你。")
		return a, p, false
	}
	return a, p, true
}

func (s *Server) requireCaseAccess(w http.ResponseWriter, r *http.Request, key string) (actor, store.Problem, bool) {
	a, err := s.authenticate(r)
	if err != nil {
		s.writeAuthError(w, r, err)
		return actor{}, store.Problem{}, false
	}
	p, err := s.db.FindProblem(r.Context(), key)
	if err != nil {
		s.writeStoreError(w, r, err)
		return actor{}, store.Problem{}, false
	}
	if !a.canSeeTestcases(p) {
		s.respondError(w, r, http.StatusForbidden,
			"需要 "+permTestcaseManage+" 或该题的作者权限")
		return a, p, false
	}
	return a, p, true
}
