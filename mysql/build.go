package mysql

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// pointTextPattern 匹配 ST_AsText(btPoint) 输出的坐标文本，如 "POINT(116.39 39.9)"，
// 兼容小写 point、正负号与多余空白，对应 PHP 中的 preg_match('/POINT\((-?[\d\.]+)\s(-?[\d\.]+)\)/i', ...)
var pointTextPattern = regexp.MustCompile(`(?i)POINT\(\s*(-?[\d.]+)\s+(-?[\d.]+)\s*\)`)

// tableColumnsCache 表结构列名缓存，key 为表名；避免每次查询都访问 information_schema
var tableColumnsCache sync.Map

// errSelectInTrans 事务里执行查询时的错误：事务只用于写，SELECT 一律走连接池（mysql.Table）。
var errSelectInTrans = errors.New("事务里不允许执行查询（SELECT），查询请用 mysql.Table(...) 走连接池")

// Builder 查询构建器，支持链式调用。
// trx 非空时（由 DbTrans.Table 创建）所有语句都跑在该事务上，为空时走连接池 DB；
// 事务里只允许写，查询会被 errSelectInTrans 拦下。
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
	decodePoint   []string
}

// PagingData 分页查询结果
type PagingData struct {
	Recode  uint64 `json:"recode"`  // 总记录数
	Total   uint64 `json:"total"`   // 总页数
	Size    uint64 `json:"size"`    // 每页记录数
	Current uint64 `json:"current"` // 当前页号
}

// rawSQLValue 原生 SQL 片段值，标记该字段应直接拼入 SQL 而不走参数化
type rawSQLValue struct {
	sqlText string
}

type GpsPoint struct {
	Longitude float64 `json:"longitude" binding:"required"`
	Latitude  float64 `json:"latitude" binding:"required"`
}

// Point 构造一个原生 SQL 片段值，用于 MySQL 函数/表达式字段（如空间类型 POINT）。
// 用法: mysql.Table("tabBlacklist").Insert(mysql.M("btPoint", mysql.Raw("POINT(116.39 39.9)")))
// 注意: Point 值不会经过参数化转义，调用方必须保证 sqlText 内容安全（无 SQL 注入）。
func Point(gps *GpsPoint) any {
	return rawSQLValue{sqlText: fmt.Sprintf("ST_GeomFromText('POINT(%f %f)')", gps.Longitude, gps.Latitude)}
}

// Table 创建一个指定表的查询构建器（使用默认 DB）
func Table(table string) *Builder {
	return &Builder{
		db:    DB,
		table: table,
	}
}

// TableWithDB 使用指定数据库实例的查询构建器
func TableWithDB(db *sql.DB, table string) *Builder {
	return &Builder{
		db:    db,
		table: table,
	}
}

// Raw 构造一个原生 SQL 片段值，供 SET / INSERT 的值位置写 SQL 表达式（如字段自增）。
// 用法: mysql.Table("tabVoucher").Update(mysql.M("voNumber", mysql.Raw("voNumber + 10")), mysql.M("voID", 1))
// 注意: 片段不做参数化转义，调用方必须保证内容安全（不得把用户输入直接拼进来）；
// 片段内也不要写占位符 ?（参数只按 map 顺序拼接，片段里的 ? 取不到对应值）。
func Raw(sqlText string) any {
	return rawSQLValue{sqlText: sqlText}
}

// exec 执行写语句（Insert / InsertBatch / Update / Delete 的唯一出口）：
// 有事务走事务，否则走连接池。事务里的语句一出错就地回滚整笔事务，
// 所以调用方按错误返回即可，不需要再手动 Rollback（事务已结束，后续语句只会拿到 sql.ErrTxDone）。
func (builder *Builder) exec(query string, args ...any) (sql.Result, error) {
	if builder.trx == nil {
		return builder.db.Exec(query, args...)
	}
	result, execErr := builder.trx.Exec(query, args...)
	if execErr != nil {
		return result, builder.rollbackOnErr(execErr)
	}
	return result, nil
}

// rollbackOnErr 事务里出错的统一收口：先回滚整笔事务，再把原错误交回调用方。
// 非事务构建器（走连接池）与 err 为 nil 时原样返回，不做任何回滚。
// 回滚失败只记日志：调用方手上那个错误才是要报出去的，不能被回滚失败覆盖。
func (builder *Builder) rollbackOnErr(returnErr error) error {
	if builder.trx == nil || returnErr == nil {
		return returnErr
	}
	if rollbackErr := builder.trx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		logError("事务回滚失败", rollbackErr)
	}
	return returnErr
}

// query 执行查询语句：查询只走连接池，事务里调用直接返回错误
func (builder *Builder) query(query string, args ...any) (*sql.Rows, error) {
	if builder.trx != nil {
		return nil, errSelectInTrans
	}
	return builder.db.Query(query, args...)
}

// queryRow 查询单行：查询只走连接池，事务里调用属于编码错误，直接 panic
// （*sql.Row 带不了错误；正常也走不到这里——事务路径下的存在性探测已跳过）
func (builder *Builder) queryRow(query string, args ...any) *sql.Row {
	if builder.trx != nil {
		panic(errSelectInTrans)
	}
	return builder.db.QueryRow(query, args...)
}

// Select 指定查询字段，默认 *
func (builder *Builder) Select(fields ...string) *Builder {
	builder.fields = fields
	return builder
}

// Distinct 消除查询结果中的重复行（SELECT DISTINCT）。
// 与 Select / SelectSkip 连用，结果按所选列去重。
// 用法: mysql.Table("tabVoucher").Distinct().Select("voState").Pluck("voState")
func (builder *Builder) Distinct() *Builder {
	builder.distinct = true
	return builder
}

// SelectSkip 查询除指定字段外的所有列。
// 其余列自动从 information_schema 获取（首次查询后缓存），无需手写完整字段列表。
// 与 Select 互斥：调用 Select 后 SelectSkip 不生效。
// 用法: mysql.Table("tabPrice").SelectSkip("priceValue").Paging(page, pageSize)
func (builder *Builder) SelectSkip(fields ...string) *Builder {
	builder.skipFields = fields
	return builder
}

// WhereArg 添加原生 SQL 片段条件（可多次调用，用 AND 连接），片段里的占位符按顺序取 args。
// 除函数、OR 组合等原生表达式外，优先用 Where 的键后缀写法，少写字符串更不容易写错条件。
func (builder *Builder) WhereArg(query string, args ...any) *Builder {
	builder.where = append(builder.where, query)
	builder.whereArgs = append(builder.whereArgs, args...)
	return builder
}

// Where 添加 WHERE 条件（多个条件用 AND 连接）。
// 多组 OR 条件（(a AND b) OR (c AND d)）用 WhereOr；更深的嵌套用 WhereArg 写原生片段。
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
// 每组用 mysql.M 构造；组不能为空（空组会拼出 () 这种非法 SQL，直接报错）。
//
//	builder.WhereOr(mysql.M("key1", 3, "key2", 23), mysql.M("key1", 5, "key3", 23))
//	builder.WhereOr(mysql.M("key6", 23), mysql.M("key88", 23))
//
// 上面两行合起来就是 ((key1 = ? AND key2 = ?) OR (key1 = ? AND key3 = ?)) AND ((key6 = ?) OR (key88 = ?))。
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

// whereConditions 把 Where 的入参归一成条件 map：
//   - 只传一个 map[string]any（mysql.M 的返回值，或 All / Update / Delete 转发的条件 map）时直接用它；
//   - 否则按「键、值」成对解析。
//
// 参数不合法时返回错误而绝不静默当「没有条件」：条件一旦丢掉，查询会捞全表，
// 更新 / 删除会落到整张表上。
func whereConditions(args []any) (map[string]any, error) {
	switch len(args) {
	case 0:
		return nil, nil
	case 1:
		if args[0] == nil {
			// 显式传 nil 等同没有条件（All(nil) / Delete(nil) 这类写法）
			return nil, nil
		}
		if conditions, isMap := args[0].(map[string]any); isMap {
			return conditions, nil
		}
	}

	if len(args)%2 != 0 {
		return nil, fmt.Errorf("入参Where 必须成对出现（键、值），实际 %d 个；原生 SQL 片段请用 WhereArg", len(args))
	}

	conditions := make(map[string]any, len(args)/2)
	for index := 0; index < len(args); index += 2 {
		field, isString := args[index].(string)
		if !isString {
			return nil, fmt.Errorf("入参Where 的键必须是字符串，实际 %T", args[index])
		}
		conditions[field] = args[index+1]
	}
	return conditions, nil
}

// WhereIn 添加 WHERE field IN (values...) 条件
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

// OrderBy 排序，如 "id DESC"、"create_time ASC"
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

// GroupBy 分组
func (builder *Builder) GroupBy(groupBy string) *Builder {
	builder.groupBy = groupBy
	return builder
}

// Having 添加 HAVING 条件
func (builder *Builder) Having(query string, args ...any) *Builder {
	builder.having = query
	builder.havingArgs = args
	return builder
}

// Decode 指定查询结果中需要解码的字段名（可多个）
// 字符串字段 → json.Unmarshal，整型字段 → 位拆分（如 7 → [1,2,4]）
func (builder *Builder) Decode(fieldNames ...string) *Builder {
	builder.decodeFields = fieldNames
	return builder
}

// DecodePoint 指定查询结果中需要按 ST_AsText 文本解析的 POINT 字段名（可多个），
// 解析后字段值变为 {lng, lat} 对象。
// 与 Decode 分离：仅对显式声明的字段做 POINT 正则解析，避免普通字段每次解码都做正则匹配。
func (builder *Builder) DecodePoint(fieldNames ...string) *Builder {
	builder.decodePoint = fieldNames
	return builder
}

// decodeResults 对查询结果中的指定字段执行解码
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
			case float64:
				row[fieldName] = builder.bitSplit(uint64(casted))

			case []byte:
				strVal := string(casted)
				var decoded any
				if jsonErr := json.Unmarshal([]byte(strVal), &decoded); jsonErr == nil {
					row[fieldName] = decoded
				}
			}
		}
	}
}

// decodePointResults 对查询结果中显式声明的 POINT 文本字段执行解析，
// 仅当 builder 声明了 DecodePoint 字段时才会执行，普通查询零开销。
func (builder *Builder) decodePointResults(results []map[string]any) {
	if len(builder.decodePoint) == 0 {
		return
	}
	fieldSet := make(map[string]bool, len(builder.decodePoint))
	for _, fieldName := range builder.decodePoint {
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
				if pointObj := ParsePointText(casted); pointObj != nil {
					row[fieldName] = pointObj
				}
			case []byte:
				if pointObj := ParsePointText(string(casted)); pointObj != nil {
					row[fieldName] = pointObj
				}
			}
		}
	}
}

// bitSplit 将整数按位拆分，例如 7 → [1, 2, 4]
func (builder *Builder) bitSplit(value uint64) []uint64 {
	if value <= 0 {
		return nil
	}
	bits := make([]uint64, 0)
	var bit uint64 = 1
	for bit <= value {
		if value&bit != 0 {
			bits = append(bits, bit)
		}
		bit <<= 1
	}
	return bits
}

// buildQuery 构建完整的 SQL 查询语句和参数
func (builder *Builder) buildQuery() (string, []any, error) {
	// WHERE 键后缀解析失败时不继续拼 SQL，把错误原样交给调用方
	if builder.whereParseErr != nil {
		return "", nil, builder.whereParseErr
	}

	// SELECT 字段
	fieldStr := "*"
	if len(builder.fields) > 0 {
		fieldStr = strings.Join(builder.fields, ", ")

	} else if len(builder.skipFields) > 0 {
		// SelectSkip：动态取全列后剔除指定字段，避免手写完整字段列表
		allColumns, columnErr := getTableColumns(builder.db, builder.table)
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

	query := fmt.Sprintf("%s %s %s %s", "SELECT", fieldStr, "FROM", builder.table)

	// WHERE
	if len(builder.where) > 0 {
		query += " WHERE " + strings.Join(builder.where, " AND ")
	}

	// GROUP BY
	if builder.groupBy != "" {
		query += " GROUP BY " + builder.groupBy
		// HAVING
		if builder.having != "" {
			query += " HAVING " + builder.having
		}
	}

	// ORDER BY
	if builder.orderBy != "" {
		query += " ORDER BY " + builder.orderBy
	}

	// LIMIT
	if builder.limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", builder.limit)
	}

	// OFFSET
	if builder.offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", builder.offset)
	}

	args := builder.whereArgs
	if len(builder.havingArgs) > 0 {
		args = append(args, builder.havingArgs...)
	}

	return query, args, nil
}

// Paging 分页查询，返回分页数据，pageSize 默认 20
// 用法: pagingData, err := mysql.Table("tabUser").Paginate(1)             // 每页20条
//
//	pagingData, err := mysql.Table("tabUser").Paginate(1, 50)        // 每页50条
func (builder *Builder) Paging(page int64, pageSize ...int64) ([]map[string]any, *PagingData, error) {
	var actualPageSize int64 = 20
	if len(pageSize) > 0 {
		actualPageSize = pageSize[0]
	}

	// 先查总数
	countBuilder := &Builder{
		db:            builder.db,
		trx:           builder.trx,
		table:         builder.table,
		where:         builder.where,
		whereArgs:     builder.whereArgs,
		whereParseErr: builder.whereParseErr,
		groupBy:       builder.groupBy,
		having:        builder.having,
		havingArgs:    builder.havingArgs,
	}
	total, err := countBuilder.Count()
	if err != nil {
		return nil, nil, err
	}

	// 查分页数据
	if actualPageSize > 0 {
		builder.Limit(actualPageSize).Offset((page - 1) * actualPageSize)
	}

	rows, err := builder.All()
	if err != nil {
		return nil, nil, err
	}

	totalPages := uint64(1)

	if actualPageSize > 0 {
		totalPages = (total + uint64(actualPageSize) - 1) / uint64(actualPageSize)
	}

	p := &PagingData{
		Recode:  total,
		Total:   totalPages,
		Size:    uint64(actualPageSize),
		Current: uint64(page),
	}
	return rows, p, nil
}

// All 执行查询，返回所有结果行；查询只走连接池，事务里调用直接报错
// （Get / Count / Value / Pluck / Paging 都经这里，所以一并被拦下）
// conditions 可省略，写法与 Where 一致，两种都行：
//
//	rows := mysql.Table("tabUser").All()                     // SELECT * FROM tabUser
//	rows := mysql.Table("tabUser").All("status", 1)
//	rows := mysql.Table("tabUser").All(mysql.M("status", 1))
func (builder *Builder) All(conditions ...any) ([]map[string]any, error) {
	if builder.trx != nil {
		return nil, errSelectInTrans
	}
	builder.Where(conditions...)
	query, queryArgs, buildErr := builder.buildQuery()
	if buildErr != nil {
		return nil, buildErr
	}
	queryBegin := time.Now()
	rows, err := builder.query(query, queryArgs...)
	recordSQL(time.Since(queryBegin), query, queryArgs...)
	if err != nil {
		logError(fmt.Sprintf("Mysql执行 Query(%s)报错", query), err)
		return nil, fmt.Errorf("MYSQL查询失败 [%s]: %w", query, err)
	}
	results, buildErr2 := buildResult(rows)
	if buildErr2 != nil {
		return nil, buildErr2
	}
	builder.decodeResults(results)
	builder.decodePointResults(results)
	return results, nil
}

// Get 返回第一条结果（没有数据时返回 nil, nil）
// conditions 可省略；传入时自动追加为 WHERE 条件，写法与 Where 一致：
//
//	row, queryErr := mysql.Table("tabActivities").Get("actID", request.ID)
//	row, queryErr := mysql.Table("tabActivities").Get(mysql.M("actID", request.ID))
func (builder *Builder) Get(conditions ...any) (map[string]any, error) {
	builder.Limit(1)
	results, err := builder.All(conditions...)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0], nil
}

// InsertBatch 批量插入多行数据，返回影响行数。
// 字段列表取自第一行，各行必须使用同一套键（键可带操作符后缀，见 insertOperators）。
// 用法: affected, err := mysql.Table("user").InsertBatch([]map[string]any{
//
//	mysql.M("name", "张三", "age", 25),
//	mysql.M("name", "李四", "age", 30),
//
//	})
func (builder *Builder) InsertBatch(rows []map[string]any) (uint64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	// 以第一行的 key 作为字段名（排序保证字段顺序固定）
	firstRow := rows[0]
	fieldList := make([]string, 0, len(firstRow))
	for field := range firstRow {
		fieldList = append(fieldList, field)
	}
	sort.Strings(fieldList)

	// 列名与值表达式由第一行决定（各行键名需一致），逐行只换参数
	columnList := make([]string, 0, len(fieldList))
	for _, rawField := range fieldList {
		columnName, _, _, _, fieldErr := buildInsertField(rawField, firstRow[rawField])
		if fieldErr != nil {
			return 0, fieldErr
		}
		columnList = append(columnList, columnName)
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
		builder.table,
		strings.Join(columnList, ", "),
		strings.Join(placeholderList, ", "))

	execBegin := time.Now()
	result, execErr := builder.exec(query, argList...)
	recordSQL(time.Since(execBegin), query, argList...)
	if execErr != nil {
		logError(fmt.Sprintf("Mysql执行 InsertBatch(%s)报错", query), execErr)
		return 0, fmt.Errorf("批量插入失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return uint64(affectedRows), nil
}

// Insert 插入一行数据，返回自增ID。
// 键可带操作符后缀（见 insertOperators）：
//
//	mysql.M("btPoint@", "POINT(116.39 39.9)") 生成 ST_GeomFromText(?)
//	mysql.M("expTime\\", "now()")              表达式原样拼入 SQL，安全由调用方保证
//	mysql.M("content#", "长文本")              值先做 zlib 压缩再写入（列须为二进制列）
//
// 用法: id, err := mysql.Table("user").Insert(mysql.M("name", "张三", "age", 25))
func (builder *Builder) Insert(data map[string]any) (uint64, error) {
	// 排序保证字段顺序固定
	fieldList := make([]string, 0, len(data))
	for field := range data {
		fieldList = append(fieldList, field)
	}
	sort.Strings(fieldList)

	columnList := make([]string, 0, len(data))
	placeholderList := make([]string, 0, len(data))
	argList := make([]any, 0, len(data))

	for _, rawField := range fieldList {
		columnName, valueExpr, arg, hasArg, fieldErr := buildInsertField(rawField, data[rawField])
		if fieldErr != nil {
			return 0, fieldErr
		}
		columnList = append(columnList, columnName)
		placeholderList = append(placeholderList, valueExpr)
		if hasArg {
			argList = append(argList, arg)
		}
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		builder.table,
		strings.Join(columnList, ", "),
		strings.Join(placeholderList, ", "))

	execBegin := time.Now()
	result, execErr := builder.exec(query, argList...)
	recordSQL(time.Since(execBegin), query, argList...)
	if execErr != nil {
		logError(fmt.Sprintf("Mysql执行 Insert(%s)报错", query), execErr)
		return 0, fmt.Errorf("插入失败 [%s]: %w", query, execErr)
	}
	lastInsertID, insertErr := result.LastInsertId()
	if insertErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取自增ID失败 [%s]: %w", query, insertErr))
	}
	return uint64(lastInsertID), nil
}

// Update 更新数据，返回影响行数。
// data 的键可带操作符后缀（见 setOperators），生成字段自运算：
//
//	mysql.M("voNumber+", 10)  生成 voNumber = voNumber + ?
//	mysql.M("voStock-", 1)    生成 voStock = voStock - ?
//	mysql.M("voFlag|", 8)     生成 voFlag = voFlag | ?（按位置位）
//	mysql.M("voFlag^", 8)     生成 voFlag = voFlag ^ ?（翻转位）
//	mysql.M("voFlag!", 8)     生成 voFlag = voFlag - (voFlag & ?)（清位）
//	mysql.M("voName.", "_x")  生成 voName = CONCAT(voName, ?)
//
// 不带后缀时直接赋值；mysql.Raw 片段优先于后缀解析。
// conditions 写法与 Where 一致：Update(data, "voID", 1) 或 Update(data, mysql.M("voID", 1))；
// 没有条件会直接报错，禁止整表更新。
func (builder *Builder) Update(data map[string]any, conditions ...any) (uint64, error) {
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

	query := fmt.Sprintf("UPDATE %s SET %s", builder.table, strings.Join(setClauses, ", "))

	if len(builder.where) > 0 {
		query += " WHERE " + strings.Join(builder.where, " AND ")
		argList = append(argList, builder.whereArgs...)
	}

	execBegin := time.Now()
	result, execErr := builder.exec(query, argList...)
	recordSQL(time.Since(execBegin), query, argList...)
	if execErr != nil {
		logError(fmt.Sprintf("Mysql执行 Update(%s)报错", query), execErr)
		return 0, fmt.Errorf("更新失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}

	// 事务里不做存在性探测：查询不允许走事务，需要判断记录是否存在请在 mysql.Begin() 之前查好
	if affectedRows == 0 && len(builder.where) > 0 && builder.trx == nil {
		// affectedRows 为 0 可能是"没有符合条件的记录"，也可能是"记录存在但值未变化"。
		// 用相同的表名和 where 条件查 1 条即可：只要有 1 条或以上都算有符合条件的记录。
		verifyQuery := fmt.Sprintf("SELECT 1 FROM %s WHERE %s LIMIT 1", builder.table, strings.Join(builder.where, " AND "))
		var hasRecord bool
		verifyBegin := time.Now()
		queryErr := builder.queryRow(verifyQuery, builder.whereArgs...).Scan(&hasRecord)
		recordSQL(time.Since(verifyBegin), verifyQuery, builder.whereArgs...)
		if errors.Is(queryErr, sql.ErrNoRows) {
			return 0, fmt.Errorf("未更新：没有符合条件（%s）的记录", strings.Join(builder.where, " AND "))
		}
		if queryErr != nil {
			return 0, fmt.Errorf("更新后校验记录是否存在失败 [%s]: %w", verifyQuery, queryErr)
		}
	}

	return uint64(affectedRows), nil
}

// Delete 删除数据，返回影响行数。
// conditions 写法与 Where 一致：Delete("refID", id) 或 Delete(mysql.M("refID", id))；
// 没有条件会直接报错，禁止整表删除。
func (builder *Builder) Delete(conditions ...any) (int64, error) {
	builder.Where(conditions...)
	if builder.whereParseErr != nil {
		return 0, builder.whereParseErr
	}
	if len(builder.where) == 0 {
		return 0, fmt.Errorf("禁止不带条件删除（%s）：请用 Where / WhereOr 或 Delete 的 conditions 指定条件；确实要清空表请显式写 WhereArg(\"1 = 1\")", builder.table)
	}

	// 删除前先用相同的表名和 where 条件查 1 条：没有符合条件的数据就直接报错，不再执行删除。
	// 事务里不做存在性探测：查询不允许走事务，需要判断记录是否存在请在 mysql.Begin() 之前查好
	if len(builder.where) > 0 && builder.trx == nil {
		verifyQuery := fmt.Sprintf("SELECT 1 FROM %s WHERE %s LIMIT 1", builder.table, strings.Join(builder.where, " AND "))
		var hasRecord bool
		verifyBegin := time.Now()
		verifyErr := builder.queryRow(verifyQuery, builder.whereArgs...).Scan(&hasRecord)
		recordSQL(time.Since(verifyBegin), verifyQuery, builder.whereArgs...)
		if errors.Is(verifyErr, sql.ErrNoRows) {
			return 0, fmt.Errorf("删除失败：没有符合条件的记录")
		}
		if verifyErr != nil {
			return 0, fmt.Errorf("删除前校验记录是否存在失败 [%s]: %w", verifyQuery, verifyErr)
		}
	}

	query := fmt.Sprintf("DELETE FROM %s", builder.table)
	if len(builder.where) > 0 {
		query += " WHERE " + strings.Join(builder.where, " AND ")
	}

	execBegin := time.Now()
	result, execErr := builder.exec(query, builder.whereArgs...)
	recordSQL(time.Since(execBegin), query, builder.whereArgs...)
	if execErr != nil {
		logError(fmt.Sprintf("Mysql执行 Delete(%s)报错", query), execErr)
		return 0, fmt.Errorf("删除失败 [%s]: %w", query, execErr)
	}
	affectedRows, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return 0, builder.rollbackOnErr(fmt.Errorf("获取影响行数失败 [%s]: %w", query, affectedErr))
	}
	return affectedRows, nil
}

// Value 返回单个字段的值
func (builder *Builder) Value(field string) (any, error) {
	builder.fields = []string{field}
	row, err := builder.Get()
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return row[field], nil
}

// Count 返回 COUNT 结果
func (builder *Builder) Count() (uint64, error) {
	builder.fields = []string{"COUNT(*) AS total"}
	row, err := builder.Get()
	if err != nil {
		return 0, err
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

// Pluck 返回某一列的所有值
func (builder *Builder) Pluck(field string) ([]any, error) {
	builder.fields = []string{field}
	results, err := builder.All()
	if err != nil {
		return nil, err
	}
	values := make([]any, 0, len(results))
	for _, row := range results {
		values = append(values, row[field])
	}
	return values, nil
}

// buildResult 将 *sql.Rows 转换为 []map[string]any
func buildResult(rows *sql.Rows) ([]map[string]any, error) {
	defer func(rows *sql.Rows) {
		_ = rows.Close()
	}(rows)

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("获取列名失败: %w", err)
	}

	result := make([]map[string]any, 0)

	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}

		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("扫描行数据失败: %w", err)
		}

		row := make(map[string]any)
		for index, column := range columns {
			value := values[index]
			if byteVal, ok := value.([]byte); ok {
				row[column] = string(byteVal)
			} else {
				row[column] = value
			}
		}
		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历结果出错: %w", err)
	}

	return result, nil
}
