# gdo

自用的 Go 数据库工具集：一个 MySQL 查询构建器 + 一个 SQLite 语句构建器，两者写法一致（键值对 + 键后缀表达运算符、链式调用、条件与参数严格一一对应）。

| 包 | 说明 | 直接依赖 |
|---|---|---|
| [`mysql`](mysql/) | MySQL 连接池管理、链式查询/写入构建器、多语句事务、SQL 执行统计、MySQL 中继 HTTP 接口 | `go-sql-driver/mysql`、`gin`（只有中继接口用） |
| [`sqlite`](sqlite/) | SQLite 语句构建器（含不持连接的 `Cond`），方言按 SQLite 自身的规矩来 | `modernc.org/sqlite`（纯 Go 驱动，`CGO_ENABLED=0` 也能编译） |

两个包互不依赖，可以只用其中一个。

## 安装

```bash
go get github.com/laocc/gdo
```

## mysql 快速上手

```go
import "github.com/laocc/gdo/mysql"

// 1. 启动时初始化连接池（map 的 key 是实例名，名为 default 的作默认库）
if initErr := mysql.InitAllDB(map[string]*mysql.SetupMySQL{
	"default": {Run: true, Host: "127.0.0.1", Port: 3306, User: "root", Password: "***", Database: "demo", PoolSize: 10},
}); initErr != nil {
	log.Fatal(initErr)
}
defer mysql.CloseAllDb()

// 2. 查询
rows, paging, queryErr := mysql.Table("tabUser").
	Select("userID", "userName").
	Where("userState", 1).
	Where("userName~", keyword). // LIKE ?：自动补 %，^ 前缀锚、$ 后缀锚
	OrderBy("userID DESC").
	Paging(1, 20)

// 3. 写入
mysql.Table("tabVoucher").Update(mysql.M("voNumber+", 10), mysql.M("voID", voID)) // voNumber = voNumber + ?
mysql.Table("tabLog").Insert(mysql.M("logText", "hello"))

// 4. 事务：只允许写，事务里做查询会直接返回错误
trans, beginErr := mysql.Begin()
if beginErr != nil {
	return beginErr
}
defer trans.Rollback()
if _, updateErr := trans.Table("tabOrder").Update(mysql.M("orderState", 2), mysql.M("orderID", orderID)); updateErr != nil {
	return updateErr
}
return trans.Commit()
```

`Where` 收「键值对」或 `mysql.M(...)`；原生 SQL 片段用 `WhereArg`；多组 OR 用 `WhereOr`。完整用法见 [mysql/help.md](mysql/help.md)。

### 键后缀总表

| 位置 | 后缀 | 生成 |
|---|---|---|
| `Where` | `>` `<` `>=` `<=` `!` `!=` `<>` | 比较运算 `col > ?` 等 |
| `Where` | `~` `!~` | `col LIKE ?`（按锚点自动补 `%`）/ `NOT LIKE` |
| `Where` | `&` `&?` `!&` | 位含（`col > 0 AND (col & ?) > 0`）/ 位含且字段为 0 也算 / 位不含 |
| `Where` | `*` `#` `!#` `@` `!@` `%` `!%` `$` `!$` | 正则 / BETWEEN / IN / MOD / 全文检索（需 FULLTEXT 索引） |
| `Update` | `+` `-` `*` `/` | `col = col + ?` 等 |
| `Update` | `|` `^` `!` `.` | 位置位 / 位异或 / 清位 / `CONCAT(col, ?)` |
| `Insert` | `\` `@` `#` | 原生表达式 / `ST_GeomFromText(?)` / zlib 压缩后写入 |

多列条件只支持 like 与全文检索：`Where("voTitle,voName~", keyword)` → `CONCAT(voTitle, voName) LIKE ?`。

### 可选：接入日志与 SQL 统计

库不依赖任何日志框架——SQL 日志、错误日志、后台协程、调用位置裁剪全部走钩子，不设置就用默认行为（SQL 静默、错误写标准库 `log`、后台任务用普通协程）：

```go
mysql.SetHook(mysql.Hook{
	CallSite: func(raw mysql.CallSite) mysql.CallSite { return trimSite(raw) }, // 裁成项目内相对路径
	SQL:      func(site mysql.CallSite, query string, args ...any) { mylog.SQL(site, query, args) },
	Error:    func(message string, err error) { mylog.Error(message, err) },
	Async:    func(task func()) { core.Go(task) }, // 用协程本地存储时必须换成能继承上下文的实现
})
```

SQL 执行统计（`mysql.SetSQLStatEnabled(true)` 开关 + `mysql.Snapshot()` 取增量）的调用位置同样走 `CallSite` 钩子裁剪。

## sqlite 快速上手

```go
import "github.com/laocc/gdo/sqlite"

if openErr := sqlite.Open("data/app.db"); openErr != nil { // 不存在则创建，WAL + 5 秒写锁等待
	return openErr
}
defer sqlite.Close()

rows, paging, listErr := sqlite.Table("tabUser").
	Where("userState", 1).
	Where("userName~", keyword).
	Paging(1, 20)

// 已有查询函数、只想拼 WHERE 时用 Cond（不持连接、不执行语句）
cond := sqlite.NewCond()
cond.And("userState", 1)
cond.AndOr(sqlite.M("userName~", kw), sqlite.M("userMail~", kw))
whereSQL, whereArgs, whereErr := cond.Where()
// whereSQL: " WHERE userState = ? AND ((userName LIKE ?) OR (userMail LIKE ?))"

// 写入：Insert / InsertBatch / Upsert / Update / Delete，DDL 与复杂语句用 Exec
sqlite.Table("tabUser").Upsert(sqlite.M("userID", 1, "userName", "tom"), []string{"userID"}, "userName")
```

方言按 SQLite 来：字符串拼接 `||`、取模 `%`，没有 `REGEXP` / 全文检索 / 空间函数——不支持的写法直接报错，不硬套 MySQL 语义。与 mysql 版的差异对照见 [sqlite/help.md](sqlite/help.md)。

## 文档

- [mysql/help.md](mysql/help.md)：API 一览、后缀总表、`~` 通配符规则、`WhereOr`、事务、常见错误排错表
- [sqlite/help.md](sqlite/help.md)：API 一览、后缀总表、`Cond`、`Upsert`、与 mysql 版的方言差异对照

## 约定

- 键后缀只在 `Where`（键值对）、`Update` 的 data、`Insert` 的 data 上生效；`WhereArg("col > ?", v)` 是原生片段，不解析后缀。
- `Update` / `Delete` 不带条件直接报错，要整表操作请显式写 `Where("1 = 1")`。
- 条件写错（字段名不合法、值类型不符、不支持的写法）一律返回错误，不静默拼出一条错的 SQL。
- 单位口径：时间 Unix 秒、金额分、重量克。
