# sqlite —— 独立可复用的 SQLite 语句组装库

`github.com/laocc/gdo/sqlite` 是一套**与 MySQL 构建器（`github.com/laocc/gdo/mysql`）无任何代码关系**的 SQLite 语句组装库（借鉴它的写法：键值对 + 键后缀表达运算符、链式调用、条件与参数严格一一对应）。

* 只依赖标准库 + `modernc.org/sqlite`（纯 Go 驱动，`CGO_ENABLED=0` 也能编译）→ 可被任意 Go 项目直接引用。
* 方言按 SQLite 自身的规矩来，没有的能力**直接报错**，不硬套 MySQL 语义（差异见第 8 节）。
* 包名 `sqlite`：项目里若另有同名的 `package sqlite`（例如 SQLite 日志层），同时引用时给这边加个别名即可：`sqlkit "github.com/laocc/gdo/sqlite"`。

## 一分钟上手

```go
import "github.com/laocc/gdo/sqlite"

func Demo() error {
	if openErr := sqlite.Open("data/app.db"); openErr != nil {   // 不存在则创建
		return openErr
	}
	defer sqlite.Close()

	// 建表（构建器覆盖不到的原生语句用 Exec）
	if _, ddlErr := sqlite.Table("tabUser").Exec(`
		CREATE TABLE IF NOT EXISTS tabUser (
			userID     INTEGER PRIMARY KEY AUTOINCREMENT,
			userName   TEXT,
			userState  INTEGER DEFAULT 0,
			userFlag   INTEGER DEFAULT 0,
			createTime INTEGER DEFAULT 0
		)`); ddlErr != nil {
		return ddlErr
	}

	// 增
	userID, insertErr := sqlite.Table("tabUser").Insert(sqlite.M(
		"userName",  "张三",
		"userState", 1,
		"userFlag",  8,
		"createTime\\", "unixepoch()",   // 反斜杠后缀 = 原生表达式
	))

	// 查
	rows, paging, listErr := sqlite.Table("tabUser").
		Select("userID", "userName", "userState").
		Where("userState", 1).                    // userState = ?
		Where("userName~", "张").                 // userName LIKE ?
		Where("createTime#", []any{start, end}).  // createTime BETWEEN ? AND ?
		OrderBy("userID DESC").
		Paging(1, 20)                             // (数据, 分页信息, 错误)

	// 改（第二个参数是条件，必须有；没有条件会报错）
	_, updateErr := sqlite.Table("tabUser").
		Update(sqlite.M("userFlag|", 16), "userID", userID)   // userFlag = userFlag | ?

	// 删（同样必须有条件）
	_, deleteErr := sqlite.Table("tabUser").Delete("userID", userID)
	_ = paging
	return errors.Join(insertErr, listErr, updateErr, deleteErr)
}
```

## 1. 连接

| 函数 | 说明 |
|---|---|
| `Open(path)` | 打开（不存在则创建）文件并设为**默认连接**，`Table(...)` 都用它；已打开会先关旧连接 |
| `OpenMemory()` | 内存库（进程退出即消失，测试/临时计算用） |
| `OpenAt(path, readOnly)` | 只打开连接、不改默认连接（多库临时查询），用完自行 `Close` |
| `Close()` | 关闭默认连接（幂等） |
| `BuildDSN(path, readOnly)` | 拼 DSN，默认带 `journal_mode(WAL)` + `busy_timeout(5000)` + `foreign_keys(1)` + `synchronous(NORMAL)` |
| `Table(name)` / `TableWithDB(db, name)` | 用默认连接 / 指定连接建构建器（多文件、分桶场景用后者） |

连接固定 `SetMaxOpenConns(1)`（SQLite 写是文件级串行，连接多了只会互相等锁）；需要自定义时可在 Open 后自己调 `sqlite.DB.SetMaxOpenConns(...)`，或自行 `sql.Open("sqlite", dsn)` 配 `TableWithDB`。

## 2. 条件

### 2.1 三种写法

```go
builder.Where("userState", 1, "userFlag>", 0)          // 键值对，多个条件之间 AND
builder.Where(sqlite.M("userState", 1))                // sqlite.M 的 map
builder.WhereArg("(userName || userNick) LIKE ?", kw)  // 原生片段（也是 AND 连接）
builder.WhereOr(sqlite.M("userID", 1), sqlite.M("userID", 2))  // 多组：组内 AND、组间 OR
```

`All` / `Get` / `Update` / `Delete` 的 conditions 参数写法与 `Where` 完全一致：

```go
sqlite.Table("tabUser").Get("userID", 1)
sqlite.Table("tabUser").Get(sqlite.M("userID", 1))
sqlite.Table("tabUser").All()                       // 无条件查询是允许的
sqlite.Table("tabUser").Update(data, "userID", 1)
sqlite.Table("tabUser").Delete("userID", 1)
```

### 2.2 WHERE 操作符后缀总表

| 后缀 | 含义 | 生成 SQL | 值要求 |
|---|---|---|---|
| 无 / `=` | 等于 | `col = ?` | 任意 |
| `>` `<` `>=` `<=` | 比较 | `col >= ?` | 任意 |
| `!` / `!=` / `<>` | 不等于 | `col != ?` | 任意 |
| `~` / `!~` | 模糊 / 不模糊 | `col LIKE ?` / `NOT LIKE ?` | 字符串（通配符规则见 2.4） |
| `&` | 位含 | `col > 0 AND (col & ?) > 0` | 非负整数（数组按求和） |
| `&?` | 位含（`col = 0` 也算命中） | `(col = 0 OR (col & ?) > 0)` | 同上 |
| `!&` | 位不含 | `(col = 0 OR (col & ?) = 0)` | 同上 |
| `#` / `!#` | 区间 | `col BETWEEN ? AND ?` / `NOT BETWEEN` | `[]any{起, 止}` |
| `@` / `!@` | 集合 | `col IN (?, ?, …)` / `NOT IN` | 任意切片；空切片 → `1 = 0` / `1 = 1` |
| `%` / `!%` | 取模 | `col % ? = ?` / `!= ?` | `[]any{除数, 余数}`，除数不能为 0 |
| `LIKE`（词形） | 原样传入 | `col LIKE ?` | 不自动补 `%`（兼容写法，建议用 `~`） |

位标记值为 0 时：`&` → `1 = 0`（恒不命中）、`!&` → `1 = 1`（恒命中）、`&?` → `col = 0`。

**本库不支持**（会明确报错，不是悄悄拼错 SQL）：`*`/`!*` 正则（SQLite 默认没有 REGEXP 函数）、`$`/`!$` 全文检索（需 FTS5 虚表）。

### 2.3 多列：逗号与加号等价

```go
builder.Where("userName,userNick~", kw)   // ((userName || userNick) LIKE ?)
builder.Where("userName+userNick~", kw)   // 同上（等价写法，SQLite 用 || 拼接）
builder.Where("userName,userNick!~", kw)  // ((userName || userNick) NOT LIKE ?)
```

* 只有 `~` / `!~` 支持多列；其它后缀配多列直接报错（需要「任一列命中」的精确语义时用 `WhereOr` 或 `WhereArg`）。
* 拼接后匹配是「任一列命中」的超集（跨列边界也能命中）。

### 2.4 `~` 的通配符规则

| 传入值 | 生成的值 |
|---|---|
| `"张"` | `%张%` |
| `"%张"` / `"张%"` / `"%张%"` | 原样（只要带了一个 `%` 就完全不补） |
| `"^张"` | `张%`（前缀锚点） |
| `"张$"` | `%张`（后缀锚点） |
| `"^张$"` | `张`（精确） |

注意 SQLite 的 `LIKE` 对 ASCII **不区分大小写**，且 `%` `_` 都是通配符；要区分大小写可改用 `GLOB`（用 `WhereArg` 手写）。

### 2.5 `Cond`：不持连接的独立条件收集器

手里已经有查询函数（例如日志层的 `PagedQuery`：它接收 `countSQL`/`listSQL` 文本）时，用它拼条件：

```go
cond := sqlite.NewCond()
cond.And("userID", userID)                                    // userID = ?
cond.And("userName~", keyword)                                // userName LIKE ?
cond.AndOr(sqlite.M("title~", kw), sqlite.M("content~", kw))  // ((title LIKE ?) OR (content LIKE ?))
cond.AndRaw("(a || b) LIKE ?", "%x%")                         // 原生片段

whereSQL, whereArgs, whereErr := cond.Where()   // " WHERE userID = ? AND ..." 与参数
if whereErr != nil {
	// 条件写错只可能是代码问题：绝不能当成「没有条件」用，否则会退化成全表扫描
	whereSQL, whereArgs = " WHERE 1 = 0", nil
}
```

## 3. 查询

| 方法 | 说明 |
|---|---|
| `Select(fields...)` | 指定查询列，默认 `*`；可用 `AS` 别名（返回 map 的键就是别名） |
| `SelectSkip(fields...)` | 全列中剔除若干列（列名按 `PRAGMA table_info` 取） |
| `Distinct()` | 消除查询结果中的重复行（`SELECT DISTINCT`），与 `Select` / `SelectSkip` 连用 |
| `OrderBy` / `GroupBy` / `Having` | 排序、分组、分组后过滤（`Having` 仅在 `GroupBy` 后生效） |
| `Limit(n)` / `Offset(n)` | 限制与偏移（`Paging` 内部会覆盖它们） |
| `Decode(fields...)` | 指定列：字符串/`[]byte` 按 JSON 解码成对象；整型按位拆分成 `[1,2,4]` |
| `All(cond?)` | 返回 `[]map[string]any`（无结果返回空切片，不是 nil） |
| `Get(cond?)` | 返回第一行（无数据返回 `nil, nil`） |
| `Paging(page, pageSize?)` | 返回 `([]map[string]any, *PagingData, error)`，`pageSize` 默认 20，`page` 最小 1 |
| `Count()` / `Value(field)` / `Pluck(field)` | 计数 / 单值 / 单列 |

`PagingData` 的 JSON 字段：`recode`（总记录数）、`total`（总页数）、`size`（每页条数）、`current`（当前页）。

```go
// JSON 列与位标记列解码
rows, queryErr := sqlite.Table("tabUser").
	Select("userID", "userProfile", "userFlag").
	Decode("userProfile", "userFlag").     // profile → 对象；flag 24 → [8,16]
	All()
```

## 4. 写入

### 4.1 `Insert` / `InsertBatch`

```go
// 普通插入，返回自增 rowid（WITHOUT ROWID 表取不到，会报错，这种表请用 Upsert / Exec）
userID, insertErr := sqlite.Table("tabUser").Insert(sqlite.M("userName", "张三", "userState", 1))

// 后缀：\ 原生表达式、# zlib 压缩后写 BLOB
sqlite.M("createTime\\", "unixepoch()")
sqlite.M("content#", longText)

// 批量（字段列表取自第一行，各行键名必须一致）
affected, batchErr := sqlite.Table("tabLog").InsertBatch([]map[string]any{
	sqlite.M("logDay", 20261001, "logNum", 10),
	sqlite.M("logDay", 20261002, "logNum", 20),
})
```

值直接给结构体/指针/slice 会报 `unsupported type`；JSON 列请先 `json.Marshal` 成字符串。

### 4.2 `Upsert`（SQLite 特色，MySQL 版没有）

```go
// 冲突时覆盖
sqlite.Table("tabCount").Upsert(
	sqlite.M("countAppID", 1, "countDay", 20261001, "countNum", 10),
	[]string{"countAppID", "countDay"},   // 冲突列（必须有主键或唯一索引）
	"countNum",                            // 要更新的列
)
// => INSERT INTO tabCount (...) VALUES (...) ON CONFLICT (countAppID, countDay) DO UPDATE SET countNum = excluded.countNum

// 冲突时累加（更新列支持 + - * / 后缀）
..., "countNum+")   // 生成 countNum = countNum + excluded.countNum

// 冲突时忽略
sqlite.Table("tabCount").Upsert(data, []string{"countAppID", "countDay"})   // DO NOTHING
```

需要 SQLite 3.24+。

### 4.3 `Update`

```go
sqlite.Table("tabUser").Update(sqlite.M("userState", 0, "userName", "新名字"), "userID", id)
```

SET 的键后缀：

| 后缀 | 生成 | 说明 |
|---|---|---|
| 无 | `col = ?` | 直接赋值 |
| `+` `-` `*` `/` | `col = col + ?` | 算术（**SQLite 整数相除是整除**；`/` 值不能为 0） |
| `\|` | `col = col \| ?` | 置位 |
| `^` | `col = (col \| ?) - (col & ?)` | 翻转位（SQLite 没有异或运算符，用等价式展开） |
| `!` | `col = col - (col & ?)` | 清位 |
| `.` | `col = (col \|\| ?)` | 字符串拼接（`\|\|` 而非 CONCAT） |

`Raw("表达式")` 仍可用（值位置整段拼入 SQL），优先于后缀解析。**没有条件会直接报错**，禁止整表更新。

### 4.4 `Delete` 与 `Exec`

```go
deleted, deleteErr := sqlite.Table("tabUser").Delete("userID", id)   // 没有条件会报错
affected, execErr := sqlite.Table("tabUser").Exec("CREATE INDEX IF NOT EXISTS idx_user_name ON tabUser(userName)")
```

## 5. 事务

```go
trans, beginErr := sqlite.Begin()
if beginErr != nil {
	return beginErr
}
if _, insertErr := trans.Table("tabOrder").Insert(order); insertErr != nil {
	return insertErr   // 写语句出错时构建器已自动整笔回滚，不需要手动 Rollback
}
if _, updateErr := trans.Table("tabCount").Update(sqlite.M("countNum+", 1), "countID", countID); updateErr != nil {
	return updateErr
}
return trans.Commit()
```

* **事务里读写都允许**（SQLite 单文件、同一连接），且能读到事务内的未提交数据（读己所写）。
* 写语句出错自动回滚（`exec` 是唯一出口）；`Rollback()` 只在主动放弃时调用。
* 多库场景用 `sqlite.BeginOn(db)` 在指定连接上开事务。

## 6. 工具函数

| 函数 | 用途 |
|---|---|
| `sqlite.M(pairs...)` | 构造 `map[string]any`，键可带后缀 |
| `sqlite.Raw(sql)` | SET / INSERT 值位置的原生 SQL 表达式（不参数化） |
| `sqlite.TrimFieldOperator(key)` | 剥掉键后缀取纯列名，字段白名单过滤用 |
| `sqlite.IsNoDB(err)` | 判断错误是否为「默认连接未打开」 |

## 7. 常见错误

| 报错 | 原因与处理 |
|---|---|
| `sqlite 不支持该条件后缀（正则匹配…）` | 用了 `*`；SQLite 默认没有 REGEXP，改用 `~`（LIKE）或 `WhereArg` 手写 `GLOB` |
| `sqlite 不支持该条件后缀（全文检索…）` | 用了 `$`；全文要用 FTS5 虚表，不是普通表上的函数 |
| `sqlite 不支持该插入后缀（空间函数…）` | 用了 `@`；SQLite 没有 `ST_GeomFromText`，坐标存两列或存 WKT 文本 |
| `where 条件字段名不合法: "userID = ?"` | 把原生片段写进了 `Where` → 改用 `WhereArg` |
| `where 多列（逗号/加号分隔）只支持 like 后缀 ~ / !~` | 多列只想配 `~` / `!~`，其它后缀请用 `WhereOr` |
| `禁止不带条件更新/删除（表名）` | 必须带条件；确实要整表操作显式写 `WhereArg("1 = 1")` |
| `sqlite 默认连接未打开…` | 先 `Open(path)` / `OpenMemory()` |
| `unsupported type ...` | 值给了结构体/指针/slice，先转成字符串/数字，JSON 列先 `json.Marshal` |
| 写库报 `database is locked` | 多连接/多进程同时写同一文件；本库默认单连接串行，跨进程场景请靠 `busy_timeout` 与 WAL |

## 8. 与 MySQL 构建器的差异（有意为之）

| 能力 | 这里（SQLite） | `github.com/laocc/gdo/mysql` |
|---|---|---|
| 字符串拼接 | `(a \|\| b)` | `CONCAT(a, b)` |
| 取模 | `col % ?` | `MOD(col, ?)` |
| 位翻转 `^` | `(col \| ?) - (col & ?)` | `col ^ ?` |
| 正则 `*` / 全文 `$` | 不支持，明确报错 | 支持 `REGEXP` / `MATCH … AGAINST` |
| INSERT 的 `@`（空间点） | 不支持，明确报错 | `ST_GeomFromText(?)` |
| 事务里的读 | 允许（同连接读写） | 只允许写，查询走连接池 |
| `Upsert` | 支持 | 无 |
| 整数除法 | 整除（除非是 REAL） | `/` 返回小数 |

## 9. 约定与建议

* 金额存**分**（整数）、重量存**克**（整数）、时间存 **Unix 秒**（整数）——SQLite 的类型亲和性不强制类型，全靠约定，和大系统的口径保持一致。
* 建索引的 SQL 用 `Exec` 执行；常用索引：`(主体ID, 时间)` 复合索引（等值列在前、范围列在后）。
* `IN` 的变量数上限默认 999（3.32+ 为 32766），批次很大请分段查询。
* 大量写入建议放在事务里批量执行，比逐条 `INSERT` 快一个数量级。
