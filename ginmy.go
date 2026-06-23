package ginmy

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/yumosx/gotrace"
)

// defaultLogger 是包级别的全局日志器，通过 InitLogger 初始化。
// 使用 atomic.Pointer 避免 InitLogger 与 logError 之间的 data race。
var defaultLogger atomic.Pointer[gotrace.Logger]

// InitLogger 初始化全局日志器。
//   - svc: 服务名，用于日志文件名（格式：YYYY-MM-DD.svc.log）
//   - dir: 日志目录，传 "" 则不写文件，仅输出到 stdout
//   - cfg: 配置，传零值会使用合理的默认值
//
// 使用示例：
//
//	ginmy.InitLogger("myapp", "/var/log/app", gotrace.Config{Stdout: true})
func InitLogger(svc, dir string, cfg gotrace.Config) error {
	logger, err := gotrace.NewLogger(svc, dir, cfg)
	if err != nil {
		return err
	}
	defaultLogger.Store(logger)
	return nil
}

// normalizeResult 归一化 Result.Code：未显式设置时默认为 200，与 HTTP 语义保持一致。
func normalizeResult(r Result) Result {
	if r.Code == 0 {
		r.Code = http.StatusOK
	}
	return r
}

// writeError 统一写入错误响应：HTTP 状态码与 body 内的 Code 保持一致，
// body 使用 Result 结构体保持与成功路径字段名一致。
func writeError(c *gin.Context, status int, msg string) {
	c.JSON(status, Result{
		Code:    status,
		Message: msg,
		Value:   nil,
	})
}

// B 绑定请求参数并执行业务函数，自动处理参数绑定错误和业务错误日志。
func B[Req any](fn func(ctx *gin.Context, req Req) (Result, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req Req
		if err := c.ShouldBind(&req); err != nil {
			logError(c, err)
			writeError(c, http.StatusBadRequest, "invalid request")
			return
		}
		result, err := fn(c, req)
		if err != nil {
			logError(c, err)
			writeError(c, http.StatusInternalServerError, "internal server error")
			return
		}
		c.JSON(http.StatusOK, normalizeResult(result))
	}
}

// NB 不绑定请求参数，直接执行业务函数，自动处理业务错误日志。
func NB(fn func(ctx *gin.Context) (Result, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := fn(c)
		if err != nil {
			logError(c, err)
			writeError(c, http.StatusInternalServerError, "internal server error")
			return
		}
		c.JSON(http.StatusOK, normalizeResult(result))
	}
}

// BS 绑定请求参数并执行业务函数，业务函数可访问调用方注入的 Session。
//
// Session 在路由注册时由调用方提供，框架不感知具体实现（JWT、Redis Session 等）。
// 由于 session 会在所有请求间共享，调用方需保证其实现是并发安全的
// （无状态校验器 / 仅持有共享配置如密钥、连接池）。
// 当 session 为 nil 时返回 500 错误，避免业务函数对 nil 解引用。
func BS[Req any](session Session, fn func(ctx *gin.Context, session Session, req Req) (Result, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if session == nil {
			writeError(c, http.StatusInternalServerError, "session is nil")
			return
		}
		var req Req
		if err := c.ShouldBind(&req); err != nil {
			logError(c, err)
			writeError(c, http.StatusBadRequest, "invalid request")
			return
		}
		result, err := fn(c, session, req)
		if err != nil {
			logError(c, err)
			writeError(c, http.StatusInternalServerError, "internal server error")
			return
		}
		c.JSON(http.StatusOK, normalizeResult(result))
	}
}

// NBS 不绑定请求参数，直接执行业务函数，业务函数可访问调用方注入的 Session。
//
// Session 在路由注册时由调用方提供，框架不感知具体实现（JWT、Redis Session 等）。
// 由于 session 会在所有请求间共享，调用方需保证其实现是并发安全的
// （无状态校验器 / 仅持有共享配置如密钥、连接池）。
// 当 session 为 nil 时返回 500 错误，避免业务函数对 nil 解引用。
func NBS(session Session, fn func(ctx *gin.Context, session Session) (Result, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if session == nil {
			writeError(c, http.StatusInternalServerError, "session is nil")
			return
		}
		result, err := fn(c, session)
		if err != nil {
			logError(c, err)
			writeError(c, http.StatusInternalServerError, "internal server error")
			return
		}
		c.JSON(http.StatusOK, normalizeResult(result))
	}
}

// logError 使用 gotrace 记录请求和错误日志。
// 当 defaultLogger 未初始化时，回退到标准 log，避免日志静默丢失。
func logError(c *gin.Context, err error) {
	logger := defaultLogger.Load()
	if logger == nil {
		log.Printf("[ginmy] logError called before InitLogger, err=%v", err)
		return
	}
	tracer := gotrace.NewTracer()
	c.Request = c.Request.WithContext(gotrace.NewContext(c.Request.Context(), tracer))

	ip := c.ClientIP()
	if idx := strings.IndexByte(ip, ','); idx > 0 {
		ip = strings.TrimSpace(ip[:idx])
	}
	if h, _, e := net.SplitHostPort(ip); e == nil {
		ip = h
	}

	gotrace.LogRequestEntry(logger, tracer, ip, c.Request.URL.Path)
	gotrace.LogErrorEntry(logger, tracer, err.Error(), nil)
}
