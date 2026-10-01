package mysql

import (
	"bytes"
	"database/sql"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RegisterRelayRoutes 注册 MySQL 中继路由，按实例名分发
func RegisterRelayRoutes(routerGroup *gin.RouterGroup, mysqlConfigs map[string]*SetupMySQL) {
	relayGroup := routerGroup.Group("/mysql")
	relayGroup.Use(relayRequestLogger())    // 打印每个请求信息
	relayGroup.Use(relayAuth(mysqlConfigs)) // 鉴权
	{
		relayGroup.POST("/:name/query", mysqlQuery) // SELECT
		relayGroup.POST("/:name/exec", mysqlExec)   // INSERT/UPDATE/DELETE
	}
}

// relayRequestLogger 中继请求日志中间件：打印方法、路径、实例名、操作类型、请求体
func relayRequestLogger() gin.HandlerFunc {
	return func(ginCtx *gin.Context) {
		instanceName := ginCtx.Param("name")
		// 从 URL 最后一段推断操作类型（query 或 exec）
		action := "unknown"
		fullPath := ginCtx.FullPath()
		if fullPath != "" {
			lastSlash := 0
			for idx := len(fullPath) - 1; idx >= 0; idx-- {
				if fullPath[idx] == '/' {
					lastSlash = idx
					break
				}
			}
			action = fullPath[lastSlash+1:]
		}

		// 读取请求体
		bodyBytes, _ := io.ReadAll(ginCtx.Request.Body)
		ginCtx.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		// 格式化请求体（去掉换行便于单行显示）
		bodyStr := string(bodyBytes)
		compactBody := strings.ReplaceAll(bodyStr, "\n", "")
		compactBody = strings.ReplaceAll(compactBody, "\r", "")
		compactBody = strings.ReplaceAll(compactBody, "\t", " ")
		// 截断过长的请求体
		if len(compactBody) > 200 {
			compactBody = compactBody[:200] + "..."
		}

		log.Printf("[中继] %s %s | 实例=%s 操作=%s | Body: %s",
			ginCtx.Request.Method, ginCtx.Request.URL.Path,
			instanceName, action, compactBody)

		ginCtx.Next()
	}
}

// relayAuth 中继服务鉴权中间件（按实例校验 token）
func relayAuth(mysqlConfigs map[string]*SetupMySQL) gin.HandlerFunc {
	return func(ginCtx *gin.Context) {
		instanceName := ginCtx.Param("name")
		mysqlCfg, exists := mysqlConfigs[instanceName]
		if !exists {
			ginCtx.JSON(http.StatusNotFound, gin.H{"error": "未知的 MySQL 实例: " + instanceName})
			ginCtx.Abort()
			return
		}

		if mysqlCfg.RelayToken == "" {
			ginCtx.Next()
			return
		}

		requestToken := ginCtx.GetHeader("X-Relay-Token")
		if requestToken == "" {
			requestToken = ginCtx.Query("token")
		}
		if requestToken != mysqlCfg.RelayToken {
			ginCtx.JSON(http.StatusUnauthorized, gin.H{"error": "无效的中继令牌"})
			ginCtx.Abort()
			return
		}
		ginCtx.Next()
	}
}

func getDBConnection(ginCtx *gin.Context) *sql.DB {
	instanceName := ginCtx.Param("name")
	connection := GetPool(instanceName)
	if connection == nil {
		ginCtx.JSON(http.StatusServiceUnavailable, gin.H{"error": "MySQL 实例未就绪: " + instanceName})
		return nil
	}
	return connection
}

// queryReq 查询请求体
type queryReq struct {
	SQL  string `json:"sql" binding:"required"`
	Args []any  `json:"args"`
}

// mysqlQuery POST /api/mysql/:name/query
func mysqlQuery(ginCtx *gin.Context) {
	connection := getDBConnection(ginCtx)
	if connection == nil {
		return
	}

	var request queryReq
	if bindErr := ginCtx.ShouldBindJSON(&request); bindErr != nil {
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + bindErr.Error()})
		return
	}

	resultRows, queryErr := connection.Query(request.SQL, request.Args...)
	if queryErr != nil {
		ginCtx.JSON(http.StatusInternalServerError, gin.H{
			"error":   "查询执行失败",
			"detail":  queryErr.Error(),
			"success": false,
		})
		return
	}
	defer func(resultRows *sql.Rows) {
		_ = resultRows.Close()
	}(resultRows)

	columnNames, _ := resultRows.Columns()
	jsonResult := make([]map[string]any, 0)

	for resultRows.Next() {
		columnValues := make([]any, len(columnNames))
		valuePointers := make([]any, len(columnNames))
		for idx := range columnValues {
			valuePointers[idx] = &columnValues[idx]
		}

		if scanErr := resultRows.Scan(valuePointers...); scanErr != nil {
			ginCtx.JSON(http.StatusInternalServerError, gin.H{
				"error":   "结果扫描失败",
				"detail":  scanErr.Error(),
				"success": false,
			})
			return
		}

		resultRow := make(map[string]any)
		for colIdx, colName := range columnNames {
			rawValue := columnValues[colIdx]
			if byteVal, isBytes := rawValue.([]byte); isBytes {
				resultRow[colName] = string(byteVal)
			} else {
				resultRow[colName] = rawValue
			}
		}
		jsonResult = append(jsonResult, resultRow)
	}

	if rowsErr := resultRows.Err(); rowsErr != nil {
		ginCtx.JSON(http.StatusInternalServerError, gin.H{
			"error":   "遍历结果出错",
			"detail":  rowsErr.Error(),
			"success": false,
		})
		return
	}

	ginCtx.JSON(http.StatusOK, gin.H{
		"success": true,
		"columns": columnNames,
		"rows":    jsonResult,
		"count":   len(jsonResult),
	})
}

// mysqlExec POST /api/mysql/:name/exec
func mysqlExec(ginCtx *gin.Context) {
	connection := getDBConnection(ginCtx)
	if connection == nil {
		return
	}

	var request queryReq
	if bindErr := ginCtx.ShouldBindJSON(&request); bindErr != nil {
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + bindErr.Error()})
		return
	}

	execResult, execErr := connection.Exec(request.SQL, request.Args...)
	if execErr != nil {
		ginCtx.JSON(http.StatusInternalServerError, gin.H{
			"error":   "操作执行失败",
			"detail":  execErr.Error(),
			"success": false,
		})
		return
	}

	affectedRows, _ := execResult.RowsAffected()
	insertedID, _ := execResult.LastInsertId()

	ginCtx.JSON(http.StatusOK, gin.H{
		"success":        true,
		"rows_affected":  affectedRows,
		"last_insert_id": insertedID,
	})
}
