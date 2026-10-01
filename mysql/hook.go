package mysql

// 宿主接入点（钩子）：
//
// 本库不依赖任何日志框架、调度框架与项目代码，对外只有两个可注入点：
//   - CallSite：裁剪调用位置的展示形态（库采集到的是磁盘上的原始路径）
//   - SQL：每条 SQL 执行后回调（语句 + 耗时 + 调用位置）
//
// 其余行为都在库内固定，不需要宿主接管：
//   - 错误日志：标准库 log（严重错误直接打到控制台）
//   - 后台协程：普通 go 协程并 recover 掉 panic（只用在连接池预热）
//   - 未注入 SQL 钩子时：完全不输出，也不做栈回溯
//
// 用法（宿主启动时设置一次）：
//
//	mysql.SetHook(mysql.Hook{
//		CallSite: func(raw mysql.CallSite) mysql.CallSite { return trimSite(raw) },
//		SQL:      func(site mysql.CallSite, query string, cost time.Duration, args ...any) { mylog.SQL(site, query, cost, args) },
//	})

import (
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// CallSite 一次调用的位置：File 为源文件路径（库只做归一化，不假定宿主的目录结构），
// Function 为完整函数名（形如 github.com/you/project/model.LoadVoucherList）。
type CallSite struct {
	File     string
	Line     int
	Function string
}

// Hook 宿主注入的钩子；两个字段都可以为 nil，nil 表示不做任何事情。
type Hook struct {
	// CallSite 裁剪调用位置：库采集到的 File 是磁盘上的原始路径，
	// 宿主可在这里换成自己习惯的展示形态（例如裁成项目内相对路径）。
	// 裁剪结果对 SQL 回调与错误日志都生效，为 nil 时原样使用。
	CallSite func(raw CallSite) CallSite

	// SQL 每执行一条 SQL 回调一次（查询、写入、表结构探测都算）。
	// cost 为本次执行耗时（拿不到耗时的出口传 0）；为 nil 时既不出日志也不做栈回溯。
	SQL func(site CallSite, query string, cost time.Duration, args ...any)
}

// currentHook 当前生效的钩子集合，用原子指针承载，允许启动阶段与运行阶段并发读写
var currentHook atomic.Pointer[Hook]

// SetHook 设置钩子集合（宿主启动时调用一次即可；传零值 Hook 表示全部退回默认行为）
func SetHook(hookValue Hook) {
	currentHook.Store(&hookValue)
}

// hook 取当前钩子（未设置时返回零值）
func hook() Hook {
	if loaded := currentHook.Load(); loaded != nil {
		return *loaded
	}
	return Hook{}
}

// recordSQL 库内所有 SQL 出口的统一回调（Builder 的增删改查、包级 Query/Exec、表结构探测都走它）：
// 采集调用位置后交给 Hook.SQL，宿主一次就能拿到「执行了什么 SQL、耗了多久、由哪行业务代码发起」。
// 未注入 SQL 钩子时零开销返回，连栈回溯都不做。
func recordSQL(cost time.Duration, query string, args ...any) {
	sqlHook := hook().SQL
	if sqlHook == nil {
		return
	}

	// skip=1：跳过 recordSQL 自身，取「第一个不在本库内的帧」，即真正发起这次 SQL 的业务位置
	site := callSite(1)
	if site.File == "" {
		return
	}
	sqlHook(site, query, cost, args...)
}

// logError 库内统一的错误日志出口：写标准库 log（严重错误直接打到控制台），不依赖宿主的日志框架。
func logError(message string, queryErr error) {
	log.Printf("[MYSQL] %s: %v", message, queryErr)
}

// runAsync 库内后台任务入口（目前只有连接池预热用）：普通 go 协程，panic 只记日志、不带崩进程。
func runAsync(task func()) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("[MYSQL] 后台任务异常: %v", recovered)
			}
		}()
		task()
	}()
}

// libraryDir 本库源码目录（init 时取自身文件所在目录），回溯调用栈时用来跳过库内帧
var libraryDir string

// libraryModulePath 本库的模块内路径前缀，供 -trimpath / vendored 构建兜底：
// 这两种情况下栈帧里是模块路径，而不是磁盘路径。
const libraryModulePath = "github.com/laocc/gdo/mysql/"

func init() {
	if _, sourceFile, _, ok := runtime.Caller(0); ok {
		libraryDir = filepath.ToSlash(filepath.Dir(sourceFile)) + "/"
	}
}

// callSite 回溯调用栈，返回第一个不在本库内的帧——即真正发起这次 SQL 的业务位置。
// skip 语义与 runtime.Caller 一致：0 = callSite 的调用者本身。
// 采集到的位置先交给 Hook.CallSite 裁剪（未注入则原样返回）。
func callSite(skip int) CallSite {
	var pcBuffer [16]uintptr
	if runtime.Callers(skip+2, pcBuffer[:]) == 0 {
		return CallSite{}
	}

	callSiteValue := CallSite{}
	frames := runtime.CallersFrames(pcBuffer[:])
	for {
		frame, more := frames.Next()
		// 跳过本库的帧：库内的 All/Get/Query/RecordSQL 都指向业务代码才算准确
		if frame.File != "" && !isLibraryFrame(frame.File) {
			callSiteValue = CallSite{
				File:     filepath.ToSlash(frame.File),
				Line:     frame.Line,
				Function: frame.Function,
			}
			break
		}
		if !more {
			break
		}
	}

	if formatHook := hook().CallSite; formatHook != nil {
		return formatHook(callSiteValue)
	}
	return callSiteValue
}

// isLibraryFrame 判断栈帧是否属于本库
func isLibraryFrame(sourceFile string) bool {
	normalized := filepath.ToSlash(sourceFile)
	if libraryDir != "" && strings.HasPrefix(normalized, libraryDir) {
		return true
	}
	return strings.Contains(normalized, libraryModulePath)
}
