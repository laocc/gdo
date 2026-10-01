package sqlite

import (
	"fmt"
	"strings"
)

// Cond 独立的条件收集器：只拼装 WHERE 片段与参数，不持有连接、不执行语句。
//
// 适用场景：手里已经有一个查询函数（例如日志层自带的 PagedQuery：它接收 countSQL / listSQL 文本），
// 只想复用键后缀语法把条件拼出来，不想换成 Builder。
//
//	cond := sqlite.NewCond()
//	cond.And("userID", userID)                                   // userID = ?
//	cond.And("userName~", keyword)                               // userName LIKE ?
//	cond.AndOr(sqlite.M("title~", kw), sqlite.M("content~", kw))  // ((title LIKE ?) OR (content LIKE ?))
//	cond.AndRaw("(a || b) LIKE ?", "%x%")                        // 原生片段
//	whereSQL, whereArgs, whereErr := cond.Where()                 // " WHERE userID = ? AND userName LIKE ?" 与参数
//
// 条件写错（字段名不合法、值类型不符、用了本库不支持的 * / $ 后缀）时会返回错误，
// 调用方必须处理：绝不能把错误当成「没有条件」，否则查询会退化成全表扫描。
type Cond struct {
	clauses  []string
	args     []any
	parseErr error
}

// NewCond 新建一个空条件收集器
func NewCond() *Cond {
	return &Cond{}
}

// And 追加一组条件（键值对或单个 map），与已有条件用 AND 连接。
// 写法与 Builder.Where 完全一致；解析失败记在收集器上，由 Where 统一返回。
func (cond *Cond) And(args ...any) *Cond {
	if cond.parseErr != nil {
		return cond
	}

	conditions, conditionsErr := whereConditions(args)
	if conditionsErr != nil {
		cond.parseErr = conditionsErr
		return cond
	}
	if len(conditions) == 0 {
		return cond
	}

	clauses, conditionArgs, conditionsBuildErr := whereGroupClauses(conditions)
	if conditionsBuildErr != nil {
		cond.parseErr = conditionsBuildErr
		return cond
	}
	cond.clauses = append(cond.clauses, clauses...)
	cond.args = append(cond.args, conditionArgs...)
	return cond
}

// AndOr 追加一组 OR 条件（组内 AND、组间 OR），与已有条件用 AND 连接。
// 组不能为空；需要更深的嵌套请用 AndRaw 手写。
func (cond *Cond) AndOr(groups ...map[string]any) *Cond {
	if cond.parseErr != nil || len(groups) == 0 {
		return cond
	}

	groupClauses := make([]string, 0, len(groups))
	groupArgs := make([]any, 0, len(groups))
	for _, group := range groups {
		if len(group) == 0 {
			cond.parseErr = fmt.Errorf("Conditions.AndOr 的条件组不能为空")
			return cond
		}
		clauses, oneGroupArgs, groupErr := whereGroupClauses(group)
		if groupErr != nil {
			cond.parseErr = groupErr
			return cond
		}
		groupClauses = append(groupClauses, "("+strings.Join(clauses, " AND ")+")")
		groupArgs = append(groupArgs, oneGroupArgs...)
	}

	cond.clauses = append(cond.clauses, "("+strings.Join(groupClauses, " OR ")+")")
	cond.args = append(cond.args, groupArgs...)
	return cond
}

// AndRaw 追加一段原生 SQL 条件（占位符按顺序取 args），与已有条件用 AND 连接
func (cond *Cond) AndRaw(query string, args ...any) *Cond {
	if cond.parseErr != nil {
		return cond
	}
	cond.clauses = append(cond.clauses, query)
	cond.args = append(cond.args, args...)
	return cond
}

// Where 返回拼好的 WHERE 片段与参数：形如 " WHERE a = ? AND b LIKE ?"；没有条件时返回空串与 nil。
// 条件拼装出错时返回错误（此时不要当成「没有条件」用）。
func (cond *Cond) Where() (string, []any, error) {
	if cond.parseErr != nil {
		return "", nil, cond.parseErr
	}
	if len(cond.clauses) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(cond.clauses, " AND "), cond.args, nil
}
