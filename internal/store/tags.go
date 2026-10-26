package store

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// tags.go 是"管理员给人贴的话"：最多三个短标签，显示在名字后面，形如 admin [作弊者]。
//
// 为什么是一列文本、并且折进 personSQL：这个名字要出现在提交列表、排行榜、动态、帖子、评论、
// 工单和关注列表里，而 personSQL 已经是一个人"该被叫作什么"的唯一出口（注销后叫封禁用户）。
// 标签折进同一条表达式，所有视图就一起有了，一个查询都不多。代价是"按标签反查人"要用 LIKE，
// 今天没这个需求，需要时这一列就是现成的迁移素材。
//
// 校验在写入侧（SetUserTags 拒绝不合格的，不静默丢）。SQL 那一路把列原文拼进名字，靠
// html/template 的转义兜住——它进不了标签、属性或脚本，最坏是名字后面多出一串怪字符；
// 需要拆开逐个用时，走 ParseTags，它会把不合法的丢掉。

const (
	// MaxTags 一个人最多几个标签：三个已经让名字后面很挤了，再多就没人读。
	MaxTags = 3
	// MaxTagLen 单个标签最长 12 个字符（按 rune 算，中文一个字算一个）。
	MaxTagLen = 12
	// MaxTagsText 整串的长度上限，给"库里被手改过"那种情况兜底。
	MaxTagsText = 48
)

var (
	ErrTooManyTags = errors.New("标签最多三个")
	ErrBadTag      = errors.New("标签里有不能用的字（逗号、方括号和控制字符不让用，单个最长 12 字）")
)

// SplitTags 按分隔符切原文：英文逗号与中文逗号都认，只 trim 不判断。
func SplitTags(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '，' })
	out := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseTags 读那一列文本，只留下合法的标签（丢弃超长、空、带分隔符的）。
func ParseTags(raw string) []string {
	if len(raw) > MaxTagsText {
		raw = raw[:MaxTagsText]
	}
	var out []string
	for _, p := range SplitTags(raw) {
		if !validTag(p) || len(out) >= MaxTags {
			continue
		}
		out = append(out, p)
	}
	return out
}

// validTag 只放行"能安全出现在 [ ] 之间"的字：不能带分隔符、不能有控制字符、长度受限。
//
// 没有再往下限制字符集：标签是人类读的词（"作弊者"、"新生"、"退役选手"），得能写中文。
func validTag(tag string) bool {
	if tag == "" || utf8.RuneCountInString(tag) > MaxTagLen || len(tag) > MaxTagsText {
		return false
	}
	return !strings.ContainsFunc(tag, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == '[' || r == ']' || r == ',' || r == '，'
	})
}

// TagsTextOf 把表单原文归一化成库里那种形状（英文逗号分隔、trim、中文逗号换过来）。
//
// 存在的理由是"写进去的"和"记进日志与审计的"必须是同一个串：SetUserTags 用它写库，
// 控制台用它记审计，于是管理员事后查到的就是页面上真正显示的那个样子。
// 它不判断合不合法（那会静默丢字），只做形状转换；要判断用 ParseTags。
func TagsTextOf(raw string) string {
	return strings.Join(SplitTags(raw), ",")
}

// SetUserTags 存下管理员给这个人写的标签。传进来的是表单原文（逗号/中文逗号都认）。
//
// 不合格就报错，不清空也不截断：标签是写给别人看的，"保存成功但其实被丢了"是最坏的结局——
// 管理员会以为自己贴上了。
func (db *DB) SetUserTags(ctx context.Context, userID int64, raw string) error {
	if err := db.guardMutable(ctx, userID); err != nil {
		return err
	}
	parts := SplitTags(raw)
	if len(parts) > MaxTags {
		return ErrTooManyTags
	}
	for _, p := range parts {
		if !validTag(p) {
			return ErrBadTag
		}
	}
	_, err := db.SQL.ExecContext(ctx,
		`UPDATE users SET tags=? WHERE id=?`, strings.Join(parts, ","), userID)
	return err
}
