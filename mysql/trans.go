package mysql

import (
	"database/sql"
)

type DbTrans struct {
	tx *sql.Tx
}

// Begin 开启事务，返回 *DbTrans。
// 用法: tx, err := mysql.Begin()
func Begin() (*DbTrans, error) {
	tx, err := DB.Begin()
	if err != nil {
		return nil, err
	}
	dt := &DbTrans{tx: tx}
	return dt, nil
}

func (dt *DbTrans) Rollback() error {
	return dt.tx.Rollback()
}

func (dt *DbTrans) Commit() error {
	return dt.tx.Commit()
}

func (dt *DbTrans) Table(table string) *Builder {
	return &Builder{
		db:    DB,
		trx:   dt.tx,
		table: table,
	}
}
