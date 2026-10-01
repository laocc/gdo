package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

var connectionPools = make(map[string]*sql.DB)
var poolsMutex sync.RWMutex

// DB 默认连接池（兼容旧代码）：优先指向名为 default 的实例，
// 无 default 实例时取实例名字典序第一个（不再是"第一个初始化完成的"，避免 map 随机顺序带来不确定性）
var DB *sql.DB

type SetupMySQL struct {
	Name       string
	Run        bool
	Host       string
	Port       int
	User       string
	Password   string
	Database   string
	PoolSize   int
	RelayToken string
}

// DSN 生成 MySQL 连接串（Data Source Name），供 sql.Open 使用。
// 注意：Net 填 "tcp"；若将来要走 Unix Socket，改成 Net:"unix" + Addr 填 socket 文件路径即可。
//
// 用 NewConfig() 起底再覆盖字段，不能直接写 Config 字面量：
// 驱动 Config 的零值与它的默认值并不相同（如 AllowNativePasswords 默认 true、零值是 false，
// 默认 Collation 也不是空串），漏掉默认值会让服务器要求 mysql_native_password 的账号直接
// 报 "this user requires mysql native password authentication" 而连不上。
func (cfg SetupMySQL) DSN() string {
	dsnConfig := mysqldriver.NewConfig()
	dsnConfig.User = cfg.User
	dsnConfig.Passwd = cfg.Password
	dsnConfig.Net = "tcp"
	dsnConfig.Addr = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	dsnConfig.DBName = cfg.Database
	dsnConfig.Params = map[string]string{"charset": "utf8mb4"}
	dsnConfig.ParseTime = true
	dsnConfig.Loc = time.Local
	// 只设拨号超时：地址被防火墙丢包时不会无限卡在启动阶段；
	// 读写超时留空，避免把报表这类耗时较长的查询误杀
	dsnConfig.Timeout = 5 * time.Second

	return dsnConfig.FormatDSN()
}

// InitAllDB 并发初始化所有 run=1 的 MySQL 实例连接池。
// 各实例之间没有依赖，逐个初始化只是白等网络握手，所以并发进行。
func InitAllDB(mysqlConfigs map[string]*SetupMySQL) error {
	// map 遍历顺序是随机的，先固化成有序切片：日志输出稳定，默认库选取也有确定性
	instanceNames := make([]string, 0, len(mysqlConfigs))
	for instanceName, mysqlCfg := range mysqlConfigs {
		if !mysqlCfg.Run {
			log.Printf("[DB] %s: run=0, 跳过", instanceName)
			continue
		}
		instanceNames = append(instanceNames, instanceName)
	}
	sort.Strings(instanceNames)

	pools := make([]*sql.DB, len(instanceNames))
	initErrs := make([]error, len(instanceNames))

	var waitGroup sync.WaitGroup
	for index := range instanceNames {
		waitGroup.Add(1)
		go func(i int) {
			defer waitGroup.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					initErrs[i] = fmt.Errorf("初始化异常: %v", recovered)
				}
			}()
			pools[i], initErrs[i] = initOnePool(instanceNames[i], mysqlConfigs[instanceNames[i]])
		}(index)
	}
	waitGroup.Wait()

	// 任一实例失败即整体失败：已建好的池先关掉，避免残留连接
	for index, initErr := range initErrs {
		if initErr == nil {
			continue
		}
		for _, pool := range pools {
			if pool != nil {
				_ = pool.Close()
			}
		}
		return fmt.Errorf("初始化实例 [%s] 失败: %w", instanceNames[index], initErr)
	}

	// 固定顺序登记；默认库优先名为 default 的实例，其次取字典序第一个
	defaultIndex := 0
	for index, instanceName := range instanceNames {
		poolsMutex.Lock()
		connectionPools[instanceName] = pools[index]
		poolsMutex.Unlock()

		if instanceName == "default" {
			defaultIndex = index
		}
	}
	if len(instanceNames) > 0 {
		DB = pools[defaultIndex]
	} else {
		log.Println("[DB] 警告: 没有 run=1 的 MySQL 实例，服务将在无数据库状态下运行")
	}

	return nil
}

func initOnePool(instanceName string, mysqlCfg *SetupMySQL) (*sql.DB, error) {
	connection, err := sql.Open("mysql", mysqlCfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开连接失败: %w", err)
	}

	connection.SetMaxOpenConns(mysqlCfg.PoolSize)
	connection.SetMaxIdleConns(mysqlCfg.PoolSize)
	connection.SetConnMaxLifetime(30 * time.Minute)
	connection.SetConnMaxIdleTime(5 * time.Minute)

	// 先 Ping 一次，确保 MySQL 可达并初始化 driver connector（避免后续 Conn(nil) 空指针）
	log.Printf("[DB] %s: 验证 MySQL 连接...", instanceName)
	if pingErr := connection.Ping(); pingErr != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("ping 失败: %w", pingErr)
	}

	// 剩余连接放到后台补齐：启动只等这 1 次握手，余下 poolSize-1 个连接不阻塞启动
	log.Printf("[DB] %s: 剩余 %d 个长连接转后台预热", instanceName, mysqlCfg.PoolSize-1)
	warmUpPool(instanceName, connection, mysqlCfg.PoolSize)

	return connection, nil
}

// warmUpPool 后台把连接池预热到 poolSize 个长连接。
// Ping 已经建好 1 个，这里并发建立剩余 poolSize-1 个（串行建要等 N 次握手，并发只相当于 1 次）。
// 预热失败不影响已经返回的连接池，只记日志，后续请求会按需建连。
func warmUpPool(instanceName string, connection *sql.DB, poolSize int) {
	remainCount := poolSize - 1
	if remainCount <= 0 {
		return
	}

	// 后台预热：普通协程 + recover（预热日志走标准库 log，不依赖宿主的日志框架）
	runAsync(func() {
		begin := time.Now()
		preCreatedConn := make([]*sql.Conn, remainCount)

		var preCreateGroup sync.WaitGroup
		for connIndex := 0; connIndex < remainCount; connIndex++ {
			preCreateGroup.Add(1)
			go func(index int) {
				defer preCreateGroup.Done()
				conn, connErr := connection.Conn(context.Background())
				if connErr != nil {
					log.Printf("[DB] %s: 后台预热连接失败: %v", instanceName, connErr)
					return
				}
				preCreatedConn[index] = conn
			}(connIndex)
		}
		preCreateGroup.Wait()

		// 归还池（Close 不是断开，而是把连接放回空闲队列等待复用）
		for _, establishedConn := range preCreatedConn {
			if establishedConn != nil {
				_ = establishedConn.Close()
			}
		}

		log.Printf("[Go-DB] %s: 后台连接池预热完成 (MaxOpen=%d, MaxIdle=%d)，耗时 %v",
			instanceName, poolSize, poolSize, time.Since(begin))
	})
}

// GetPool 根据实例名获取连接池
func GetPool(instanceName string) *sql.DB {
	poolsMutex.RLock()
	defer poolsMutex.RUnlock()
	return connectionPools[instanceName]
}

// CloseAllDb 关闭所有连接池
func CloseAllDb() {
	poolsMutex.Lock()
	defer poolsMutex.Unlock()
	for instanceName, pool := range connectionPools {
		_ = pool.Close()
		log.Printf("[Mysql] %s: 已关闭", instanceName)
	}

	connectionPools = make(map[string]*sql.DB)
	DB = nil
}

// AllPools 返回所有活跃连接池
func AllPools() map[string]*sql.DB {
	poolsMutex.RLock()
	defer poolsMutex.RUnlock()
	allPools := make(map[string]*sql.DB, len(connectionPools))
	for instanceName, pool := range connectionPools {
		allPools[instanceName] = pool
	}
	return allPools
}

// PoolHealthInfo 单个 MySQL 实例的健康状态与连接池统计
type PoolHealthInfo struct {
	Status       string `json:"status"`       // ok / unhealthy
	Error        string `json:"error"`        // 不健康时的错误信息
	OpenConns    int    `json:"openConns"`    // 当前打开的连接数
	IdleConns    int    `json:"idleConns"`    // 空闲连接数
	InUse        int    `json:"inUse"`        // 使用中的连接数
	MaxOpenConns int    `json:"maxOpenConns"` // 连接池上限
}

// DefaultInstanceName 默认实例名：config.ini 里固定叫 default，
// 它的连接池统计由 Stats() 单独输出（系统状态里的 dbOpen 等），PoolHealth 不再重复列出。
const DefaultInstanceName = "default"

// PoolHealth 检查所有活跃连接池，返回各实例健康状态与整体是否健康。
// 默认实例 default 不出现在返回的 map 中（避免与其单独输出的统计重复），
// 但仍会 ping，因此它的健康状况照常计入 allHealthy。
func PoolHealth() (map[string]*PoolHealthInfo, bool) {
	activePools := AllPools()

	poolStatus := make(map[string]*PoolHealthInfo, len(activePools))
	allHealthy := true

	for instanceName, connection := range activePools {
		statusInfo := &PoolHealthInfo{}
		if pingErr := connection.Ping(); pingErr != nil {
			allHealthy = false
			statusInfo.Status = "unhealthy"
			statusInfo.Error = pingErr.Error()
		} else {
			poolStats := connection.Stats()
			statusInfo.Status = "ok"
			statusInfo.OpenConns = poolStats.OpenConnections
			statusInfo.IdleConns = poolStats.Idle
			statusInfo.InUse = poolStats.InUse
			statusInfo.MaxOpenConns = poolStats.MaxOpenConnections
		}
		// default 已由 Stats() 单独输出统计，这里只跳过"输出"，不影响上面的健康判定
		if instanceName == DefaultInstanceName {
			continue
		}
		poolStatus[instanceName] = statusInfo
	}
	return poolStatus, allHealthy
}

// ─── 常用数据库操作封装（基于默认 DB 连接池） ───────────

// Query 执行 SELECT 查询，返回多行结果集。
// 用法: rows, err := mysql.Query("SELECT * FROM tabFans WHERE status = ?", 1)
func Query(query string, args ...any) (*sql.Rows, error) {
	queryBegin := time.Now()
	rows, queryErr := DB.Query(query, args...)
	recordSQL(time.Since(queryBegin), query, args...)
	return rows, queryErr
}

// QueryContext 带 context 的 Query，支持超时/取消。
func QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	queryBegin := time.Now()
	rows, queryErr := DB.QueryContext(ctx, query, args...)
	recordSQL(time.Since(queryBegin), query, args...)
	return rows, queryErr
}

// QueryRow 执行 SELECT 查询，返回单行结果。
// 用法: row := mysql.QueryRow("SELECT * FROM tabFans WHERE openid = ?", "oXXX")
//
//	row.Scan(&id, &name, ...)
//
// QueryRow 真正执行发生在调用方 Scan 时，这里只能记录语句、无法计耗时。
func QueryRow(query string, args ...any) *sql.Row {
	recordSQL(0, query, args...)
	return DB.QueryRow(query, args...)
}

// QueryRowContext 带 context 的 QueryRow。
func QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	recordSQL(0, query, args...)
	return DB.QueryRowContext(ctx, query, args...)
}

// Exec 执行 INSERT / UPDATE / DELETE，返回影响行数和 lastInsertId。
// 用法: result, err := mysql.Exec("UPDATE tabFans SET nickname = ? WHERE id = ?", "新名", 1)
func Exec(query string, args ...any) (sql.Result, error) {
	execBegin := time.Now()
	result, execErr := DB.Exec(query, args...)
	recordSQL(time.Since(execBegin), query, args...)
	return result, execErr
}

// ExecContext 带 context 的 Exec。
func ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	execBegin := time.Now()
	result, execErr := DB.ExecContext(ctx, query, args...)
	recordSQL(time.Since(execBegin), query, args...)
	return result, execErr
}

// Prepare 预编译 SQL 语句，用于重复执行。
// 用法: stmt, err := mysql.Prepare("SELECT * FROM tabFans WHERE id = ?")
func Prepare(query string) (*sql.Stmt, error) {
	return DB.Prepare(query)
}

// PrepareContext 带 context 的 Prepare。
func PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return DB.PrepareContext(ctx, query)
}

// Ping 检查默认数据库连接是否正常。
func Ping() error {
	return DB.Ping()
}

// Version 返回默认 MySQL 实例的版本号（SELECT VERSION()）；连接不可用时返回空串
func Version() string {
	if DB == nil {
		return ""
	}
	var versionValue string
	if queryErr := DB.QueryRow("SELECT VERSION()").Scan(&versionValue); queryErr != nil {
		return ""
	}
	return versionValue
}

// PingContext 带 context 的 Ping。
func PingContext(ctx context.Context) error {
	return DB.PingContext(ctx)
}

// Stats 返回默认连接池的统计信息。
func Stats() sql.DBStats {
	return DB.Stats()
}
