package mysql

// 宿主接入点（钩子）：
//
// 本库不依赖任何日志框架、调度框架与项目代码，SQL 日志、错误日志、后台协程、
// 调用位置裁剪全部通过下面的 Hook 由宿主注入；不注入时用库内默认行为：
//   - SQL：不输出（避免库被无意间变成日志刷屏源）
//   - Error：写标准库日志
//   - Async：普通 go 协程，并 recover 掉 panic
//   - CallSite：原样使用
//
// 典型用法（宿主启动时设置一次）：
//
//	mysql.SetHook(mysql.Hook{
//		CallSite: func(raw mysql.CallSite) mysql.CallSite { return trimSite(raw) },
//		SQL:      func(site mysql.CallSite, query string, args ...any) { mylog.SQL(site, query, args) },
//		Error:    func(message string, err error) { mylog.Error(message, err) },
//		Async:    core.Go,
//	})

import (
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

// CallSite 一次调用的位置：File 为源文件路径（库只做归一化，不假定宿主的目录结构），
// Function 为完整函数名（形如 github.com/you/project/model.LoadVoucherList）。
type CallSite struct {
	File     string
	Line     int
	Function string
}

// Hook 宿主注入的钩子；四个字段都可以为 nil，nil 表示使用库内默认行为。
type Hook struct {
	// CallSite 裁剪调用位置：库采集到的 File 是磁盘上的原始路径，
	// 宿主可在这里换成自己习惯的展示形态（例如裁成项目内相对路径）。
	// 该裁剪同时作用于 SQL 日志与 SQL 统计，为 nil 时原样使用。
	CallSite func(raw CallSite) CallSite

	// SQL 每执行一条 SQL 回调一次（查询、写入、表结构探测都算）。
	// 为 nil 时不输出任何内容。
	SQL func(site CallSite, query string, args ...any)

	// Error 执行出错时回调（含事务回滚失败）；为 nil 时写标准库日志。
	Error func(message string, err error)

	// Async 后台任务（连接池预热）；为 nil 时用普通 go 协程并 recover 掉 panic。
	// 宿主若使用协程本地存储（gin 上下文、请求级日志写入器绑定等），
	// 必须在这里换成能继承协程变量的实现，否则后台日志会丢。
	Async func(task func())
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

// hookSQL 执行 SQL 日志钩子（未注入时不输出）
func hookSQL(site CallSite, query string, args ...any) {
	if sqlHook := hook().SQL; sqlHook != nil {
		sqlHook(site, query, args)
	}
}

// hookError 执行错误日志钩子；未注入时写标准库日志
func hookError(message string, queryErr error) {
	if errorHook := hook().Error; errorHook != nil {
		errorHook(message, queryErr)
		return
	}
	log.Printf("[MYSQL] %s: %v", message, queryErr)
}

// hookAsync 执行后台任务；未注入时用普通 go 协程（panic 只记日志，不带崩进程）
func hookAsync(task func()) {
	if asyncHook := hook().Async; asyncHook != nil {
		asyncHook(task)
		return
	}
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
