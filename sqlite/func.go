package sqlite

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// columnNamePattern 合法列名：字母数字下划线，允许「表名.列名」形式（多表 JOIN 时用得上）。
var columnNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)?$`)

// setOperators UPDATE 的 SET 子句支持的键后缀（按长度降序，优先匹配多字符）。
//   - 算术：+ - * /（注意 SQLite 里整数相除是整除，要小数请先把列定义为 REAL 或先乘后除）
//   - 位运算：| 位或（置位）、^ 位翻转（SQLite 没有异或运算符，用 (col | v) - (col & v) 等价表达）、
//     ! 清位（col - (col & v)）
//   - 字符串拼接：.（SQLite 用 || 而非 CONCAT）
var setOperators = []string{"+", "-", "*", "/", "|", "^", "!", "."}

// whereOperators WHERE 条件支持的键后缀（按长度降序，优先匹配多字符）。
//   - 比较：> < >= <= ! != <>
//   - LIKE：~ 自动补 %（规则见 wrapLikeValue）；!~ 为 NOT LIKE
//   - 位标记：& 位含（col > 0 且按位命中，值为 0 时恒不命中）、&? 位含（col 为 0 也算命中）、!& 位不含
//   - 其它：# between、!# not between、@ in、!@ not in、% mod、!% mod 不等于
var whereOperators = []string{
	"!&", "!#", "!@", "!%", "!~", "!=", "<>", ">=", "<=", "&?",
	"LIKE",
	">", "<", "~", "!", "&", "#", "@", "%", "=",
}

// whereUnsupported SQLite 没有的能力：命中这些后缀时给出明确原因，而不是含糊的“字段名不合法”。
var whereUnsupported = map[string]string{
	"!$": "全文检索（SQLite 的全文索引是 FTS5 虚表，不是普通表上的函数）",
	"$":  "全文检索（SQLite 的全文索引是 FTS5 虚表，不是普通表上的函数）",
	"!*": "正则匹配（SQLite 默认没有 REGEXP 函数，需要自己注册或改用 GLOB/LIKE）",
	"*":  "正则匹配（SQLite 默认没有 REGEXP 函数，需要自己注册或改用 GLOB/LIKE）",
}

// insertOperators INSERT 值位置支持的键后缀（按长度降序）。
//   - \ 原生 SQL 表达式（值原样拼入，不参数化，安全由调用方保证）
//   - # 值先做 zlib 压缩再写入（目标列须为 BLOB）
var insertOperators = []string{"\\", "#"}

// insertUnsupported 同样是 SQLite 没有的能力
var insertUnsupported = map[string]string{
	"@": "空间函数（SQLite 没有 ST_GeomFromText，坐标请存两列或存 WKT 文本）",
}

// allFieldSuffixes 三张后缀表 + 不支持后缀的合集（按长度降序），供 TrimFieldOperator 剥离后缀用。
var allFieldSuffixes = []string{
	"!&", "!#", "!@", "!%", "!~", "!$", "!*", "!=", "<>", ">=", "<=", "&?", "LIKE",
	"+", "-", "*", "/", "|", "^", "!", "$", "@", "#", ".",
	"\\", ">", "<", "~", "&", "%", "=",
}

// insertCompressLevel 插入时 # 后缀的 zlib 压缩级别
const insertCompressLevel = 5

// M 快速构建 map[string]any，简化 Where / WhereOr / Update / Insert 调用。
// 键可以带操作符后缀，含义由使用位置决定（见 setOperators / whereOperators / insertOperators）：
//
//	Where:  M("userState", 1)、M("userName~", kw)、M("loginTime#", []any{start, end})、M("userID@", ids)
//	Update: M("voNumber+", 10)、M("userFlag|", 8)、M("userFlag^", 8)、M("userNotes.", "追加")
//	Insert: M("rawTime\\", "datetime('now')")、M("content#", longText)
//
// 用法: sqlite.M("userID", 1)、sqlite.M("voNumber+", 10)
func M(pairs ...any) map[string]any {
	conditions := make(map[string]any, len(pairs)/2)
	for index := 0; index < len(pairs)-1; index += 2 {
		field, ok := pairs[index].(string)
		if !ok {
			continue
		}
		conditions[field] = pairs[index+1]
	}
	return conditions
}

// TrimFieldOperator 剥离键上的操作符后缀，返回纯列名（没有后缀时原样返回）。
// 供字段白名单过滤使用：白名单里存的是纯列名，带后缀的键要先还原成列名再校验。
func TrimFieldOperator(rawKey string) string {
	columnName, _ := splitFieldKey(rawKey, allFieldSuffixes)
	return columnName
}

// splitFieldKey 按键后缀表拆分键，返回列名与命中的操作符（未命中时操作符为空串）。
// 后缀表必须按长度降序排列，否则 "!&" 会被 "&" 抢先匹配。
func splitFieldKey(rawKey string, operatorTable []string) (string, string) {
	trimmedKey := strings.TrimSpace(rawKey)
	for _, candidateOperator := range operatorTable {
		if strings.HasSuffix(trimmedKey, candidateOperator) {
			return strings.TrimSpace(strings.TrimSuffix(trimmedKey, candidateOperator)), candidateOperator
		}
	}
	return trimmedKey, ""
}

// matchUnsupportedSuffix 命中 SQLite 不支持的后缀时返回它的说明
func matchUnsupportedSuffix(rawKey string, unsupported map[string]string) (string, bool) {
	trimmedKey := strings.TrimSpace(rawKey)
	for candidateOperator, reason := range unsupported {
		if strings.HasSuffix(trimmedKey, candidateOperator) {
			return reason, true
		}
	}
	return "", false
}

// buildWhereCondition 按 WHERE 键后缀生成 SQL 片段与参数。
// 片段里已带好列名与占位符（IN / BETWEEN 会展开成多个占位符），返回值参数与占位符顺序一一对应。
//
// 列名部分支持多列写法，逗号与加号等价，都表示「把这几列拼起来再比对」（SQLite 用 || 拼接）：
//
//	"userName,userNick~" 与 "userName+userNick~" 都生成 ((userName || userNick) LIKE ?)
//
// 多列只对 ~ / !~ 有意义，其它操作符配多列直接报错。
func buildWhereCondition(rawKey string, rawValue any) (string, []any, error) {
	if reason, isUnsupported := matchUnsupportedSuffix(rawKey, whereUnsupported); isUnsupported {
		return "", nil, fmt.Errorf("sqlite 不支持该条件后缀（%s）: %q", reason, rawKey)
	}

	columnPart, operator := splitFieldKey(rawKey, whereOperators)
	if columnPart == "" {
		return "", nil, fmt.Errorf("where 条件字段名不合法: %q", rawKey)
	}

	// 逗号与加号是仅有的两个多列分隔符（两者等价）
	columnNames := strings.FieldsFunc(columnPart, func(separator rune) bool {
		return separator == ',' || separator == '+'
	})
	if len(columnNames) == 1 {
		return buildColumnCondition(strings.TrimSpace(columnNames[0]), operator, rawValue, rawKey)
	}

	if operator != "~" && operator != "!~" {
		return "", nil, fmt.Errorf("where 多列（逗号/加号分隔）只支持 like 后缀 ~ / !~，当前后缀 %q: %q", operator, rawKey)
	}

	columnList := make([]string, 0, len(columnNames))
	for _, oneColumn := range columnNames {
		columnName := strings.TrimSpace(oneColumn)
		if !columnNamePattern.MatchString(columnName) {
			return "", nil, fmt.Errorf("where 条件字段名不合法: %q", rawKey)
		}
		columnList = append(columnList, columnName)
	}

	likeText, likeErr := toTextValue(rawValue)
	if likeErr != nil {
		return "", nil, fmt.Errorf("where like(%s) 值不合法: %v", rawKey, likeErr)
	}
	likeKeyword := "LIKE"
	if operator == "!~" {
		likeKeyword = "NOT LIKE"
	}
	concatText := "(" + strings.Join(columnList, " || ") + ")"
	return fmt.Sprintf("%s %s ?", concatText, likeKeyword), []any{wrapLikeValue(likeText)}, nil
}

// buildColumnCondition 生成单列条件：列名必须合法，值按操作符做类型校验。
func buildColumnCondition(columnName string, operator string, rawValue any, rawKey string) (string, []any, error) {
	if !columnNamePattern.MatchString(columnName) {
		return "", nil, fmt.Errorf("where 条件字段名不合法: %q", rawKey)
	}

	switch operator {
	case "", "=":
		return columnName + " = ?", []any{rawValue}, nil

	case ">", "<", ">=", "<=", "!=", "<>":
		return fmt.Sprintf("%s %s ?", columnName, operator), []any{rawValue}, nil

	case "!":
		return fmt.Sprintf("%s != ?", columnName), []any{rawValue}, nil

	case "~", "!~", "LIKE":
		likeText, likeErr := toTextValue(rawValue)
		if likeErr != nil {
			return "", nil, fmt.Errorf("where like(%s) 值不合法: %v", rawKey, likeErr)
		}
		if operator == "LIKE" {
			// 词形写法：值原样传入，不自动补 %（要自动包裹请用 ~）
			return fmt.Sprintf("%s LIKE ?", columnName), []any{likeText}, nil
		}
		likeKeyword := "LIKE"
		if operator == "!~" {
			likeKeyword = "NOT LIKE"
		}
		return fmt.Sprintf("%s %s ?", columnName, likeKeyword), []any{wrapLikeValue(likeText)}, nil

	case "&", "&?", "!&":
		flagValue, flagErr := toUnsignedInt(rawValue)
		if flagErr != nil {
			return "", nil, fmt.Errorf("where 位标记(%s) 值不合法: %v", rawKey, flagErr)
		}
		if flagValue == 0 {
			// 位标记为 0：与 0 做按位与恒为 0，所以「含」恒不命中、「不含」恒命中；
			// &? 的定义本身就带「字段为 0 也算命中」，此时自然落在「字段为 0」上。
			switch operator {
			case "&":
				return "1 = 0", nil, nil
			case "!&":
				return "1 = 1", nil, nil
			default:
				return fmt.Sprintf("%s = 0", columnName), nil, nil
			}
		}
		switch operator {
		case "&":
			return fmt.Sprintf("%s > 0 AND (%s & ?) > 0", columnName, columnName), []any{flagValue}, nil
		case "&?":
			return fmt.Sprintf("(%s = 0 OR (%s & ?) > 0)", columnName, columnName), []any{flagValue}, nil
		default:
			return fmt.Sprintf("(%s = 0 OR (%s & ?) = 0)", columnName, columnName), []any{flagValue}, nil
		}

	case "#", "!#":
		rangeValues, rangeErr := toSliceValues(rawValue)
		if rangeErr != nil {
			return "", nil, fmt.Errorf("where between(%s) 值不合法: %v", rawKey, rangeErr)
		}
		if len(rangeValues) != 2 {
			return "", nil, fmt.Errorf("where between(%s) 需要 2 个值（起止），实际 %d 个", rawKey, len(rangeValues))
		}
		betweenKeyword := "BETWEEN"
		if operator == "!#" {
			betweenKeyword = "NOT BETWEEN"
		}
		return fmt.Sprintf("%s %s ? AND ?", columnName, betweenKeyword), rangeValues, nil

	case "@", "!@":
		inValues, inErr := toSliceValues(rawValue)
		if inErr != nil {
			return "", nil, fmt.Errorf("where in(%s) 值不合法: %v", rawKey, inErr)
		}
		inKeyword := "IN"
		if operator == "!@" {
			inKeyword = "NOT IN"
		}
		if len(inValues) == 0 {
			// 空集合无法展开占位符，用恒假 / 恒真替代，SQL 仍然合法
			if operator == "!@" {
				return "1 = 1", nil, nil
			}
			return "1 = 0", nil, nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(inValues)), ", ")
		return fmt.Sprintf("%s %s (%s)", columnName, inKeyword, placeholders), inValues, nil

	case "%", "!%":
		modValues, modErr := toSliceValues(rawValue)
		if modErr != nil {
			return "", nil, fmt.Errorf("where mod(%s) 值不合法: %v", rawKey, modErr)
		}
		if len(modValues) != 2 {
			return "", nil, fmt.Errorf("where mod(%s) 需要 2 个值（除数、余数），实际 %d 个", rawKey, len(modValues))
		}
		divisor, divisorErr := toUnsignedInt(modValues[0])
		if divisorErr != nil {
			return "", nil, fmt.Errorf("where mod(%s) 除数不合法: %v", rawKey, divisorErr)
		}
		if divisor == 0 {
			return "", nil, fmt.Errorf("where mod(%s) 除数不能为 0", rawKey)
		}
		compare := "="
		if operator == "!%" {
			compare = "!="
		}
		// SQLite 没有 MOD() 函数，用 % 运算符
		return fmt.Sprintf("%s %% ? %s ?", columnName, compare), []any{divisor, modValues[1]}, nil
	}

	return "", nil, fmt.Errorf("where 条件不支持的操作符后缀 %q: %q", operator, rawKey)
}

// wrapLikeValue 给 LIKE 的值补通配符：
//   - 值只要带了一个 %（开头或结尾都算），就认为通配符由调用方自己控制，原样使用，不再补；
//   - 没带 % 时按锚点补：以 ^ 开头不补前 %、以 $ 结尾不补后 %，两侧都没写就前后各补一个 %（前后包含）。
//
// 注意：SQLite 的 LIKE 默认对 ASCII 不区分大小写（PRAGMA case_sensitive_like 可改），且 % 与 _ 都是通配符，
// 与 MySQL 行为一致；需要区分大小写时可改用 GLOB（值同样由调用方自己拼通配符，用 ~ 会被自动补 %）。
func wrapLikeValue(likeText string) string {
	if likeText == "" {
		return likeText
	}

	prefixAnchor := strings.HasPrefix(likeText, "^")
	suffixAnchor := strings.HasSuffix(likeText, "$")
	if prefixAnchor {
		likeText = strings.TrimPrefix(likeText, "^")
	}
	if suffixAnchor {
		likeText = strings.TrimSuffix(likeText, "$")
	}

	if strings.HasPrefix(likeText, "%") || strings.HasSuffix(likeText, "%") {
		return likeText
	}

	if !prefixAnchor {
		likeText = "%" + likeText
	}
	if !suffixAnchor {
		likeText = likeText + "%"
	}
	return likeText
}

// buildSetClauses 生成 UPDATE 的 SET 子句与参数：键后缀决定是直接赋值还是字段自运算。
// 返回的子句形如 "voNumber = voNumber + ?"，参数与占位符顺序一致。
func buildSetClauses(data map[string]any) ([]string, []any, error) {
	sortedFields := make([]string, 0, len(data))
	for rawField := range data {
		sortedFields = append(sortedFields, rawField)
	}
	sort.Strings(sortedFields)

	setClauses := make([]string, 0, len(data))
	argList := make([]any, 0, len(data))

	for _, rawField := range sortedFields {
		columnValue := data[rawField]

		// 原生 SQL 片段优先：列名剥离后缀后整段拼接，不再做后缀解析
		if rawSQL, isRaw := columnValue.(rawSQLValue); isRaw {
			columnName := TrimFieldOperator(rawField)
			if !columnNamePattern.MatchString(columnName) {
				return nil, nil, fmt.Errorf("更新字段名不合法: %q", rawField)
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = %s", columnName, rawSQL.sqlText))
			continue
		}

		columnName, operator := splitFieldKey(rawField, setOperators)
		if !columnNamePattern.MatchString(columnName) {
			return nil, nil, fmt.Errorf("更新字段名不合法: %q", rawField)
		}

		switch operator {
		case "":
			setClauses = append(setClauses, fmt.Sprintf("%s = ?", columnName))
			argList = append(argList, columnValue)

		case "+", "-", "*", "/":
			numberValue, numberErr := toNumber(columnValue)
			if numberErr != nil {
				return nil, nil, fmt.Errorf("更新字段 %s 的算术运算值不合法: %v", rawField, numberErr)
			}
			if operator == "/" && numberValue == 0 {
				return nil, nil, fmt.Errorf("更新字段 %s 的除法运算值不能为 0", rawField)
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = %s %s ?", columnName, columnName, operator))
			argList = append(argList, columnValue)

		case "|":
			flagValue, flagErr := toUnsignedInt(columnValue)
			if flagErr != nil {
				return nil, nil, fmt.Errorf("更新字段 %s 的位运算值不合法: %v", rawField, flagErr)
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = %s | ?", columnName, columnName))
			argList = append(argList, flagValue)

		case "^":
			// SQLite 没有异或运算符：a ^ b 等价于 (a | b) - (a & b)
			flagValue, flagErr := toUnsignedInt(columnValue)
			if flagErr != nil {
				return nil, nil, fmt.Errorf("更新字段 %s 的位翻转值不合法: %v", rawField, flagErr)
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = (%s | ?) - (%s & ?)", columnName, columnName, columnName))
			argList = append(argList, flagValue, flagValue)

		case "!":
			// 清位：先把要清的位与出来，再从原值里减掉
			flagValue, flagErr := toUnsignedInt(columnValue)
			if flagErr != nil {
				return nil, nil, fmt.Errorf("更新字段 %s 的清位值不合法: %v", rawField, flagErr)
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = %s - (%s & ?)", columnName, columnName, columnName))
			argList = append(argList, flagValue)

		case ".":
			textValue, textErr := toTextValue(columnValue)
			if textErr != nil {
				return nil, nil, fmt.Errorf("更新字段 %s 的拼接值不合法: %v", rawField, textErr)
			}
			// SQLite 用 || 拼接（没有 CONCAT 函数）
			setClauses = append(setClauses, fmt.Sprintf("%s = (%s || ?)", columnName, columnName))
			argList = append(argList, textValue)

		default:
			return nil, nil, fmt.Errorf("更新不支持的操作符后缀 %q: %q", operator, rawField)
		}
	}

	return setClauses, argList, nil
}

// buildInsertField 解析 INSERT 的单个字段，返回列名、值位置的 SQL 表达式、参数与是否有参数。
// 支持的后缀见 insertOperators：\ 原生表达式、# zlib 压缩。
func buildInsertField(rawKey string, rawValue any) (string, string, any, bool, error) {
	if reason, isUnsupported := matchUnsupportedSuffix(rawKey, insertUnsupported); isUnsupported {
		return "", "", nil, false, fmt.Errorf("sqlite 不支持该插入后缀（%s）: %q", reason, rawKey)
	}

	if rawSQL, isRaw := rawValue.(rawSQLValue); isRaw {
		columnName := TrimFieldOperator(rawKey)
		if !columnNamePattern.MatchString(columnName) {
			return "", "", nil, false, fmt.Errorf("插入字段名不合法: %q", rawKey)
		}
		return columnName, rawSQL.sqlText, nil, false, nil
	}

	columnName, operator := splitFieldKey(rawKey, insertOperators)
	if !columnNamePattern.MatchString(columnName) {
		return "", "", nil, false, fmt.Errorf("插入字段名不合法: %q", rawKey)
	}

	switch operator {
	case "":
		return columnName, "?", rawValue, true, nil

	case "\\":
		expression, isText := rawValue.(string)
		if !isText {
			return "", "", nil, false, fmt.Errorf("插入字段 %s 的原生表达式必须是字符串", rawKey)
		}
		return columnName, expression, nil, false, nil

	case "#":
		compressedValue, compressErr := compressBytes(rawValue)
		if compressErr != nil {
			return "", "", nil, false, fmt.Errorf("插入字段 %s 压缩失败: %v", rawKey, compressErr)
		}
		return columnName, "?", compressedValue, true, nil
	}

	return "", "", nil, false, fmt.Errorf("插入不支持的操作符后缀 %q: %q", operator, rawKey)
}

// compressBytes 把字符串或字节切片按 zlib 压缩，供插入 # 后缀使用（目标列须为 BLOB）。
func compressBytes(rawValue any) ([]byte, error) {
	var payload []byte
	switch typed := rawValue.(type) {
	case string:
		payload = []byte(typed)
	case []byte:
		payload = typed
	default:
		return nil, fmt.Errorf("值类型 %T 不支持压缩，请先转成字符串或字节切片", rawValue)
	}

	var compressBuffer bytes.Buffer
	writer, writerErr := zlib.NewWriterLevel(&compressBuffer, insertCompressLevel)
	if writerErr != nil {
		return nil, writerErr
	}
	if _, writeErr := writer.Write(payload); writeErr != nil {
		_ = writer.Close()
		return nil, writeErr
	}
	if closeErr := writer.Close(); closeErr != nil {
		return nil, closeErr
	}
	return compressBuffer.Bytes(), nil
}

// toNumber 把值转成 float64，仅用于数值校验与除零判断；真正入库的参数仍然用原值，避免大整数被浮点截断。
func toNumber(rawValue any) (float64, error) {
	switch typed := rawValue.(type) {
	case uint64:
		return float64(typed), nil
	case uint32:
		return float64(typed), nil
	case uint:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case int32:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case float64:
		return typed, nil
	case float32:
		return float64(typed), nil
	case string:
		numberValue, parseErr := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if parseErr != nil {
			return 0, fmt.Errorf("值 %q 不是数值", typed)
		}
		return numberValue, nil
	}
	return 0, fmt.Errorf("值类型 %T 不是数值", rawValue)
}

// toTextValue 把值转成字符串，供 LIKE / 字符串拼接使用。
// 切片、数组、map、结构体等复合类型直接拒绝，避免拼出 "map[k:v]" 这类无意义文本。
func toTextValue(rawValue any) (string, error) {
	switch typed := rawValue.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	}

	switch reflect.TypeOf(rawValue).Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.Struct, reflect.Ptr:
		return "", fmt.Errorf("值类型 %T 不能作为文本使用", rawValue)
	}
	return fmt.Sprintf("%v", rawValue), nil
}

// toSliceValues 把任意切片 / 数组转成 []any，供 IN / BETWEEN / MOD 展开占位符使用。
func toSliceValues(rawValue any) ([]any, error) {
	if rawValue == nil {
		return nil, fmt.Errorf("值不能为空")
	}
	reflectValue := reflect.ValueOf(rawValue)
	if reflectValue.Kind() != reflect.Slice && reflectValue.Kind() != reflect.Array {
		return nil, fmt.Errorf("值类型 %T 不是数组/切片", rawValue)
	}

	values := make([]any, 0, reflectValue.Len())
	for index := 0; index < reflectValue.Len(); index++ {
		values = append(values, reflectValue.Index(index).Interface())
	}
	return values, nil
}

// toUnsignedInt 把值转成非负整数，供位标记（含 / 不含 / 置位 / 翻转 / 清位）使用。
// 数组按元素求和（一个字段要同时命中多个位时，把位值相加传入即可）。
func toUnsignedInt(rawValue any) (uint64, error) {
	if rawValue == nil {
		return 0, fmt.Errorf("值不能为空")
	}

	reflectValue := reflect.ValueOf(rawValue)
	if reflectValue.Kind() == reflect.Slice || reflectValue.Kind() == reflect.Array {
		var total uint64
		for index := 0; index < reflectValue.Len(); index++ {
			itemValue, itemErr := toUnsignedInt(reflectValue.Index(index).Interface())
			if itemErr != nil {
				return 0, itemErr
			}
			total += itemValue
		}
		return total, nil
	}

	switch typed := rawValue.(type) {
	case uint64:
		return typed, nil
	case uint32:
		return uint64(typed), nil
	case uint:
		return uint64(typed), nil
	case int64:
		if typed < 0 {
			return 0, fmt.Errorf("位标记值必须为非负数，实际 %d", typed)
		}
		return uint64(typed), nil
	case int32:
		if typed < 0 {
			return 0, fmt.Errorf("位标记值必须为非负数，实际 %d", typed)
		}
		return uint64(typed), nil
	case int:
		if typed < 0 {
			return 0, fmt.Errorf("位标记值必须为非负数，实际 %d", typed)
		}
		return uint64(typed), nil
	case float64:
		if typed < 0 || typed != math.Trunc(typed) {
			return 0, fmt.Errorf("位标记值必须为非负整数，实际 %v", typed)
		}
		return uint64(typed), nil
	case float32:
		return toUnsignedInt(float64(typed))
	case string:
		parsedValue, parseErr := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("位标记值 %q 不是非负整数", typed)
		}
		return parsedValue, nil
	}
	return 0, fmt.Errorf("值类型 %T 不能作为位标记", rawValue)
}
