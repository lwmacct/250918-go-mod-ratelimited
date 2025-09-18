// Package ratelimited 提供支持多层速率限制的高效数据丢弃功能
//
// 核心功能：
//   - 基于 io.Discard 的零内存拷贝数据丢弃
//   - 支持任意层数的嵌套速率限制器
//   - 精确的字节计数和请求统计
//   - 上下文控制和配额管理
//
// 基本使用：
//
//	// 单层限制
//	limiters := ratelimited.Chain(primaryLimiter)
//	copied, err := ratelimited.CopyWithRateLimit(ctx, reader, limiters)
//
//	// 双层限制 (常见场景：主要+次要限制)
//	limiters := ratelimited.Chain(primaryLimiter, secondaryLimiter)
//
//	// 多层限制 (复杂场景：分层级联限制)
//	limiters := ratelimited.Chain(level1Limiter, level2Limiter, level3Limiter, level4Limiter)
//
//	// 有限流配额管理
//	var quota int64 = 1024*1024 // 1MB配额
//	copied, err := ratelimited.CopyNWithRateLimit(
//	    ctx, reader, quota, limiters,
//	    ratelimited.WithSharedQuota(&quota),
//	    ratelimited.WithBytesCounter(&totalBytes),
//	)
//
// 建造者模式（适用于复杂场景）：
//
//	limiters := ratelimited.NewBuilder().
//	    Add("tier1", tier1Limiter).
//	    Add("tier2", tier2Limiter).
//	    Add("tier3", tier3Limiter).
//	    Build()
package ratelimited

import (
	"context"
	"io"
	"sync/atomic"

	"golang.org/x/time/rate"
)

// Limiter 速率限制器接口，兼容 golang.org/x/time/rate.Limiter
type Limiter interface {
	WaitN(ctx context.Context, n int) error
}

// DiscardWriter 支持多层速率限制的高效数据丢弃写入器
type DiscardWriter struct {
	// 速率限制器链 - 支持多层嵌套限制
	limiters []Limiter

	// 上下文控制
	ctx context.Context

	// 统计信息 (可选)
	bytesWritten *int64  // 写入字节统计
	requestCount *uint64 // 请求次数统计

	// 配额管理 (可选，用于有限流)
	sharedRemaining *int64 // 共享剩余配额指针

	// 批量令牌处理
	batchSize       int64 // 批量申请令牌大小
	remainingTokens int64 // 当前批次剩余令牌 (需要原子访问)
}

// DiscardWriterOption 配置选项
type DiscardWriterOption func(*DiscardWriter)

// WithContext 设置上下文
func WithContext(ctx context.Context) DiscardWriterOption {
	return func(w *DiscardWriter) {
		w.ctx = ctx
	}
}

// WithBytesCounter 设置字节统计计数器
func WithBytesCounter(counter *int64) DiscardWriterOption {
	return func(w *DiscardWriter) {
		w.bytesWritten = counter
	}
}

// WithRequestCounter 设置请求计数器
func WithRequestCounter(counter *uint64) DiscardWriterOption {
	return func(w *DiscardWriter) {
		w.requestCount = counter
	}
}

// WithSharedQuota 设置共享配额（有限流模式）
func WithSharedQuota(quota *int64) DiscardWriterOption {
	return func(w *DiscardWriter) {
		w.sharedRemaining = quota
	}
}

// WithBatchSize 设置批量令牌大小
func WithBatchSize(size int64) DiscardWriterOption {
	return func(w *DiscardWriter) {
		w.batchSize = size
	}
}

// NewDiscardWriter 创建支持多层速率限制的数据丢弃写入器
func NewDiscardWriter(limiters []Limiter, opts ...DiscardWriterOption) *DiscardWriter {
	w := &DiscardWriter{
		limiters:  limiters,
		ctx:       context.Background(),
		batchSize: 64 * 1024, // 默认64KB批次
	}

	// 应用选项
	for _, opt := range opts {
		opt(w)
	}

	return w
}

// Write 实现 io.Writer 接口，支持多层速率限制的数据丢弃
func (w *DiscardWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}

	// 检查上下文是否被取消
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	default:
	}

	// 有限流：使用原子操作安全地检查和预留配额
	if w.sharedRemaining != nil {
		for {
			current := atomic.LoadInt64(w.sharedRemaining)
			if current <= 0 {
				return 0, io.EOF // 配额耗尽
			}

			// 确定实际可用的字节数
			available := int(current)
			if n > available {
				n = available // 调整到剩余配额
			}
			if n <= 0 {
				return 0, io.EOF
			}

			// 原子地预留配额，避免竞态条件
			newRemaining := current - int64(n)
			if atomic.CompareAndSwapInt64(w.sharedRemaining, current, newRemaining) {
				// 成功预留配额，跳出循环
				break
			}
			// 如果CAS失败，说明其他goroutine修改了配额，重试
		}
	}

	// 批量令牌管理
	if atomic.LoadInt64(&w.remainingTokens) < int64(n) {
		batchSize := w.batchSize
		
		// 注意：配额检查已在前面完成，这里不再重复检查
		// 如果有配额限制，batchSize可能需要调整以适应剩余配额
		if w.sharedRemaining != nil && batchSize > int64(n) {
			// 在有配额限制的情况下，避免申请过多令牌
			batchSize = int64(n)
		}

		if batchSize <= 0 {
			return 0, io.EOF
		}

		// 为所有速率限制器申请令牌
		if err := w.waitForTokens(int(batchSize)); err != nil {
			// 如果令牌申请失败且我们已经预留了配额，需要回滚配额
			if w.sharedRemaining != nil {
				atomic.AddInt64(w.sharedRemaining, int64(n)) // 回滚配额
			}
			return 0, err
		}
		atomic.StoreInt64(&w.remainingTokens, batchSize)
	}

	// 更新统计
	if w.requestCount != nil {
		atomic.AddUint64(w.requestCount, 1)
	}
	if w.bytesWritten != nil {
		atomic.AddInt64(w.bytesWritten, int64(n))
	}

	// 配额已在前面通过CAS操作预留，这里不需要再次扣除

	// 消费令牌
	atomic.AddInt64(&w.remainingTokens, -int64(n))

	// 数据直接丢弃，不做任何存储
	return n, nil
}

// waitForTokens 为所有速率限制器等待令牌
// 对于上下文相关错误（取消、超时）立即返回，对于其他错误则跳过该限制器继续处理
func (w *DiscardWriter) waitForTokens(n int) error {
	var lastErr error
	successCount := 0

	for _, limiter := range w.limiters {
		if limiter != nil {
			if err := limiter.WaitN(w.ctx, n); err != nil {
				// 检查是否为上下文相关的致命错误
				if w.ctx.Err() != nil {
					// 上下文被取消或超时，立即返回
					return err
				}

				// 非致命错误，记录并继续处理下一个限制器
				lastErr = err
				continue
			}
			successCount++
		}
	}

	// 如果所有限制器都失败了，返回最后一个错误
	if successCount == 0 && lastErr != nil {
		return lastErr
	}

	return nil
}

// CopyWithRateLimit 使用多层速率限制从 reader 复制数据到 Discard
// 这是最常用的便利函数
func CopyWithRateLimit(ctx context.Context, reader io.Reader, limiters []Limiter, opts ...DiscardWriterOption) (int64, error) {
	// 添加上下文选项
	allOpts := append([]DiscardWriterOption{WithContext(ctx)}, opts...)

	writer := NewDiscardWriter(limiters, allOpts...)
	return io.Copy(writer, reader)
}

// CopyNWithRateLimit 使用多层速率限制复制指定字节数到 Discard
func CopyNWithRateLimit(ctx context.Context, reader io.Reader, n int64, limiters []Limiter, opts ...DiscardWriterOption) (int64, error) {
	// 添加上下文选项
	allOpts := append([]DiscardWriterOption{WithContext(ctx)}, opts...)

	writer := NewDiscardWriter(limiters, allOpts...)
	return io.CopyN(writer, reader, n)
}

// =============================================================================
// 多层限制器构造函数
// =============================================================================

// Chain 创建多层速率限制器链
// 支持任意数量的速率限制器，按顺序应用限制（越靠前的限制器优先生效）
//
// 常见使用模式：
//   - 单层限制: Chain(primaryLimiter)
//   - 双层限制: Chain(tier1Limiter, tier2Limiter)
//   - 三层限制: Chain(level1, level2, level3)
//   - 四层限制: Chain(upstream, midstream, downstream, endpoint)
//   - 多层限制: Chain(limiter1, limiter2, limiter3, ...)
//
// nil 限制器会被自动过滤，因此可以安全地传入 nil 值
func Chain(limiters ...*rate.Limiter) []Limiter {
	result := make([]Limiter, 0, len(limiters))
	for _, limiter := range limiters {
		if limiter != nil {
			result = append(result, limiter)
		}
	}
	return result
}

// =============================================================================
// 调试支持 - 带名称的限制器
// =============================================================================

// NamedLimiter 带名称的限制器，便于调试和日志记录
// 使用示例：
//
//	limiters := ChainWithNames(
//	    NamedLimiter{Name: "primary", Limiter: primaryLimiter},
//	    NamedLimiter{Name: "secondary", Limiter: secondaryLimiter},
//	)
type NamedLimiter struct {
	Name    string
	Limiter *rate.Limiter
}

// ChainWithNames 创建带名称的多层限制器链
func ChainWithNames(namedLimiters ...NamedLimiter) []Limiter {
	result := make([]Limiter, 0, len(namedLimiters))
	for _, nl := range namedLimiters {
		if nl.Limiter != nil {
			result = append(result, nl.Limiter)
		}
	}
	return result
}

// =============================================================================
// 建造者模式 - 灵活的链式构造方式
// =============================================================================

// Builder 限制器建造者，支持链式调用构造复杂的限制器链
// 使用示例：
//
//	limiters := NewBuilder().
//	    Add("primary", primaryLimiter).
//	    Add("secondary", secondaryLimiter).
//	    Build()
type Builder struct {
	limiters []NamedLimiter
}

// NewBuilder 创建限制器建造者
func NewBuilder() *Builder {
	return &Builder{}
}

// Add 添加命名限制器
func (b *Builder) Add(name string, limiter *rate.Limiter) *Builder {
	if limiter != nil {
		b.limiters = append(b.limiters, NamedLimiter{Name: name, Limiter: limiter})
	}
	return b
}

// Build 构建限制器链
func (b *Builder) Build() []Limiter {
	return ChainWithNames(b.limiters...)
}

// BuildWithNames 构建限制器链并返回名称信息
func (b *Builder) BuildWithNames() ([]Limiter, []string) {
	limiters := make([]Limiter, 0, len(b.limiters))
	names := make([]string, 0, len(b.limiters))

	for _, nl := range b.limiters {
		if nl.Limiter != nil {
			limiters = append(limiters, nl.Limiter)
			names = append(names, nl.Name)
		}
	}

	return limiters, names
}
