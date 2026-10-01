package sqlite

import (
	"database/sql"
	"errors"
	"log"
)

// DbTrans 事务句柄。
//
// 与 MySQL（连接池 + 只读走池）不同，SQLite 是单文件、同连接内读写都在一个事务里，
// 所以本库的事务【同时允许读和写】：trans.Table("t").Get(...) / All(...) 都能用，
// 且看到的是事务内的未提交数据（读己所写）。
type DbTrans struct {
	tx *sql.Tx
}

// Begin 在默认连接上开启事务。
// 调用方：需要多步写操作原子完成的场景。
// 返回：事务句柄与错误（默认连接未打开时返回明确错误）。
func Begin() (*DbTrans, error) {
	defaultDB, dbErr := currentDB()
	if dbErr != nil {
		return nil, dbErr
	}
	tx, beginErr := defaultDB.Begin()
	if beginErr != nil {
		return nil, beginErr
	}
	return &DbTrans{tx: tx}, nil
}

// BeginOn 在指定连接上开启事务（多库场景）。
// 返回：事务句柄与错误。
func BeginOn(db *sql.DB) (*DbTrans, error) {
	if db == nil {
		return nil, errNoDB
	}
	tx, beginErr := db.Begin()
	if beginErr != nil {
		return nil, beginErr
	}
	return &DbTrans{tx: tx}, nil
}

// Commit 提交事务。
// 返回：错误（事务已结束、磁盘满等）。
func (trans *DbTrans) Commit() error {
	return trans.tx.Commit()
}

// Rollback 回滚事务；写语句出错时构建器已经自动回滚过，这里再调只会拿到 sql.ErrTxDone。
// 返回：错误。
func (trans *DbTrans) Rollback() error {
	return trans.tx.Rollback()
}

// Table 在事务上打开一张表，得到链式构建器（读写都在本事务内）。
// 调用方：事务内的每一步写操作、以及需要读己所写的查询。
// 返回：*Builder。
func (trans *DbTrans) Table(table string) *Builder {
	return &Builder{
		trx:   trans.tx,
		table: table,
	}
}

// rollbackOnErr 事务里出错的统一收口：先回滚整笔事务，再把原错误交回调用方。
// 非事务构建器与 err 为 nil 时原样返回；回滚失败只记日志，不覆盖调用方手上的错误。
func (builder *Builder) rollbackOnErr(returnErr error) error {
	if builder.trx == nil || returnErr == nil {
		return returnErr
	}
	if rollbackErr := builder.trx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		log.Printf("[Sqlite] 事务回滚失败: %v", rollbackErr)
	}
	return returnErr
}
