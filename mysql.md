# mysql 查询构建器（`github.com/laocc/gdo/mysql`）使用说明


```go
package example

import "github.com/laocc/gdo/mysql"

func LoadList(params ListParams) ([]map[string]any, *mysql.PagingData, error) {
	page, pageSize := params.NormalizePage()

	builder := mysql.Table("tabVoucher").
		Select("voID", "voTitle", "voNumber", "voState").
		Where("voState", 1). // voState = ?
		Where("voNumber>", 0). // voNumber > ?
		Where("voTitle,voName~", params.Keyword). // CONCAT(voTitle, voName) LIKE ?
		Where("voID@", []any{1, 2, 3}). // voID IN (?, ?, ?)
		WhereOr( // ((voAppGID&? 命中) OR (voAppGID = 0))
			mysql.M("voAppGID&?", params.AppGID),
			mysql.M("voAppGID", 0),
		).
		OrderBy("voSort DESC, voID DESC").
		Limit(100).
		Offset(0)

	return builder.Paging(page, pageSize) // (数据, 分页信息, 错误)
}
```

规则只有一条要记住：**`Where` 的键 = 列名 + 可选的操作符后缀**，同一段 SQL 可以完全不写字符串。

---

## 1. 查询 API 一览

| 方法                                             | 作用                                                           |
|------------------------------------------------|--------------------------------------------------------------|
| `mysql.Table(table)`                           | 用默认库打开一张表，得到 `*Builder`（链式调用）                                |
| `mysql.TableWithDB(db, table)`                 | 指定另一个 `*sql.DB` 实例                                           |
| `Select(fields...)`                            | 指定查询列，默认 `*`                                                 |
| `SelectSkip(fields...)`                        | 全列中剔除若干列（列名自动查 `information_schema` 并缓存），用于大表少读几列            |
| `Distinct()`                                   | 消除查询结果中的重复行（`SELECT DISTINCT`），与 `Select` / `SelectSkip` 连用          |
| `Where(args...)`                               | 键值对条件，**多个条件之间 AND**；也接受一个 `map[string]any`                  |
| `WhereOr(groups...)`                           | 多组条件，**组内 AND、组间 OR**                                        |
| `WhereIn(field, values)`                       | `field IN (?, ?, ...)`（与 `Where("field@", values)` 等价）       |
| `WhereBetween(field, start, end)`              | `field BETWEEN ? AND ?`（与 `Where("field#", []any{s, e})` 等价） |
| `WhereArg(query, args...)`                     | 直接写原生片段，片段之间也是 AND                                           |
| `OrderBy("voID DESC")`                         | 排序                                                           |
| `GroupBy(field)` / `Having(fragment, args...)` | 分组与分组后过滤（`Having` 仅在 `GroupBy` 之后生效）                         |
| `Limit(n)` / `Offset(n)`                       | 限制与偏移（`Paging` 内部会覆盖它们）                                      |
| `Decode(fields...)`                            | 结果里指定列：字符串按 JSON 解码、数值按位拆分（`7` → `[1,2,4]`）                  |
| `DecodePoint(fields...)`                       | 结果里指定 POINT 列按 `ST_AsText` 文本解析成 `{lng, lat}`                |
| `All()` / `All(cond)`                          | 返回 `[]map[string]any`（保证是 `[]` 而不是 `nil`）                    |
| `Get()` / `Get(cond)`                          | 返回第一行（无数据返回 `nil, nil`）                                      |
| `Paging(page, pageSize?)`                      | 返回 `([]map[string]any, *PagingData, error)`，`pageSize` 默认 20 |
| `Count()`                                      | `COUNT(*)`                                                   |
| `Value(field)`                                 | 某个字段的单值                                                      |
| `Pluck(field)`                                 | 某列的所有值 `[]any`                                               |

`PagingData` 的 JSON 字段：`recode`（总记录数）、`total`（总页数）、`size`（每页条数）、`current`（当前页）。

**`All` / `Get` / `Update` / `Delete` 的条件参数写法与 `Where` 完全一致**，两种都可：

```go
mysql.Table("tabActivities").Get("actID", request.ID)             // 键值对，最省事
mysql.Table("tabActivities").Get(mysql.M("actID", request.ID))    // mysql.M 的 map
mysql.Table("tabActivities").Get()                               // 无条件（查询允许；Update / Delete 会直接报错）
mysql.Table("tabVoucher").Update(data, "voID", voID)              // Update 的条件同理
mysql.Table("tabRefund").Delete("refID", refundID)                // Delete 的条件同理
```

---

## 2. 条件构建

### 2.1 `Where`：键值对 + 后缀

```go
// 两种写法等价，前者更省事
builder.Where("voID", 1)
builder.Where(mysql.M("voID", 1))

// 一次写多个条件（内部是 map，条件之间 AND；同一条 SQL 里顺序不保证）
builder.Where("voState", 1, "voNumber>", 0)
builder.Where(mysql.M("voState", 1, "voNumber>", 0))

// 跨调用链式 AND
builder.Where("voState", 1).Where("voNumber>", 0)
// => WHERE voState = ? AND voNumber > ?
```

`Where` 的参数不合法会**报错而不是静默丢条件**：键值不成对、键不是字符串、传了非 `map[string]any` 的 map，都会被记下来，在
`All / Get / Update / Delete` 执行前返回错误。

### 2.2 WHERE 操作符总表

| 键后缀               | 含义                 | 生成 SQL                                                    | 值要求                                       |
|-------------------|--------------------|-----------------------------------------------------------|-------------------------------------------|
| 无 / `=`           | 等于                 | `col = ?`                                                 | 任意                                        |
| `>` `<` `>=` `<=` | 比较                 | `col >= ?`                                                | 任意                                        |
| `!` / `!=` / `<>` | 不等于                | `col != ?`                                                | 任意                                        |
| `~` / `!~`        | 模糊 / 不模糊           | `col LIKE ?` / `col NOT LIKE ?`                           | 字符串（见 2.4 通配符规则）                          |
| `$` / `!$`        | 全文检索               | `MATCH(col) AGAINST (?)` / `NOT (MATCH(col) AGAINST (?))` | 字符串或 `[关键词, 匹配度]`，列上需 FULLTEXT 索引         |
| `&`               | 位含                 | `col > 0 AND (col & ?) > 0`                               | 非负整数（数组按求和）                               |
| `&?`              | 位含（`col = 0` 也算命中） | `(col = 0 OR (col & ?) > 0)`                              | 同上                                        |
| `!&`              | 位不含                | `(col = 0 OR (col & ?) = 0)`                              | 同上                                        |
| `*`               | 正则                 | `col REGEXP ?`                                            | 字符串                                       |
| `#` / `!#`        | 区间                 | `col BETWEEN ? AND ?` / `NOT BETWEEN`                     | `[]any{起, 止}`                             |
| `@` / `!@`        | 集合                 | `col IN (?, ?, …)` / `NOT IN`                             | 任意切片；**空切片**：`@` → `1 = 0`、`!@` → `1 = 1` |
| `%` / `!%`        | 取模                 | `MOD(col, ?) = ?` / `!= ?`                                | `[]any{除数, 余数}`，除数不能为 0                   |
| `LIKE`（词形）        | 原样传入               | `col LIKE ?`                                              | 字符串，**不自动补 %**（兼容旧写法，建议用 `~`）             |

位标记值为 0 时的退化行为：`&` → `1 = 0`（恒不命中）、`!&` → `1 = 1`（恒命中）、`&?` → `col = 0`。

```go
builder.Where("voState", 1) // voState = ?
builder.Where("voNumber>", 0) // voNumber > ?
builder.Where("voTitle~", "优惠") // voTitle LIKE ?
builder.Where("voAppGID&?", params.AppGID) // (voAppGID = 0 OR (voAppGID & ?) > 0)
builder.Where("voMask!&", 8) // (voMask = 0 OR (voMask & ?) = 0)
builder.Where("addTime#", []any{startTime, endTime}) // addTime BETWEEN ? AND ?
builder.Where("voID@", []any{1, 2, 3}) // voID IN (?, ?, ?)
builder.Where("voNumber%", []any{2, 1}) // MOD(voNumber, ?) = ?
builder.Where("voKey*", "^VIP") // voKey REGEXP ?
```

> **位标记列的取值口径**：`&` 是严格位含（要求 `col > 0` 且按位命中）；渠道 / 应用这类「字段为 0 表示不限，对所有都适用」的过滤必须用
`&?`。

### 2.3 多列：逗号与加号等价（拼接后比对）

只有 like（`~` / `!~`）与全文检索（`$` / `!$`）支持多列，分隔符只有 `,` 和 `+`（两者含义完全相同，都表示「把这几列拼起来再比对」）：

```go
builder.Where("voTitle,voName~", keyword) // CONCAT(voTitle, voName) LIKE ?
builder.Where("voTitle+voName~", keyword) // 同上（等价写法）
builder.Where("voTitle,voName!~", keyword) // CONCAT(voTitle, voName) NOT LIKE ?
builder.Where("voTitle,voName$", keyword) // MATCH(voTitle, voName) AGAINST (?)
builder.Where("voTitle,voName$", []any{keyword, 0.5}) // MATCH(voTitle, voName) AGAINST (?) > ?
```

* 拼接后匹配是「任一列命中」的**超集**：任一列含关键字，拼接串必然也含（另外还能命中跨列边界，如 `voTitle="电话"`、
  `voName="123"` 搜 `话1`）。
* **多列 + 其它后缀（如 `"a,b>"`、`"a,b@"`）会直接报错**，不猜语义；需要「任一列命中」的精确 OR 语义时用 `WhereArg` 或
  `WhereOr`。
* 全文检索要求列上有 FULLTEXT 索引，否则 MySQL 报 1191；**中文必须用 ngram 解析器**：

```sql
ALTER TABLE tabArticle ADD FULLTEXT INDEX ft_art_search (artTitle, artContent) WITH PARSER ngram;
```

### 2.4 `~` 的通配符规则

| 传入值     | 生成的值  | 说明                      |
|---------|-------|-------------------------|
| `"张"`   | `%张%` | 没带 `%`，前后各补一个（包含匹配）     |
| `"%张%"` | `%张%` | 已经带了 `%`，原样使用           |
| `"%张"`  | `%张`  | 只要带了一个 `%`（开头或结尾）就完全不补  |
| `"张%"`  | `张%`  | 同上                      |
| `"^张"`  | `张%`  | `^` 前缀锚点：以前缀匹配（不补前 `%`） |
| `"张$"`  | `%张`  | `$` 后缀锚点：以后缀匹配（不补后 `%`） |
| `"^张$"` | `张`   | 精确匹配                    |

所以要「只按前缀搜」用 `Where("voTitle~", "^"+kw)`；要自己控制通配符时直接传 `%`（带了 `%` 就不会再补）。

### 2.5 `WhereOr`：多组 OR

```go
// ((key1 = ? AND key2 = ?) OR (key1 = ? AND key3 = ?))
builder.WhereOr(
mysql.M("key1", 3, "key2", 23),
mysql.M("key1", 5, "key3", 23),
)

// AND ((key6 = ?) OR (key88 = ?))
builder.WhereOr(mysql.M("key6", 23), mysql.M("key88", 23))
```

* 每组一个 `mysql.M(...)`：**组内 AND、组间 OR**；与 `Where` / `WhereArg` 连用时整体 AND。
* 组内条件同样支持全部后缀与多列，例如 `WhereOr(mysql.M("voID@", ids), mysql.M("voTitle,voName~", kw))`。
* **空组报错**（`WhereOr(mysql.M())` → `WhereOr 的条件组不能为空`），组内条件解析失败也报错并整体不生效——避免静默丢掉一整组条件。
* 无参 `WhereOr()` 不加条件、不报错。
* 只支持一层 OR（组里不再套组）；更深的嵌套用 `WhereArg` 手写。

### 2.6 `WhereArg`：原生 SQL 片段

```go
builder.WhereArg("voTitle = ?", title)
builder.WhereArg("(refNumber LIKE ? OR refTransactionID LIKE ?)", kw, kw)
builder.WhereArg("(voCompGID & ?) > 0 OR voCompGID = 0", compGID)
builder.WhereArg("((a = ? AND b = ?) OR (a = ? AND c = ?))", 3, 23, 5, 23)
```

片段不做任何解析：**占位符个数必须与 args 一致**，列名与拼接由调用方保证。它和其它条件之间都是 AND。

### 2.7 条件写错了会怎样

构建器不在 `Where` 当场返回错误，而是把错误记在 `Builder` 上（`whereParseErr`），在 `All` / `Get` / `Update` / `Delete`真正执行前原样返回，例如：

```
where 条件字段名不合法: "voNumber+"
where 多列（逗号/加号分隔）只支持 like 与全文检索后缀 ~ !~ $ !$，当前后缀 ">" : "a,b>"
where in(voID@) 值不合法: 值类型 int 不是数组/切片
where mod(voID%) 除数不能为 0
Where 参数必须成对出现（键、值），实际 1 个；原生 SQL 片段请用 WhereArg
WhereOr 的条件组不能为空
```

**绝不静默忽略**：条件一旦被丢掉，查询会捞全表，更新/删除会落到整张表上。

---

## 3. 查询

```go
// 列表 + 分页
rows, paging, pageErr := mysql.Table("tabVoucher").
  SelectSkip("voSetting"). // 全列里剔除 voSetting
  Where("voState", 1).
  Where("voTitle,voName~", keyword).
  OrderBy("voSort DESC, voID DESC").
  Paging(page, pageSize) // pageSize 省略时默认 20
  // paging.Recode / paging.Total / paging.Size / paging.Current

// 单行
row, queryErr := mysql.Table("tabUser").Get(mysql.M("userID", userID))
if row == nil { /* 不存在 */ }

// 单值 / 单列 / 计数
state, valueErr := mysql.Table("tabVoucher").Where("voID", voID).Value("voState")
ids, pluckErr := mysql.Table("tabApp").Where("appID>", 0).Pluck("appID")
total, countErr := mysql.Table("tabCoupon").Where("copUserID", userID).Count()

// JSON 列与位标记列解码、POINT 列解析
rows, decodeErr := mysql.Table("tabExpress").
Select("*", "ST_AsText(expPoint) AS expPoint").
Where("expID", expID).
Decode("expSender", "expReceiver", "expMemo", "expType").
DecodePoint("expPoint").
All()
// expSender/expReceiver/expMemo 变成对象；expType 数值按位拆成 [1,2,4]；expPoint 变成 {lng, lat}
```

`GroupBy` + `Having`：

```go
rows, groupErr := mysql.Table("tabCoupon AS cop JOIN tabVoucher AS voc ON voc.voID = cop.copVoucherID").
Select("cop.copVoucherID AS voucherID", "COUNT(*) AS allRec").
Where("cop.copUserID", userID).
WhereIn("cop.copVoucherID", voucherIDs).
GroupBy("cop.copVoucherID").
Having("allRec > ?", 1).
All()
```

`All` / `Get` 的条件参数与 `Where` 写法一致（等价于先 `Where`）：`All("compID>", 0)`、`Get("userID", userID)`、`Get(mysql.M("userID", userID))` 都行。

---

## 4. 写入

### 4.1 `Insert` / `InsertBatch`

```go
// 普通插入，返回自增 ID
voID, insertErr := mysql.Table("tabVoucher").Insert(mysql.M(
"voTitle", "新人券",
"voNumber", 1000,
"voState", 1,
"voAppGID",  uint64(1)<<(appID-1),
"voTime", time.Now().Unix(),
))

// 键后缀：\ 原生表达式、@ 空间点、# zlib 压缩后写入
_, insertErr = mysql.Table("tabBlacklist").Insert(mysql.M(
"btMobile", "13800000000",
"btPoint@", "POINT(116.39 39.9)", // ST_GeomFromText(?)
"btTime\\", "now()",              // 原样拼入 SQL（内容安全由调用方保证）
"btData#", longText,              // zlib 压缩后写二进制列
))

// 批量插入（字段列表取自第一行，各行键名必须一致）
affected, batchErr := mysql.Table("tabCount").InsertBatch([]map[string]any{
mysql.M("countAppID", 1, "countDay", 20261001, "countNum", 10),
mysql.M("countAppID", 1, "countDay", 20261002, "countNum", 20),
})
```

注意：`Insert` / `InsertBatch` / `Update` **不会自动序列化**，结构体、指针、slice 直接当参数会在驱动层报 `unsupported type`
；JSON 列请先 `json.Marshal` 成字符串。

### 4.2 `Update`

```go
// 直接赋值（第二个参数是条件，必须有）
affected, updateErr := mysql.Table("tabVoucher").
Update(mysql.M("voTitle", "双十一券", "voState", 1), mysql.M("voID", voID))
// => UPDATE tabVoucher SET voState = ?, voTitle = ? WHERE voID = ?
```

SET 的键后缀（字段自运算）：

| 键后缀             | 生成 SQL                      | 值要求            |
|-----------------|-----------------------------|----------------|
| 无               | `col = ?`                   | 任意             |
| `+` `-` `*` `/` | `col = col + ?`             | 数值（`/` 值不能为 0） |
| `\|`            | `col = col \| ?`（置位）        | 非负整数           |
| `^`             | `col = col ^ ?`（翻转位）        | 非负整数           |
| `!`             | `col = col - (col & ?)`（清位） | 非负整数           |
| `.`             | `col = CONCAT(col, ?)`      | 字符串            |

```go
// 发行数量自增：不要再手拼 mysql.Raw("voNumber + n")
_, updateErr := mysql.Table("tabVoucher").
Update(mysql.M("voNumber+", request.Number), mysql.M("voID", request.VoID))
// => UPDATE tabVoucher SET voNumber = voNumber + ? WHERE voID = ?

// 位标记置位 / 清位
mysql.Table("tabUser").Update(mysql.M("userFilter|", 8), mysql.M("userID", userID)) // 置位
mysql.Table("tabUser").Update(mysql.M("userFilter!", 8), mysql.M("userID", userID))   // 清位

// 备注追加
mysql.Table("tabExpress").Update(mysql.M("expNotes.", "已联系"), mysql.M("expID", expID))
// => UPDATE tabExpress SET expNotes = CONCAT(expNotes, ?) WHERE expID = ?
```

`mysql.Raw("表达式")` 仍可用（值位置整段拼入 SQL），且优先于后缀解析；能用后缀表达的就不必手写。

**没有条件会直接报错**（防全表更新）：

```
禁止不带条件更新（tabVoucher）：请用 Where / WhereOr 或 Update 的 conditions 指定条件；确实要整表更新请显式写 WhereArg("1 = 1")
```

### 4.3 `Delete`

```go
affected, deleteErr := mysql.Table("tabRefund").Delete(mysql.M("refID", refundID))
```

* 删除前会先 `SELECT 1 ... LIMIT 1` 探测记录是否存在，不存在直接返回 `删除失败：没有符合条件的记录`（事务里跳过这一步）。
* 同样**禁止无条件删除**，要清表必须显式 `WhereArg("1 = 1")`。

---

## 5. 事务

```go
trans, beginErr := mysql.Begin()
if beginErr != nil { return beginErr }

// 事务里只允许写：Select / All / Get / Count / Value / Pluck / Paging 一律返回错误
_, updateErr := trans.Table("tabVoucher").Update(mysql.M("voNumber+", n), mysql.M("voID", voID))
if updateErr != nil { return updateErr }   // 写语句出错时构建器已自动整笔回滚，不需要手动 Rollback

_, insertErr := trans.Table("tabVocIssue").Insert(mysql.M("visVoID", voID, "visNumber", n))
if insertErr != nil { return insertErr }

if commitErr := trans.Commit(); commitErr != nil { return commitErr }
```

* **事务里只允许写**：查询走连接池（`mysql.Table(...)`），事务构建器上的查询方法返回 `事务里不允许执行查询（SELECT）...`。
* **写语句出错自动回滚**：`exec` 是 Insert / InsertBatch / Update / Delete 的唯一出口，出错即整笔回滚；调用方只管返回错误。
* 事务里「影响行数为 0」按正常处理（不再做存在性探测），要判断记录是否存在必须在 `Begin()` 之前用 `mysql.Table(...)` 查好。
* 需要在事务里读时：先查好再开事务，或在事务外查询。

---

## 6. 其它工具

| 函数                             | 用途                                                                                    |
|--------------------------------|---------------------------------------------------------------------------------------|
| `mysql.M(pairs...)`            | 构造 `map[string]any`，`M("a", 1, "b", 2)`；键可带后缀                                         |
| `mysql.Raw(sql)`               | SET / INSERT 值位置的原生 SQL 表达式（如 `Raw("voNumber + 10")`），不参数化                            |
| `mysql.Point(gps)`             | 构造 `ST_GeomFromText('POINT(lng lat)')`（等价于键后缀 `@`）                                    |
| `mysql.TrimFieldOperator(key)` | 剥掉键后缀取纯列名，白名单过滤用（`FilterPriceFields` / `FilterExpressFields` / `BuildOnlyParams` 已内置） |
| `mysql.ParsePointText(text)`   | 把 `ST_AsText` 的 `POINT(lng lat)` 文本解析成 `{lng, lat}`                                   |

**白名单过滤必须带后缀剥离**，否则 `"priceValue+"` 这种键会被当成未知列丢掉：

```go
for fieldKey, fieldValue := range params {
    if PriceField[mysql.TrimFieldOperator(fieldKey)] {   // 用剥后缀后的列名比对
        filtered[fieldKey] = fieldValue // 键保留后缀，交给 Update 解析
    }
}
```

所有经构建器执行的语句（Builder 的增删改查、包级 `Query`/`Exec`、表结构探测）都会回调 `Hook.SQL`：
回调参数是「调用位置 + 语句 + 耗时 + 参数」，宿主拿它自己写日志、做统计或慢查询监控，**本库不做任何 SQL 存储**。
`Skip`/`GetPool` 这类拿裸连接执行的语句不经过构建器，自然也不会回调。

---

## 7. 常见错误与排查

| 现象 / 报错                                | 原因与处理                                                                                    |
|----------------------------------------|------------------------------------------------------------------------------------------|
| `where 条件字段名不合法: "xxx"`                | 把原生 SQL 片段写进了 `Where`（如 `Where("voID = ?", kv)`）→ 改用 `WhereArg`；或列名写了不支持的符号              |
| `where 多列（逗号/加号分隔）只支持 like 与全文检索后缀...` | 多列只配 `~ !~ $ !$`，其它后缀请用 `WhereOr` / `WhereArg`                                           |
| `禁止不带条件更新/删除（表名）`                      | `Update` / `Delete` 必须带条件；确实要整表操作显式写 `WhereArg("1 = 1")`                                 |
| 查询结果比预期多                               | 检查条件是否被写成了「恒真」：`WhereArg("col")`（MySQL 里 `WHERE col` 是非 0 即真）、`Where("col", v)` 误写成原生片段等 |
| `MATCH ... AGAINST` 报 1191             | 列上没有 FULLTEXT 索引；中文还需 `WITH PARSER ngram`                                                |
| `Unsupported type ...`                 | `Insert` / `Update` 收到结构体、指针、slice → 先转成字符串 / 数字，JSON 列先 `json.Marshal`                  |
| 事务里查询报 `事务里不允许执行查询（SELECT）`            | 事务只写；查询请用 `mysql.Table(...)`（连接池）                                                        |
| 前端传字符串数字导致条件不匹配                        | 用 `common.FlexibleUint64` / `common.FlexibleFloat64` 接参；数值字段的 `binding:"required"` 拦不住 0 |

---

## 8. 变更记录（2026-10-01）

* `WhereMap` → **`Where`**（键值对 + 后缀解析，主用）；原 `Where` → **`WhereArg`**（原生片段），全仓调用已迁移。
* 新增键后缀语法：WHERE 的 `> < >= <= ! != <> ~ !~ & &? !& * # !# @ !@ % !%` 与 `$ !$`（全文检索）；UPDATE SET 的
  `+ - * / | ^ ! .`；INSERT 的 `\ @ #`。
* 新增多列（`key1,key2` / `key1+key2`，等价）= `CONCAT(a, b)` 拼接后比对，仅限 like 与全文检索。
* 新增 `WhereOr(groups...)`：多组条件组内 AND、组间 OR。
* `~` 的通配符规则：值只要带了一个 `%`（开头或结尾）就原样使用，不再补；没带则按 `^` / `$` 锚点补。
* `Update` / `Delete` 禁止不带条件执行（返回明确错误，不再生成全表语句）。
* `Where` / `WhereOr` 的参数不合法时记录错误并在执行前返回，绝不静默丢条件。
* 新增 `Distinct()`：消除查询结果重复行（`SELECT DISTINCT`），可配合 `Select` / `SelectSkip` 去重。
