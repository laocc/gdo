package mysql

import (
	"database/sql"
	"fmt"
	"time"
)

// getTableColumns 查询表的全部列名（按 ORDINAL_POSITION 排序），首次查询后缓存
func getTableColumns(db *sql.DB, table string) ([]string, error) {
	if cached, ok := tableColumnsCache.Load(table); ok {
		return cached.([]string), nil
	}

	query := `SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`
	queryBegin := time.Now()
	rows, queryErr := db.Query(query, table)
	RecordSQL(time.Since(queryBegin), query, table)
	if queryErr != nil {
		return nil, fmt.Errorf("查询表结构失败 [%s]: %w", table, queryErr)
	}
	defer func(rows *sql.Rows) {
		_ = rows.Close()
	}(rows)

	columns := make([]string, 0, 16)
	for rows.Next() {
		var columnName string
		if scanErr := rows.Scan(&columnName); scanErr != nil {
			return nil, fmt.Errorf("读取表结构列名失败 [%s]: %w", table, scanErr)
		}
		columns = append(columns, columnName)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("遍历表结构结果失败 [%s]: %w", table, rowsErr)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("表不存在或无列可查: %s", table)
	}

	tableColumnsCache.Store(table, columns)
	return columns, nil
}
