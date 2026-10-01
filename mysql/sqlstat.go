package mysql

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SQL 执行统计（落库表 tabSql，见 model/sqlstat）：
//
// 埋点只放在本包的 SQL 出口（Builder 的 All/Insert/InsertBatch/Update/Delete、包级 Query/Exec、
// 表结构查询），业务代码不需要任何改动；relay.go 的中继 SQL 走的是 GetPool 拿到的裸连接，
// 天然不会被统计（中继是别的系统发来的语句，不属于本系统的调用点）。
//
// 每条 SQL 的归类维度：统计日 + 类型（增删改查）+ 归一化 SQL 的 md5 + 调用位置（文件:行）。
// 调用位置靠调用栈回溯取「第一个不在本包里的帧」，所以 Get → All 这种包内链路
// 会把位置记成真正发起查询的业务代码。
//
// 业务线程只做「归一化 + 位置 + 内存原子累加」，不做任何 IO；
// 落库由 model/sqlstat 的定时任务取快照后交给后台写库协程完成。

// 统计类型取值
const (
	KindSelect = "select"
	KindInsert = "insert"
	KindUpdate = "update"
	KindDelete = "delete"
	KindOther  = "other"
)

// sqlStatEnabled 统计总开关：关闭时埋点只剩一次原子读，开销可忽略。
var sqlStatEnabled atomic.Bool

// SetSQLStatEnabled 设置统计开关：只由服务启动时的配置注入（config.ini 的 [sqlstat] enable）。
// 刻意不提供运行期修改入口，避免误操作把正在观察的统计关掉；要改就改配置并重启。
func SetSQLStatEnabled(enable bool) {
	sqlStatEnabled.Store(enable)
}

// SQLStatEnabled 返回当前统计开关状态（只读，供管理端页面展示）
func SQLStatEnabled() bool {
	return sqlStatEnabled.Load()
}

// SQLRecord 一条统计快照：某天、某类型、某归一化 SQL、某调用位置的累计值增量
type SQLRecord struct {
	Day       string // 统计日（本地时区 YYYY-MM-DD）
	Kind      string // select/insert/update/delete/other
	Hash      string // 归一化 SQL 的 md5
	File      string // 调用位置文件（项目内相对路径，如 controller/admin/express.go）
	Line      int    // 调用位置行号
	Function  string // 调用位置函数名（如 controller/admin.ListExpress）
	Text      string // 归一化后的 SQL 文本（不含参数值）
	Count     uint64 // 本周期的执行次数
	CostTotal uint64 // 本周期累计耗时（微秒）
	CostMax   uint64 // 本周期单次最大耗时（微秒）
	FirstTime int64  // 当日首次执行时间（Unix 秒）
	LastTime  int64  // 本次快照时间（Unix 秒）
}

// statKey 统计主键：同一句 SQL 从不同位置调用算两条记录
type statKey struct {
	day  string
	kind string
	hash string
	file string
	line int
}

// statEntry 一个主键的累计值：业务线程只用原子操作，不加锁
type statEntry struct {
	text      string
	function  string
	count     atomic.Uint64
	costTotal atomic.Uint64
	costMax   atomic.Uint64
	firstTime int64
}

var (
	sqlStatMap  sync.Map // statKey -> *statEntry
	sqlTextMemo sync.Map // 原始 SQL 文本 -> *normalizedSQL，避免同一句反复跑正则
	sqlMemoSize atomic.Int64
)

// sqlMemoLimit 归一化缓存条数上限：超限后不再新增（已缓存的继续命中）。
// 含随机字面量的 SQL（如坐标）会让缓存不断增长，必须封顶。
const sqlMemoLimit = 4096

// normalizedSQL 归一化结果
type normalizedSQL struct {
	display string // 展示用文本：保留原始大小写，只把值抹掉
	key     string // 聚合用文本：再统一小写，避免手写 SQL 的大小写差异拆成两条
	hash    string // md5(key)
	kind    string // 增删改查类型
}

// 归一化正则：
//   - 连续空白折叠（不同人手写的 SQL 空格不一致）
//   - 单引号字符串抹成 ?（含 ” 转义）
//   - LIMIT/OFFSET 后的数字抹成 ?（分页页码、每页条数不参与归类）
//   - 运算符/逗号/括号后的数字抹成 ?（拼接进 SQL 的条件值，如 state=2）
//     只抹这两处，是为了保住 SELECT 1 这类常量探测语句的可读性，也避免抹掉标识符里的数字
//   - IN (?, ?, ?) 折叠成 IN (?)，让「按几个 ID 查」合并成一条
//   - 连续重复的 VALUES 组折叠成一组，让批量插入的行数不影响归类
//   - 结尾的 LIMIT ? 补成 LIMIT ? OFFSET ?，消除「第一页没有 OFFSET」造成的拆条
var (
	sqlSpacePattern    = regexp.MustCompile(`\s+`)
	sqlQuotePattern    = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'`)
	sqlLimitNumPattern = regexp.MustCompile(`(?i)\b(LIMIT|OFFSET)\s+\d+(?:\.\d+)?`)
	sqlNumberPattern   = regexp.MustCompile(`([=<>!+\-*/(,]\s*)\d+(?:\.\d+)?`)
	sqlInListPattern   = regexp.MustCompile(`(?i)\bIN\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	sqlValuesPattern   = regexp.MustCompile(`(?i)(VALUES\s*\(\s*\?[^()]*\))(\s*,\s*\(\s*\?[^()]*\))+`)
	sqlLimitPattern    = regexp.MustCompile(`(?i)LIMIT\s+\?\s*$`)
	sqlLimitReplace    = "LIMIT ? OFFSET ?"
	sqlLimitNumReplace = "$1 ?"
	sqlNumberReplace   = "${1}?"
	sqlValuesReplace   = "$1"
	sqlInListReplace   = "IN (?)"
	sqlQuoteReplace    = "?"
	sqlSpaceReplace    = " "
	sqlDayTimeFormat   = "2006-01-02"
)

// RecordSQL 记录一次 SQL 执行：query 为实际执行的 SQL，cost 为本次执行耗时（拿不到耗时传 0）
func RecordSQL(cost time.Duration, query string, args ...any) {
	currentHookValue := hook()

	// SQL 日志与 SQL 统计两条出口都不需要时才零开销返回
	if !sqlStatEnabled.Load() && currentHookValue.SQL == nil {
		return
	}

	// skip=1：跳过 RecordSQL 自身，取「第一个不在本库内的帧」，即真正发起这次 SQL 的业务位置
	site := callSite(1)
	if site.File == "" {
		return
	}

	if currentHookValue.SQL != nil {
		hookSQL(site, query, args)
	}

	if query == "" || !sqlStatEnabled.Load() {
		return
	}

	normalized := normalizeSQL(query)
	nowTime := time.Now()
	statKeyValue := statKey{
		day:  nowTime.Format(sqlDayTimeFormat),
		kind: normalized.kind,
		hash: normalized.hash,
		file: site.File,
		line: site.Line,
	}

	costMicro := uint64(0)
	if cost > 0 {
		costMicro = uint64(cost.Microseconds())
	}

	if loaded, exists := sqlStatMap.Load(statKeyValue); exists {
		accumulate(loaded.(*statEntry), costMicro)
		return
	}

	entry := &statEntry{
		text:      normalized.display,
		function:  site.Function,
		firstTime: nowTime.Unix(),
	}
	entry.count.Store(1)
	entry.costTotal.Store(costMicro)
	entry.costMax.Store(costMicro)

	if actual, loaded := sqlStatMap.LoadOrStore(statKeyValue, entry); loaded {
		// 并发下其它线程先写入：退化成累加，避免这一条漏记
		accumulate(actual.(*statEntry), costMicro)
	}
}

// accumulate 给已有条目累加一次执行
func accumulate(entry *statEntry, costMicro uint64) {
	entry.count.Add(1)
	if costMicro == 0 {
		return
	}
	entry.costTotal.Add(costMicro)
	raiseUint64(&entry.costMax, costMicro)
}

// Snapshot 取走当前所有累计增量并把内存计数清零（由落库任务调用）。
// 用 Swap(0) 取走，取走之后新发生的计数自然留到下一个周期，互相不丢。
func Snapshot() []SQLRecord {
	nowTime := time.Now()
	todayText := nowTime.Format(sqlDayTimeFormat)
	nowSecond := nowTime.Unix()

	records := make([]SQLRecord, 0, 64)
	sqlStatMap.Range(func(rawKey any, rawValue any) bool {
		statKeyValue := rawKey.(statKey)
		entry := rawValue.(*statEntry)

		count := entry.count.Swap(0)
		costTotal := entry.costTotal.Swap(0)
		costMax := entry.costMax.Swap(0)

		if count == 0 {
			// 已经不是当天的旧条目直接丢掉，避免内存长期堆积历史日期
			if statKeyValue.day != todayText {
				sqlStatMap.Delete(rawKey)
			}
			return true
		}

		records = append(records, SQLRecord{
			Day:       statKeyValue.day,
			Kind:      statKeyValue.kind,
			Hash:      statKeyValue.hash,
			File:      statKeyValue.file,
			Line:      statKeyValue.line,
			Function:  entry.function,
			Text:      entry.text,
			Count:     count,
			CostTotal: costTotal,
			CostMax:   costMax,
			FirstTime: entry.firstTime,
			LastTime:  nowSecond,
		})
		return true
	})

	return records
}

// normalizeSQL 归一化 SQL 并算出聚合用哈希；同一句 SQL 的结果会缓存复用
func normalizeSQL(query string) normalizedSQL {
	if cached, exists := sqlTextMemo.Load(query); exists {
		return *(cached.(*normalizedSQL))
	}

	display := sqlSpacePattern.ReplaceAllString(strings.TrimSpace(query), sqlSpaceReplace)
	display = sqlQuotePattern.ReplaceAllString(display, sqlQuoteReplace)
	display = sqlLimitNumPattern.ReplaceAllString(display, sqlLimitNumReplace)
	display = sqlNumberPattern.ReplaceAllString(display, sqlNumberReplace)
	display = sqlInListPattern.ReplaceAllString(display, sqlInListReplace)
	display = sqlValuesPattern.ReplaceAllString(display, sqlValuesReplace)
	display = sqlLimitPattern.ReplaceAllString(display, sqlLimitReplace)

	result := normalizedSQL{
		display: display,
		key:     strings.ToLower(display),
		kind:    sqlKindOf(display),
	}
	hashBytes := md5.Sum([]byte(result.key))
	result.hash = hex.EncodeToString(hashBytes[:])

	if sqlMemoSize.Load() < sqlMemoLimit {
		// 并发下可能略微超限，缓存上限只影响命中率，不影响统计正确性
		if _, loaded := sqlTextMemo.LoadOrStore(query, &result); !loaded {
			sqlMemoSize.Add(1)
		}
	}
	return result
}

// sqlKindOf 按 SQL 首个关键字判定增删改查类型（REPLACE 归入 insert）
func sqlKindOf(sqlText string) string {
	upperText := strings.ToUpper(strings.TrimSpace(sqlText))
	switch {
	case strings.HasPrefix(upperText, "SELECT"):
		return KindSelect
	case strings.HasPrefix(upperText, "INSERT"), strings.HasPrefix(upperText, "REPLACE"):
		return KindInsert
	case strings.HasPrefix(upperText, "UPDATE"):
		return KindUpdate
	case strings.HasPrefix(upperText, "DELETE"):
		return KindDelete
	default:
		return KindOther
	}
}

// raiseUint64 把峰值抬到 current（只增不减），CAS 失败就重试
func raiseUint64(peak *atomic.Uint64, current uint64) {
	for {
		oldValue := peak.Load()
		if current <= oldValue || peak.CompareAndSwap(oldValue, current) {
			return
		}
	}
}
