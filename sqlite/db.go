// Package sqlite 是一套独立的 SQLite 语句组装库（SQL builder）。
//
// 设计定位：
//   - 不依赖 MySQL 构建器（github.com/laocc/gdo/mysql）的任何代码，可被任意 Go 项目直接引用（只依赖标准库与现代化驱动 modernc.org/sqlite）；
//   - 组装思路与 MySQL 构建器一致：键值对 + 键后缀表达运算符，链式调用，条件与参数严格一一对应；
//   - 方言按 SQLite 自身的规矩来（`||` 拼接、`%` 取模、无 REGEXP / 无 MATCH AGAINST / 无空间函数），
//     不支持的写法直接报错，不硬套 MySQL 的语义。
//
// 典型用法：
//
//	if openErr := sqlite.Open("data/app.db"); openErr != nil { ... }
//	defer sqlite.Close()
//
//	rows, paging, listErr := sqlite.Table("tabUser").
//		Where("userState", 1).
//		Where("userName~", keyword).
//		OrderBy("userID DESC").
//		Paging(1, 20)
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // 纯 Go 驱动，CGO_ENABLED=0 也能编译
)

// DB 默认连接：Open / OpenMemory 之后由本包【表】使用，Table(...) 都走它。
// 多库场景（分文件、分桶）请用 TableWithDB 显式指定连接，不要去改动这个变量。
var DB *sql.DB

// defaultLock 保护 DB 指针的读写（Open 与 Table 可能来自不同协程）
var defaultLock sync.RWMutex

// errNoDB 默认连接未打开
var errNoDB = errors.New("sqlite 默认连接未打开，请先调用 sqlite.Open(path) 或 sqlite.OpenMemory()")

// Open 打开（不存在则创建）一个 SQLite 文件并设为默认连接，已打开时先关掉旧的。
// 建好的连接固定单连接串行（SQLite 单文件写并发有限，多连接反而容易 SQLITE_BUSY）。
// 调用方：程序启动时；多库场景请改用 TableWithDB，不要反复 Open。
// 返回：错误（文件不可写、DSN 非法等）。
func Open(path string) error {
	db, openErr := openWithDSN(BuildDSN(path, false))
	if openErr != nil {
		return openErr
	}
	setDefault(db)
	return nil
}

// OpenMemory 打开一个内存库并设为默认连接（进程退出即消失，多用于测试与临时计算）。
// 返回：错误（驱动不支持或 DSN 非法）。
func OpenMemory() error {
	// 内存库不支持 WAL，单独拼一遍 DSN
	dsn := "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, openErr := openWithDSN(dsn)
	if openErr != nil {
		return openErr
	}
	setDefault(db)
	return nil
}

// BuildDSN 拼 SQLite 的 DSN：WAL + 5 秒写锁等待 + 打开外键约束 + NORMAL 同步级别。
// 调用方：Open 内部；需要自定义读写模式时可自行拼 DSN 后 sql.Open("sqlite", dsn)。
// 返回：形如 file:data/app.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)...
func BuildDSN(path string, readOnly bool) string {
	dsn := "file:" + filepath.ToSlash(path) + "?"
	if readOnly {
		dsn += "mode=ro&"
	}
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	return dsn
}

// Close 关闭默认连接（幂等）；关闭后 Table(...) 会返回“默认连接未打开”。
// 调用方：程序退出时。
// 返回：错误（底层 Close 的返回值）。
func Close() error {
	defaultLock.Lock()
	closingDB := DB
	DB = nil
	defaultLock.Unlock()
	if closingDB == nil {
		return nil
	}
	return closingDB.Close()
}

// Table 用默认连接打开一张表，得到链式构建器。
// 调用方：所有单库的增删改查。
// 返回：*Builder；默认连接未打开时，执行阶段会返回明确错误（构建阶段不报错，方便链式书写）。
func Table(table string) *Builder {
	defaultLock.RLock()
	defaultDB := DB
	defaultLock.RUnlock()
	return &Builder{
		db:    defaultDB,
		table: table,
	}
}

// TableWithDB 用指定连接打开一张表（多库、分文件、分桶场景）。
// 调用方：需要跨多个 SQLite 文件的系统。
// 返回：*Builder。
func TableWithDB(db *sql.DB, table string) *Builder {
	return &Builder{
		db:    db,
		table: table,
	}
}

// OpenAt 打开指定文件并直接用其建表构建器（不改默认连接）。
// 调用方：多库场景里临时打开某个文件查一次。
// 返回：连接与错误；用完请自行 db.Close()。
func OpenAt(path string, readOnly bool) (*sql.DB, error) {
	return openWithDSN(BuildDSN(path, readOnly))
}

// openWithDSN 按 DSN 建连接并做基础检查
func openWithDSN(dsn string) (*sql.DB, error) {
	db, openErr := sql.Open("sqlite", dsn)
	if openErr != nil {
		return nil, fmt.Errorf("打开 sqlite 失败 [%s]: %w", dsn, openErr)
	}

	// 单连接串行：SQLite 的写是文件级串行，连接多了只会互相等锁
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if pingErr := db.Ping(); pingErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 sqlite 失败 [%s]: %w", dsn, pingErr)
	}
	return db, nil
}

// setDefault 替换默认连接，并关闭被替换掉的旧连接
func setDefault(db *sql.DB) {
	defaultLock.Lock()
	previousDB := DB
	DB = db
	defaultLock.Unlock()

	if previousDB != nil && previousDB != db {
		_ = previousDB.Close()
	}
}

// currentDB 取默认连接（执行阶段调用，未打开时返回统一错误）
func currentDB() (*sql.DB, error) {
	defaultLock.RLock()
	defaultDB := DB
	defaultLock.RUnlock()
	if defaultDB == nil {
		return nil, errNoDB
	}
	return defaultDB, nil
}
