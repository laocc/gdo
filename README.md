# Go Data Object

自用的 Go 数据库工具集：一个 MySQL 查询构建器 + 一个 SQLite 语句构建器，两者写法一致（键值对 + 键后缀表达运算符、链式调用、条件与参数严格一一对应）。

| 包                   | 说明                                                     | 直接依赖                                               |
|---------------------|--------------------------------------------------------|----------------------------------------------------|
| [`mysql`](mysql/)   | MySQL 连接池管理、链式查询/写入构建器、多语句事务 | `go-sql-driver/mysql`               |
| [`sqlite`](sqlite/) | SQLite 语句构建器（含不持连接的 `Cond`），方言按 SQLite 自身的规矩来          | `modernc.org/sqlite`（纯 Go 驱动，`CGO_ENABLED=0` 也能编译） |

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

`Where` 收「键值对」或 `mysql.M(...)`；原生 SQL 片段用 `WhereArg`；多组 OR 用 `WhereOr`
。完整用法见 [mysql.md](mysql.md)。

### 键后缀总表

| 位置       | 后缀                                      | 生成                                                 |
|----------|-----------------------------------------|----------------------------------------------------|
| `Where`  | `>` `<` `>=` `<=` `!` `!=` `<>`         | 比较运算 `col > ?` 等                                   |
| `Where`  | `~` `!~`                                | `col LIKE ?`（按锚点自动补 `%`）/ `NOT LIKE`               |
| `Where`  | `&` `&?` `!&`                           | 位含（`col > 0 AND (col & ?) > 0`）/ 位含且字段为 0 也算 / 位不含 |
| `Where`  | `*` `#` `!#` `@` `!@` `%` `!%` `$` `!$` | 正则 / BETWEEN / IN / MOD / 全文检索（需 FULLTEXT 索引）      |
| `Update` | `+` `-` `*` `/`                         | `col = col + ?` 等                                  |
| `Update` | `\|` `^` `!` `.`                       | 位置位 / 位异或 / 清位 / `CONCAT(col, ?)`                        |
| `Insert` | `\` `@` `#`                             | 原生表达式 / `ST_GeomFromText(?)` / zlib 压缩后写入          |

多列条件只支持 like 与全文检索：`Where("voTitle,voName~", keyword)` → `CONCAT(voTitle, voName) LIKE ?`。

### 可选：接入 SQL 回调与调用位置

库不依赖任何日志框架，也不做任何 SQL 存储，对外只有两个钩子：

| 钩子 | 作用 | 不设置时 |
|---|---|---|
| `CallSite` | 把采集到的调用位置裁成宿主习惯的形态（如项目内相对路径） | 原样使用 |
| `SQL` | 每执行一条 SQL 回调一次：`(site, query, cost, args...)` | 不输出（也不做栈回溯） |

错误日志固定走标准库 `log`，连接池后台预热固定用普通协程（panic 只记日志），都不需要宿主接管：

```go
mysql.SetHook(mysql.Hook{
	CallSite: func(raw mysql.CallSite) mysql.CallSite { return trimSite(raw) },
	SQL: func(site mysql.CallSite, query string, cost time.Duration, args ...any) {
		mylog.SQL(site, query, cost, args) // 语句 + 耗时 + 业务位置一次拿全
	},
})
```

`CallSite` 的裁剪结果对 SQL 回调与错误日志都生效；执行统计、慢查询落库这类需求由宿主在 `SQL` 回调里自己做（本库不存数据）。

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

方言按 SQLite 来：字符串拼接 `||`、取模 `%`，没有 `REGEXP` / 全文检索 / 空间函数——不支持的写法直接报错，不硬套 MySQL 语义。与
mysql 版的差异对照见 [sqlite.md](sqlite.md)。

## 文档

- [mysql.md](mysql.md)：API 一览、后缀总表、`~` 通配符规则、`WhereOr`、事务、常见错误排错表
- [sqlite.md](sqlite.md)：API 一览、后缀总表、`Cond`、`Upsert`、与 mysql 版的方言差异对照

## 约定

- 键后缀只在 `Where`（键值对）、`Update` 的 data、`Insert` 的 data 上生效；`WhereArg("col > ?", v)` 是原生片段，不解析后缀。
- `Update` / `Delete` 不带条件直接报错，要整表操作请显式写 `Where("1 = 1")`。
- 条件写错（字段名不合法、值类型不符、不支持的写法）一律返回错误，不静默拼出一条错的 SQL。
- 单位口径：时间 Unix 秒、金额分、重量克。

## License

[MIT](LICENSE)：可自由使用、修改、商用、再发布，保留版权声明即可。
