package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// rawSQLValue 原生 SQL 片段值：标记该值应直接拼入 SQL 而不走参数化。
type rawSQLValue struct {
	sqlText string
}

// Raw 构造一个原生 SQL 片段值，供 SET / INSERT 的值位置写表达式（如自增、函数调用）。
// 用法: sqlite.Table("tabCount").Update(sqlite.M("countNum", sqlite.Raw("countNum + 1")), sqlite.M("countID", id))
// 注意: 片段不做参数化转义，调用方必须保证内容安全（不得把用户输入直接拼进来）。
func Raw(sqlText string) any {
	return rawSQLValue{sqlText: sqlText}
}

// PagingData 分页查询结果（JSON 字段名与前端约定的 recode/total/size/current 保持一致）。
type PagingData struct {
	Recode  uint64 `json:"recode"`  // 总记录数
	Total   uint64 `json:"total"`   // 总页数
	Size    uint64 `json:"size"`    // 每页记录数
	Current uint64 `json:"current"` // 当前页号
}

// Builder 语句构建器，链式调用。
// trx 非空时（由 DbTrans.Table 创建）所有语句都跑在该事务上；SQLite 事务里读写都允许。
type Builder struct {
	db            *sql.DB
	trx           *sql.Tx
	table         string
	distinct      bool
	fields        []string
	skipFields    []string
	where         []string
	whereArgs     []any
	whereParseErr error // Where / WhereOr 解析键后缀失败时的错误，在真正执行语句前统一返回
	orderBy       string
	limit         int64
	offset        int64
	groupBy       string
	having        string
	havingArgs    []any
	decodeFields  []string
}

// exec 执行写语句（Insert / InsertBatch / Upsert / Update / Delete 的唯一出口）：
// 有事务走事务，否则走默认连接。事务里的语句一出错就地回滚整笔事务，
// 所以调用方按错误返回即可，不需要再手动 Rollback（事务已结束，后续语句只会拿到 sql.ErrTxDone）。
func (builder *Builder) exec(query string, args ...any) (sql.Result, error) {
	if builder.trx != nil {
		result, execErr := builder.trx.Exec(query, args...)
		if execErr != nil {
			return result, builder.rollbackOnErr(execErr)
		}
		return result, nil
	}
	if builder.db == nil {
		return nil, errNoDB
	}
	return builder.db.Exec(query, args...)
}

// query 执行查询语句：有事务走事务，否则走默认连接
func (builder *Builder) query(query string, args ...any) (*sql.Rows, error) {
	if builder.trx != nil {
		return builder.trx.Query(query, args...)
	}
	if builder.db == nil {
		return nil, errNoDB
	}
	return builder.db.Query(query, args...)
}

// queryRow 查询单行：有事务走事务，否则走默认连接
func (builder *Builder) queryRow(query string, args ...any) (*sql.Row, error) {
	if builder.trx != nil {
		return builder.trx.QueryRow(query, args...), nil
	}
	if builder.db == nil {
		return nil, errNoDB
	}
	return builder.db.QueryRow(query, args...), nil
}

// Select 指定查询字段，默认 *
func (builder *Builder) Select(fields ...string) *Builder {
	builder.fields = fields
	return builder
}

// Distinct 消除查询结果中的重复行（SELECT DISTINCT）。
// 与 Select / SelectSkip 连用，结果按所选列去重。
// 用法: sqlite.Table("tabVoucher").Distinct().Select("voState").Pluck("voState")
func (builder *Builder) Distinct() *Builder {
	builder.distinct = true
	return builder
}

// SelectSkip 查询除指定字段外的所有列（列名按 PRAGMA table_info 动态获取，不缓存）。
// 与 Select 互斥：调用 Select 后 SelectSkip 不生效。
// 用法: sqlite.Table("tabPrice").SelectSkip("priceValue").Paging(page, pageSize)
func (builder *Builder) SelectSkip(fields ...string) *Builder {
	builder.skipFields = fields
	return builder
}

// WhereArg 添加原生 SQL 片段条件（可多次调用，用 AND 连接），片段里的占位符按顺序取 args。
// 除函数、复杂 OR 组合外，优先用 Where 的键后缀写法，少写字符串更不容易写错条件。
func (builder *Builder) WhereArg(query string, args ...any) *Builder {
	builder.where = append(builder.where, query)
	builder.whereArgs = append(builder.whereArgs, args...)
	return builder
}

// Where 添加 WHERE 条件（多个条件用 AND 连接）。
// 键可带操作符后缀（见 whereOperators），不带后缀时默认使用 = 运算符：
//
//	Where("userState", 1)             生成 userState = ?
//	Where("userName~", "张")          生成 userName LIKE ?（值自动补 %）
//	Where("userName,userNick~", "张") 生成 ((userName || userNick) LIKE ?)
//	Where("userFlag&?", 8)            生成 (userFlag = 0 OR (userFlag & ?) > 0)
//	Where("userID@", []any{1, 2})     生成 userID IN (?, ?)
//	Where("loginTime#", []any{s, e})  生成 loginTime BETWEEN ? AND ?
//
// 列名可用逗号或加号分隔多个（两者等价）：按 || 拼接后比对，只对 like 后缀 ~ / !~ 有意义。
//
// 条件写错（字段名不合法、值类型不符、用了 SQLite 不支持的 * / $ 后缀）时把错误记在构建器上，
// 由 All / Update / Delete 等在执行前统一返回，不会静默拼出一条错的 SQL。
//
// 入参支持两种写法：Where("userID", 1, "userState", 1) 与 Where(sqlite.M("userID", 1)) 等价。
func (builder *Builder) Where(args ...any) *Builder {
	conditions, conditionsErr := whereConditions(args)
	if conditionsErr != nil {
		builder.whereParseErr = conditionsErr
		return builder
	}
	if len(conditions) == 0 {
		return builder
	}

	clauses, conditionArgs, conditionsBuildErr := whereGroupClauses(conditions)
	if conditionsBuildErr != nil {
		builder.whereParseErr = conditionsBuildErr
		return builder
	}
	builder.where = append(builder.where, clauses...)
	builder.whereArgs = append(builder.whereArgs, conditionArgs...)
	return builder
}

// WhereOr 把多组条件用 OR 连接，组内仍按 AND 连接，等价于 SQL 里的 ((a AND b) OR (c AND d))。
// 每组用 sqlite.M 构造；组不能为空（空组会拼出 () 这种非法 SQL，直接报错）。
//
//	builder.WhereOr(sqlite.M("key1", 3, "key2", 23), sqlite.M("key1", 5, "key3", 23))
//
// 需要更深的嵌套（组里再套组）时用 WhereArg 手写原生片段。
func (builder *Builder) WhereOr(groups ...map[string]any) *Builder {
	if len(groups) == 0 {
		return builder
	}

	groupClauses := make([]string, 0, len(groups))
	for _, group := range groups {
		if len(group) == 0 {
			builder.whereParseErr = fmt.Errorf("WhereOr 的条件组不能为空")
			return builder
		}
		clauses, groupArgs, groupErr := whereGroupClauses(group)
		if groupErr != nil {
			builder.whereParseErr = groupErr
			return builder
		}
		groupClauses = append(groupClauses, "("+strings.Join(clauses, " AND ")+")")
		builder.whereArgs = append(builder.whereArgs, groupArgs...)
	}

	builder.where = append(builder.where, "("+strings.Join(groupClauses, " OR ")+")")
	return builder
}

// whereGroupClauses 把一组键值对条件转成「子句列表 + 参数」，组内按 AND 连接，参数顺序与占位符一致。
func whereGroupClauses(group map[string]any) ([]string, []any, error) {
	clauses := make([]string, 0, len(group))
	groupArgs := make([]any, 0, len(group))
	for rawKey, rawValue := range group {
		clause, conditionArgs, conditionErr := buildWhereCondition(rawKey, rawValue)
		if conditionErr != nil {
			return nil, nil, conditionErr
		}
		clauses = append(clauses, clause)
		groupArgs = append(groupArgs, conditionArgs...)
	}
	return clauses, groupArgs, nil
}

// whereConditions 把条件入参归一成条件 map：
//   - 不传参数：没有条件；
//   - 只传一个 map[string]any（sqlite.M 的返回值）：直接用它；
//   - 其余情况按「键、值」成对解析。
//
// 参数不合法时返回错误而绝不静默当「没有条件」：条件一旦丢掉，查询会捞全表，
// 更新 / 删除会落到整张表上。
func whereConditions(args []any) (map[string]any, error) {
	switch len(args) {
	case 0:
		return nil, nil
	case 1:
		if args[0] == nil {
			// 显式传 nil 等同没有条件
			return nil, nil
		}
		if conditions, isMap := args[0].(map[string]any); isMap {
			return conditions, nil
		}
	}

	if len(args)%2 != 0 {
		return nil, fmt.Errorf("Where 参数必须成对出现（键、值），实际 %d 个；原生 SQL 片段请用 WhereArg", len(args))
	}

	conditions := make(map[string]any, len(args)/2)
	for index := 0; index < len(args); index += 2 {
		field, isString := args[index].(string)
		if !isString {
			return nil, fmt.Errorf("Where 的键必须是字符串，实际 %T", args[index])
		}
		conditions[field] = args[index+1]
	}
	return conditions, nil
}

// WhereIn 添加 WHERE field IN (values...) 条件
// 注意：SQLite 单条语句的变量上限默认 999（3.32+ 为 32766），批次很大时请分段查询。
func (builder *Builder) WhereIn(field string, values []any) *Builder {
	placeholders := make([]string, len(values))
	for index := range values {
		placeholders[index] = "?"
	}
	condition := fmt.Sprintf("%s IN (%s)", field, strings.Join(placeholders, ","))
	builder.where = append(builder.where, condition)
	builder.whereArgs = append(builder.whereArgs, values...)
	return builder
}

// WhereBetween 添加 WHERE field BETWEEN ? AND ? 条件
func (builder *Builder) WhereBetween(field string, start, end any) *Builder {
	condition := fmt.Sprintf("%s BETWEEN ? AND ?", field)
	builder.where = append(builder.where, condition)
	builder.whereArgs = append(builder.whereArgs, start, end)
	return builder
}

// OrderBy 排序，如 "userID DESC"、"createTime ASC, userID DESC"
func (builder *Builder) OrderBy(order string) *Builder {
	builder.orderBy = order
	return builder
}

// Limit 限制返回行数
func (builder *Builder) Limit(limit int64) *Builder {
	builder.limit = limit
	return builder
}

// Offset 偏移量
func (builder *Builder) Offset(offset int64) *Builder {
	builder.offset = offset
	return builder
}

// GroupBy 分组，如 "userID"
func (builder *Builder) GroupBy(groupBy string) *Builder {
	builder.groupBy = groupBy
	return builder
}

// Having 添加 HAVING 条件（仅在 GroupBy 之后生效）
func (builder *Builder) Having(query string, args ...any) *Builder {
	builder.having = query
	builder.havingArgs = args
	return builder
}

// Decode 指定查询结果中需要解码的字段名（可多个）：
// 字符串与 []byte 列按 JSON 反序列化成对象/数组；整型列按位拆分成 [1,2,4] 形式（配合位标记字段）。
func (builder *Builder) Decode(fieldNames ...string) *Builder {
	builder.decodeFields = fieldNames
	return builder
}

// buildQuery 构建完整的查询语句与参数
func (builder *Builder) buildQuery() (string, []any, error) {
	// WHERE 键后缀解析失败时不继续拼 SQL，把错误原样交给调用方
	if builder.whereParseErr != nil {
		return "", nil, builder.whereParseErr
	}

	fieldStr := "*"
	if len(builder.fields) > 0 {
		fieldStr = strings.Join(builder.fields, ", ")
	} else if len(builder.skipFields) > 0 {
		allColumns, columnErr := builder.tableColumns()
		if columnErr != nil {
			return "", nil, columnErr
		}
		skipSet := make(map[string]bool, len(builder.skipFields))
		for _, skipField := range builder.skipFields {
			skipSet[skipField] = true
		}
		selectedColumns := make([]string, 0, len(allColumns))
		for _, column := range allColumns {
			if !skipSet[column] {
				selectedColumns = append(selectedColumns, column)
			}
		}
		fieldStr = strings.Join(selectedColumns, ", ")
	}

	// DISTINCT 消除重复行
	if builder.distinct {
		fieldStr = "DISTINCT " + fieldStr
	}

	query := fmt.Sprintf("SELECT %s FROM %s", fieldStr, builder.table)

	if len(builder.where) > 0 {
		query += " WHERE " + strings.Join(builder.where, " AND ")
	}

	if builder.groupBy != "" {
		query += " GROUP BY " + builder.groupBy
		if builder.having != "" {
			query += " HAVING " + builder.having
		}
	}

	if builder.orderBy != "" {
		query += " ORDER BY " + builder.orderBy
	}

	if builder.limit > 0 {
		query += " LIMIT ?"
	}
	if builder.offset > 0 {
		query += " OFFSET ?"
	}

	args := make([]any, 0, len(builder.whereArgs)+len(builder.havingArgs)+2)
	args = append(args, builder.whereArgs...)
	if len(builder.havingArgs) > 0 {
		args = append(args, builder.havingArgs...)
	}
	if builder.limit > 0 {
		args = append(args, builder.limit)
	}
	if builder.offset > 0 {
		args = append(args, builder.offset)
	}

	return query, args, nil
}

// tableColumns 取表的列名（按表定义顺序），供 SelectSkip 使用
func (builder *Builder) tableColumns() ([]string, error) {
	rows, queryErr := builder.query("SELECT name FROM pragma_table_info(?)", builder.table)
	if queryErr != nil {
		return nil, fmt.Errorf("读取表结构失败 [%s]: %w", builder.table, queryErr)
	}
	defer rows.Close()

	columns := make([]string, 0, 16)
	for rows.Next() {
		var columnName string
		if scanErr := rows.Scan(&columnName); scanErr != nil {
			return nil, scanErr
		}
		columns = append(columns, columnName)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("表 %s 不存在或没有列", builder.table)
	}
	return columns, nil
}

// All 执行查询，返回所有结果行（无结果时返回空切片，不是 nil）。
// conditions 可省略，写法与 Where 一致：
//
//	rows, queryErr := sqlite.Table("tabUser").All()
//	rows, queryErr := sqlite.Table("tabUser").All("userState", 1)
//	rows, queryErr := sqlite.Table("tabUser").All(sqlite.M("userState", 1))
func (builder *Builder) All(conditions ...any) ([]map[string]any, error) {
	builder.Where(conditions...)

	query, queryArgs, buildErr := builder.buildQuery()
	if buildErr != nil {
		return nil, buildErr
	}

	rows, queryErr := builder.query(query, queryArgs...)
	if queryErr != nil {
		return nil, fmt.Errorf("查询失败 [%s]: %w", query, queryErr)
	}
	results, buildErr2 := buildResult(rows)
	if buildErr2 != nil {
		return nil, buildErr2
	}
	builder.decodeResults(results)
	return results, nil
}

// Get 返回第一条结果（没有数据时返回 nil, nil）
// conditions 可省略，写法与 Where 一致：Get("userID", id) 或 Get(sqlite.M("userID", id))。
func (builder *Builder) Get(conditions ...any) (map[string]any, error) {
	builder.Limit(1)
	results, queryErr := builder.All(conditions...)
	if queryErr != nil {
		return nil, queryErr
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0], nil
}

// Paging 分页查询，返回列表数据与分页信息，pageSize 默认 20
// 用法: rows, paging, pageErr := sqlite.Table("tabUser").Where("userState", 1).Paging(1, 20)
func (builder *Builder) Paging(page int64, pageSize ...int64) ([]map[string]any, *PagingData, error) {
	var actualPageSize int64 = 20
	if len(pageSize) > 0 {
		actualPageSize = pageSize[0]
	}
	if page < 1 {
		page = 1
	}
	if actualPageSize < 1 {
		actualPageSize = 20
	}

	// 先查总数（复用同一份 where，参数各用各的）
	if builder.whereParseErr != nil {
		return nil, nil, builder.whereParseErr
	}
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s", builder.table)
	if len(builder.where) > 0 {
		countQuery += " WHERE " + strings.Join(builder.where, " AND ")
	}
	countRow, countRowErr := builder.queryRow(countQuery, builder.whereArgs...)
	if countRowErr != nil {
		return nil, nil, countRowErr
	}
	var total uint64
	if countErr := countRow.Scan(&total); countErr != nil {
		return nil, nil, fmt.Errorf("统计总数失败 [%s]: %w", countQuery, countErr)
	}

	builder.Limit(actualPageSize).Offset((page - 1) * actualPageSize)
	rows, listErr := builder.All()
	if listErr != nil {
		return nil, nil, listErr
	}

	totalPages := (total + uint64(actualPageSize) - 1) / uint64(actualPageSize)
	return rows, &PagingData{
		Recode:  total,
		Total:   totalPages,
		Size:    uint64(actualPageSize),
		Current: uint64(page),
	}, nil
}

// Count 返回 COUNT(*) 结果
func (builder *Builder) Count() (uint64, error) {
	builder.fields = []string{"COUNT(*) AS total"}
	row, queryErr := builder.Get()
	if queryErr != nil {
		return 0, queryErr
	}
	if row == nil {
		return 0, nil
	}
	switch raw := row["total"].(type) {
	case int64:
		return uint64(raw), nil
	case float64:
		return uint64(raw), nil
	case string:
		var parsed uint64
		_, _ = fmt.Sscanf(raw, "%d", &parsed)
		return parsed, nil
	default:
		return 0, fmt.Errorf("无法转换 COUNT 结果类型: %T", raw)
	}
}

// Value 返回单个字段的值
func (builder *Builder) Value(field string) (any, error) {
	builder.fields = []string{field}
	row, queryErr := builder.Get()
	if queryErr != nil {
		return nil, queryErr
	}
	if row == nil {
		return nil, nil
	}
	return row[field], nil
}

// Pluck 返回某一列的所有值
func (builder *Builder) Pluck(field string) ([]any, error) {
	builder.fields = []string{field}
	results, queryErr := builder.All()
	if queryErr != nil {
		return nil, queryErr
	}
	values := make([]any, 0, len(results))
	for _, row := range results {
		values = append(values, row[field])
	}
	return values, nil
}

// Insert 插入一行数据，返回自增主键（表没有自增主键时返回 0）。
// 键可带操作符后缀（见 insertOperators）：
//
//	sqlite.M("createTime\\", "datetime('now')")  表达式原样拼入 SQL，安全由调用方保证
//	sqlite.M("content#", "长文本")                值先做 zlib 压缩再写入（列须为 BLOB）
func (builder *Builder) Insert(data map[string]any) (uint64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("插入数据不能为空")
	}

	columnList, placeholderList, argList, buildErr := builder.buildInsertParts(data)
	if buildErr != nil {
		return 0, buildErr
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		builder.table, strings.Join(columnList, ", "), strings.Join(placeholderList, ", "))

	result, execErr := builder.exec(query, argList...)
	if execErr != nil {
		return 0, fmt.Errorf("插入失败 [%s]: %w", query, execErr)
	}
	lastInsertID, insertErr := result.LastInsertId()
	if insertErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取自增ID失败 [%s]: %w", query, insertErr))
	}
	if lastInsertID < 0 {
		return 0, nil
	}
	return uint64(lastInsertID), nil
}

// InsertBatch 批量插入多行数据，返回影响行数。
// 字段列表取自第一行，各行必须使用同一套键（键可带操作符后缀）。
func (builder *Builder) InsertBatch(rows []map[string]any) (uint64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	firstRow := rows[0]
	fieldList := sortedKeys(firstRow)

	// 列名与值表达式由第一行决定（各行键名需一致），逐行只换参数
	columnList, _, _, buildErr := builder.buildInsertParts(firstRow)
	if buildErr != nil {
		return 0, buildErr
	}

	placeholderList := make([]string, 0, len(rows))
	argList := make([]any, 0, len(rows)*len(fieldList))
	for _, row := range rows {
		rowPlaceholderList := make([]string, 0, len(fieldList))
		for _, rawField := range fieldList {
			_, valueExpr, arg, hasArg, fieldErr := buildInsertField(rawField, row[rawField])
			if fieldErr != nil {
				return 0, fieldErr
			}
			rowPlaceholderList = append(rowPlaceholderList, valueExpr)
			if hasArg {
				argList = append(argList, arg)
			}
		}
		placeholderList = append(placeholderList, "("+strings.Join(rowPlaceholderList, ", ")+")")
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
		builder.table, strings.Join(columnList, ", "), strings.Join(placeholderList, ", "))

	result, execErr := builder.exec(query, argList...)
	if execErr != nil {
		return 0, fmt.Errorf("批量插入失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return uint64(affectedRows), nil
}

// Upsert 插入或更新（SQLite 的 UPSERT），返回影响行数。
// conflictColumns 必须是主键或唯一索引覆盖的列；updateColumns 省略时冲突即忽略（DO NOTHING）。
//
//	// 冲突时把 countNum 累加
//	affected, upsertErr := sqlite.Table("tabCount").Upsert(
//	    sqlite.M("countAppID", 1, "countDay", 20261001, "countNum", 10),
//	    []string{"countAppID", "countDay"},
//	    "countNum",
//	)
//	// => INSERT INTO tabCount (...) VALUES (...) ON CONFLICT (countAppID, countDay) DO UPDATE SET countNum = excluded.countNum
//
// 需要「累加」这类表达式时，updateColumns 里用键后缀：Upsert(data, []string{"a"}, "countNum+")。
// 需要 SQLite 3.24+。
func (builder *Builder) Upsert(data map[string]any, conflictColumns []string, updateColumns ...string) (uint64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("upsert 数据不能为空")
	}
	if len(conflictColumns) == 0 {
		return 0, fmt.Errorf("upsert 必须指定冲突列（主键或唯一索引上的列）")
	}
	for _, conflictColumn := range conflictColumns {
		if !columnNamePattern.MatchString(strings.TrimSpace(conflictColumn)) {
			return 0, fmt.Errorf("upsert 冲突列名不合法: %q", conflictColumn)
		}
	}

	columnList, placeholderList, argList, buildErr := builder.buildInsertParts(data)
	if buildErr != nil {
		return 0, buildErr
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s)",
		builder.table, strings.Join(columnList, ", "), strings.Join(placeholderList, ", "),
		strings.Join(conflictColumns, ", "))

	if len(updateColumns) == 0 {
		query += " DO NOTHING"
	} else {
		setClauses := make([]string, 0, len(updateColumns))
		for _, updateColumn := range updateColumns {
			columnName, operator := splitFieldKey(updateColumn, setOperators)
			if !columnNamePattern.MatchString(columnName) {
				return 0, fmt.Errorf("upsert 更新列名不合法: %q", updateColumn)
			}
			switch operator {
			case "":
				setClauses = append(setClauses, fmt.Sprintf("%s = excluded.%s", columnName, columnName))
			case "+", "-", "*", "/":
				setClauses = append(setClauses, fmt.Sprintf("%s = %s %s excluded.%s", columnName, columnName, operator, columnName))
			default:
				return 0, fmt.Errorf("upsert 更新列只支持算术后缀（+ - * /），实际 %q", updateColumn)
			}
		}
		query += " DO UPDATE SET " + strings.Join(setClauses, ", ")
	}

	result, execErr := builder.exec(query, argList...)
	if execErr != nil {
		return 0, fmt.Errorf("upsert 失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return uint64(affectedRows), nil
}

// buildInsertParts 生成 INSERT 的列名、值表达式与参数（列名按字典序，保证语句稳定）
func (builder *Builder) buildInsertParts(data map[string]any) ([]string, []string, []any, error) {
	fieldList := sortedKeys(data)

	columnList := make([]string, 0, len(fieldList))
	placeholderList := make([]string, 0, len(fieldList))
	argList := make([]any, 0, len(fieldList))

	for _, rawField := range fieldList {
		columnName, valueExpr, arg, hasArg, fieldErr := buildInsertField(rawField, data[rawField])
		if fieldErr != nil {
			return nil, nil, nil, fieldErr
		}
		columnList = append(columnList, columnName)
		placeholderList = append(placeholderList, valueExpr)
		if hasArg {
			argList = append(argList, arg)
		}
	}
	return columnList, placeholderList, argList, nil
}

// Update 更新数据，返回影响行数。
// data 的键可带操作符后缀（见 setOperators），生成字段自运算：
//
//	M("voNumber+", 10)   生成 voNumber = voNumber + ?
//	M("userFlag|", 8)    生成 userFlag = userFlag | ?（置位）
//	M("userFlag^", 8)    生成 userFlag = (userFlag | ?) - (userFlag & ?)（翻转位）
//	M("userFlag!", 8)    生成 userFlag = userFlag - (userFlag & ?)（清位）
//	M("userNotes.", "_x") 生成 userNotes = (userNotes || ?)
//
// 不带后缀时直接赋值；Raw 片段优先于后缀解析。
// conditions 写法与 Where 一致：Update(data, "userID", id) 或 Update(data, sqlite.M("userID", id))；
// 没有条件会直接报错，禁止整表更新。
func (builder *Builder) Update(data map[string]any, conditions ...any) (uint64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("更新数据不能为空")
	}
	builder.Where(conditions...)
	if builder.whereParseErr != nil {
		return 0, builder.whereParseErr
	}
	if len(builder.where) == 0 {
		return 0, fmt.Errorf("禁止不带条件更新（%s）：请用 Where / WhereOr 或 Update 的 conditions 指定条件；确实要整表更新请显式写 WhereArg(\"1 = 1\")", builder.table)
	}

	setClauses, argList, buildErr := buildSetClauses(data)
	if buildErr != nil {
		return 0, buildErr
	}
	argList = append(argList, builder.whereArgs...)

	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		builder.table, strings.Join(setClauses, ", "), strings.Join(builder.where, " AND "))

	result, execErr := builder.exec(query, argList...)
	if execErr != nil {
		return 0, fmt.Errorf("更新失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return uint64(affectedRows), nil
}

// Delete 删除数据，返回影响行数。
// conditions 写法与 Where 一致：Delete("userID", id) 或 Delete(sqlite.M("userID", id))；
// 没有条件会直接报错，禁止整表删除。
func (builder *Builder) Delete(conditions ...any) (int64, error) {
	builder.Where(conditions...)
	if builder.whereParseErr != nil {
		return 0, builder.whereParseErr
	}
	if len(builder.where) == 0 {
		return 0, fmt.Errorf("禁止不带条件删除（%s）：请用 Where / WhereOr 或 Delete 的 conditions 指定条件；确实要清空表请显式写 WhereArg(\"1 = 1\")", builder.table)
	}

	query := fmt.Sprintf("DELETE FROM %s WHERE %s", builder.table, strings.Join(builder.where, " AND "))

	result, execErr := builder.exec(query, builder.whereArgs...)
	if execErr != nil {
		return 0, fmt.Errorf("删除失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return affectedRows, nil
}

// Exec 执行一条完全自定义的原生语句（DDL、复杂 UPDATE 等），返回影响行数。
// 调用方：建表、建索引、批量搬运等构建器覆盖不到的场景；SQL 与参数由调用方保证正确。
// 返回：影响行数与错误。
func (builder *Builder) Exec(query string, args ...any) (int64, error) {
	result, execErr := builder.exec(query, args...)
	if execErr != nil {
		return 0, fmt.Errorf("执行失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return affectedRows, nil
}

// sortedKeys 取 map 的键并排序（保证生成的语句稳定、可比对）
func sortedKeys(data map[string]any) []string {
	keys := make([]string, 0, len(data))
	for field := range data {
		keys = append(keys, field)
	}
	sort.Strings(keys)
	return keys
}

// decodeResults 对查询结果中声明的字段执行解码（JSON 字符串 → 对象、整型 → 位拆分）
func (builder *Builder) decodeResults(results []map[string]any) {
	if len(builder.decodeFields) == 0 {
		return
	}
	fieldSet := make(map[string]bool, len(builder.decodeFields))
	for _, fieldName := range builder.decodeFields {
		fieldSet[fieldName] = true
	}

	for _, row := range results {
		for fieldName := range fieldSet {
			rawValue, exists := row[fieldName]
			if !exists {
				continue
			}
			switch casted := rawValue.(type) {
			case string:
				var decoded any
				if jsonErr := json.Unmarshal([]byte(casted), &decoded); jsonErr == nil {
					row[fieldName] = decoded
				}
			case int64:
				// SQLite 的 INTEGER 列由驱动取回来就是 int64，位标记字段在这里拆
				row[fieldName] = bitSplit(uint64(casted))
			case float64:
				row[fieldName] = bitSplit(uint64(casted))
			case []byte:
				var decoded any
				if jsonErr := json.Unmarshal(casted, &decoded); jsonErr == nil {
					row[fieldName] = decoded
				}
			}
		}
	}
}

// bitSplit 将整数按位拆分，例如 7 → [1, 2, 4]
func bitSplit(value uint64) []uint64 {
	if value == 0 {
		return nil
	}
	bits := make([]uint64, 0, 8)
	var bit uint64 = 1
	for bit <= value {
		if value&bit != 0 {
			bits = append(bits, bit)
		}
		bit <<= 1
	}
	return bits
}

// buildResult 把 *sql.Rows 转成 []map[string]any
func buildResult(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()

	columns, columnErr := rows.Columns()
	if columnErr != nil {
		return nil, fmt.Errorf("获取列名失败: %w", columnErr)
	}

	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if scanErr := rows.Scan(pointers...); scanErr != nil {
			return nil, fmt.Errorf("扫描行数据失败: %w", scanErr)
		}

		row := make(map[string]any, len(columns))
		for index, column := range columns {
			value := values[index]
			if byteValue, isBytes := value.([]byte); isBytes {
				row[column] = string(byteValue)
			} else {
				row[column] = value
			}
		}
		result = append(result, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("遍历结果出错: %w", rowsErr)
	}
	return result, nil
}

// ErrNoDB 暴露「默认连接未打开」错误，方便调用方判断
var ErrNoDB = errNoDB

// IsNoDB 判断错误是否为「默认连接未打开」
func IsNoDB(checkErr error) bool {
	return errors.Is(checkErr, errNoDB)
}
