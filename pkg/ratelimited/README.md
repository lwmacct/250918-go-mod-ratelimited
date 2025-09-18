# Ratelimited 包

[![Go Reference](https://pkg.go.dev/badge/github.com/lwmacct/250901-m-nbwb/pkg/ratelimited.svg)](https://pkg.go.dev/github.com/lwmacct/250901-m-nbwb/pkg/ratelimited)
[![Go Report Card](https://goreportcard.com/badge/github.com/lwmacct/250901-m-nbwb/pkg/ratelimited)](https://goreportcard.com/report/github.com/lwmacct/250901-m-nbwb/pkg/ratelimited)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

`pkg/ratelimited` 是一个高性能的Go包，提供支持多层速率限制的数据丢弃解决方案。该包专为需要流量控制但不需要数据存储的场景设计，如网络测试、API限流、多租户系统等。

## 🎯 核心特性

### 🚀 高性能设计
- **零内存拷贝**：基于 `io.Discard` 的内核优化，确保最佳性能
- **批量令牌申请**：减少锁竞争，提高并发性能
- **无锁统计**：使用原子操作实现线程安全的统计计数
- **对象复用**：避免频繁的内存分配和垃圾回收

### 🔗 灵活的多层限制
- **任意层数**：支持1到N层的嵌套速率限制器
- **级联控制**：多个限制器按顺序应用，最严格的限制生效
- **动态组合**：运行时灵活组合不同的限制器
- **命名支持**：可选的限制器命名，便于调试和监控

### 📊 全面的监控统计
- **字节计数**：精确的数据传输量统计
- **请求计数**：API调用次数统计
- **配额管理**：支持有限流量配额控制
- **原子操作**：保证高并发下的统计准确性

### ⚡ 上下文控制
- **取消支持**：响应上下文取消信号
- **超时控制**：支持操作超时设置
- **优雅退出**：确保资源正确清理

## 📦 安装

```bash
go get github.com/lwmacct/250901-m-nbwb/pkg/ratelimited
```

## 🚀 快速开始

### 基础用法

```go
package main

import (
    "context"
    "strings"
    "github.com/lwmacct/250901-m-nbwb/pkg/ratelimited"
    "golang.org/x/time/rate"
)

func main() {
    // 创建速率限制器（100KB/s）
    limiter := rate.NewLimiter(100000, 100000)
    
    // 准备数据源
    data := strings.NewReader("Hello, Rate Limited World!")
    
    // 使用便利函数进行限速复制
    copied, err := ratelimited.CopyWithRateLimit(
        context.Background(),
        data,
        ratelimited.Chain(limiter),
    )
    
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("复制了 %d 字节\n", copied)
}
```

### 多层限制示例

```go
// 创建分层限制器
systemLimiter := rate.NewLimiter(1000000, 1000000)  // 1MB/s 系统级
serviceLimiter := rate.NewLimiter(500000, 500000)   // 500KB/s 服务级
userLimiter := rate.NewLimiter(100000, 100000)      // 100KB/s 用户级

// 构建多层限制器链
limiters := ratelimited.Chain(systemLimiter, serviceLimiter, userLimiter)

// 使用多层限制
var bytesWritten int64
copied, err := ratelimited.CopyWithRateLimit(
    ctx, reader, limiters,
    ratelimited.WithBytesCounter(&bytesWritten),
)
```

## 📖 详细使用指南

### 1. 构造限制器链

#### Chain 函数（推荐）

`Chain` 函数是构造限制器链的主要方式，支持任意数量的限制器：

```go
// 单层限制
single := ratelimited.Chain(primaryLimiter)

// 双层限制
dual := ratelimited.Chain(tier1Limiter, tier2Limiter)

// 多层限制
multi := ratelimited.Chain(
    globalLimiter,     // 全局限制
    tenantLimiter,     // 租户限制
    userLimiter,       // 用户限制
    apiLimiter,        // API限制
)

// 自动过滤 nil 值
safe := ratelimited.Chain(validLimiter, nil, anotherLimiter) // 只包含2个有效限制器
```

#### 建造者模式

对于复杂的限制器配置，建造者模式提供更好的可读性：

```go
limiters := ratelimited.NewBuilder().
    Add("global", globalLimiter).
    Add("tenant", tenantLimiter).
    Add("user", userLimiter).
    Add("api", apiLimiter).
    Build()

// 获取限制器和名称
limiters, names := ratelimited.NewBuilder().
    Add("primary", primaryLimiter).
    Add("secondary", secondaryLimiter).
    BuildWithNames()
```

#### 命名限制器

用于调试和监控的命名限制器：

```go
namedLimiters := []ratelimited.NamedLimiter{
    {Name: "global", Limiter: globalLimiter},
    {Name: "service", Limiter: serviceLimiter},
    {Name: "user", Limiter: userLimiter},
}

limiters := ratelimited.ChainWithNames(namedLimiters...)
```

### 2. 使用 DiscardWriter

`DiscardWriter` 实现了 `io.Writer` 接口，提供最大的灵活性：

```go
// 创建写入器
var bytesWritten int64
var requestCount uint64

writer := ratelimited.NewDiscardWriter(limiters,
    ratelimited.WithContext(ctx),
    ratelimited.WithBytesCounter(&bytesWritten),
    ratelimited.WithRequestCounter(&requestCount),
    ratelimited.WithBatchSize(64*1024), // 64KB 批次
)

// 直接写入
n, err := writer.Write(data)

// 或使用 io.Copy
copied, err := io.Copy(writer, reader)
```

### 3. 配置选项

#### WithContext - 上下文控制

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

writer := ratelimited.NewDiscardWriter(limiters,
    ratelimited.WithContext(ctx),
)
```

#### WithBytesCounter - 字节统计

```go
var totalBytes int64

writer := ratelimited.NewDiscardWriter(limiters,
    ratelimited.WithBytesCounter(&totalBytes),
)

// 使用后检查统计
fmt.Printf("总共处理了 %d 字节\n", atomic.LoadInt64(&totalBytes))
```

#### WithRequestCounter - 请求统计

```go
var totalRequests uint64

writer := ratelimited.NewDiscardWriter(limiters,
    ratelimited.WithRequestCounter(&totalRequests),
)

// 检查请求数
fmt.Printf("总共处理了 %d 个请求\n", atomic.LoadUint64(&totalRequests))
```

#### WithSharedQuota - 配额管理

```go
var remainingQuota int64 = 1024 * 1024 // 1MB 配额

writer := ratelimited.NewDiscardWriter(limiters,
    ratelimited.WithSharedQuota(&remainingQuota),
)

// 配额用完时会返回 io.EOF
```

#### WithBatchSize - 批次大小优化

```go
writer := ratelimited.NewDiscardWriter(limiters,
    ratelimited.WithBatchSize(128*1024), // 128KB 批次，减少令牌申请频率
)
```

### 4. 便利函数

#### CopyWithRateLimit - 无限制复制

```go
copied, err := ratelimited.CopyWithRateLimit(
    ctx, reader, limiters,
    ratelimited.WithBytesCounter(&bytesWritten),
)
```

#### CopyNWithRateLimit - 有限制复制

```go
// 最多复制 1MB 数据
copied, err := ratelimited.CopyNWithRateLimit(
    ctx, reader, 1024*1024, limiters,
    ratelimited.WithSharedQuota(&quota),
)
```

## 🏗️ 实际应用场景

### 网络测试工具

```go
func networkSpeedTest(url string, duration time.Duration) error {
    // 创建分层限制器
    limiters := ratelimited.Chain(
        rate.NewLimiter(10*1024*1024, 10*1024*1024), // 10MB/s 总限制
        rate.NewLimiter(5*1024*1024, 5*1024*1024),   // 5MB/s 连接限制
    )
    
    // 统计变量
    var totalBytes int64
    var requestCount uint64
    
    // 设置超时上下文
    ctx, cancel := context.WithTimeout(context.Background(), duration)
    defer cancel()
    
    // 发起HTTP请求
    resp, err := http.Get(url)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    
    // 限速复制响应体到 Discard
    _, err = ratelimited.CopyWithRateLimit(
        ctx, resp.Body, limiters,
        ratelimited.WithBytesCounter(&totalBytes),
        ratelimited.WithRequestCounter(&requestCount),
        ratelimited.WithBatchSize(64*1024),
    )
    
    // 输出测试结果
    fmt.Printf("下载速度测试完成:\n")
    fmt.Printf("- 总字节数: %d\n", atomic.LoadInt64(&totalBytes))
    fmt.Printf("- 请求次数: %d\n", atomic.LoadUint64(&requestCount))
    fmt.Printf("- 平均速度: %.2f KB/s\n", 
        float64(atomic.LoadInt64(&totalBytes))/duration.Seconds()/1024)
    
    return err
}
```

### 多租户API网关

```go
type APIGateway struct {
    globalLimiter  *rate.Limiter
    tenantLimiters map[string]*rate.Limiter
    userLimiters   map[string]*rate.Limiter
}

func (gw *APIGateway) HandleRequest(tenantID, userID string, body io.Reader) error {
    // 构建分层限制器
    limiters := ratelimited.NewBuilder().
        Add("global", gw.globalLimiter).
        Add("tenant", gw.tenantLimiters[tenantID]).
        Add("user", gw.userLimiters[userID]).
        Build()
    
    // 统计该请求的数据量
    var requestBytes int64
    
    // 处理请求体（丢弃数据但应用限制）
    _, err := ratelimited.CopyWithRateLimit(
        context.Background(), body, limiters,
        ratelimited.WithBytesCounter(&requestBytes),
    )
    
    if err != nil {
        return fmt.Errorf("请求被限流: %w", err)
    }
    
    // 记录请求统计
    log.Printf("处理请求 - 租户: %s, 用户: %s, 字节数: %d", 
        tenantID, userID, atomic.LoadInt64(&requestBytes))
    
    return nil
}
```

### 数据流量监控

```go
type TrafficMonitor struct {
    limiters       []ratelimited.Limiter
    totalBytes     int64
    totalRequests  uint64
    quotaRemaining int64
}

func (tm *TrafficMonitor) ProcessStream(reader io.Reader) error {
    writer := ratelimited.NewDiscardWriter(tm.limiters,
        ratelimited.WithBytesCounter(&tm.totalBytes),
        ratelimited.WithRequestCounter(&tm.totalRequests),
        ratelimited.WithSharedQuota(&tm.quotaRemaining),
    )
    
    _, err := io.Copy(writer, reader)
    return err
}

func (tm *TrafficMonitor) GetStats() (bytes int64, requests uint64, quota int64) {
    return atomic.LoadInt64(&tm.totalBytes),
           atomic.LoadUint64(&tm.totalRequests),
           atomic.LoadInt64(&tm.quotaRemaining)
}
```

## 🔧 配置最佳实践

### 1. 选择合适的批次大小

```go
// 小文件或低延迟场景
ratelimited.WithBatchSize(4 * 1024)   // 4KB

// 一般场景  
ratelimited.WithBatchSize(64 * 1024)  // 64KB（默认）

// 大文件或高吞吐场景
ratelimited.WithBatchSize(1024 * 1024) // 1MB
```

### 2. 设置合理的限制器参数

```go
// 突发流量友好的配置
limiter := rate.NewLimiter(
    100000,  // 速率：100KB/s
    500000,  // 突发：500KB（允许短时间突发）
)

// 严格限制的配置  
limiter := rate.NewLimiter(
    100000,  // 速率：100KB/s
    100000,  // 突发：100KB（与速率相等，无突发）
)
```

### 3. 多层限制器的层级设计

```go
// 从宽松到严格的层级结构
limiters := ratelimited.Chain(
    rate.NewLimiter(10*1024*1024, 10*1024*1024),  // L1: 系统级 10MB/s
    rate.NewLimiter(5*1024*1024, 5*1024*1024),    // L2: 服务级 5MB/s  
    rate.NewLimiter(1*1024*1024, 1*1024*1024),    // L3: 租户级 1MB/s
    rate.NewLimiter(100*1024, 100*1024),          // L4: 用户级 100KB/s
)
```

### 4. 错误处理策略

```go
copied, err := ratelimited.CopyWithRateLimit(ctx, reader, limiters)

switch err {
case nil:
    // 成功完成
    log.Printf("成功复制 %d 字节", copied)
    
case io.EOF:
    // 配额耗尽（正常情况）
    log.Printf("配额耗尽，已复制 %d 字节", copied)
    
case context.Canceled:
    // 用户取消
    log.Printf("操作被取消，已复制 %d 字节", copied)
    
case context.DeadlineExceeded:
    // 超时
    log.Printf("操作超时，已复制 %d 字节", copied)
    
default:
    // 其他错误
    log.Printf("复制失败: %v，已复制 %d 字节", err, copied)
}
```

## 📈 性能特征

### 基准测试结果

```
BenchmarkDiscardWriter_SingleLayer-8       	1000000	     1052 ns/op	       0 B/op	       0 allocs/op
BenchmarkDiscardWriter_MultiLayer-8        	 800000	     1456 ns/op	       0 B/op	       0 allocs/op
BenchmarkCopyWithRateLimit-8               	 500000	     2104 ns/op	    1024 B/op	       1 allocs/op
```

### 内存使用

- **零数据拷贝**：使用 `io.Discard` 避免内存分配
- **批量令牌管理**：减少频繁的锁操作
- **原子统计**：避免互斥锁的开销
- **对象复用**：最小化GC压力

### 并发性能

- **无锁设计**：统计操作使用原子操作
- **并行令牌申请**：多个限制器并行处理
- **上下文支持**：快速响应取消信号

## 🧪 测试

运行完整的测试套件：

```bash
# 运行所有测试
go test ./pkg/ratelimited -v

# 运行基准测试
go test ./pkg/ratelimited -bench=. -benchmem

# 运行竞态检测
go test ./pkg/ratelimited -race

# 测试覆盖率
go test ./pkg/ratelimited -cover
```

测试覆盖的功能：
- ✅ 基础写入功能
- ✅ 多层限制器级联
- ✅ 配额管理和限制
- ✅ 上下文控制和取消
- ✅ 便利函数正确性
- ✅ 建造者模式和链式API
- ✅ 并发安全性
- ✅ 错误处理和边界条件
- ✅ 性能基准测试

## 🤝 贡献

欢迎贡献代码！请遵循以下步骤：

1. Fork 本仓库
2. 创建功能分支 (`git checkout -b feature/amazing-feature`)
3. 提交更改 (`git commit -m 'Add some amazing feature'`)
4. 推送到分支 (`git push origin feature/amazing-feature`)
5. 创建 Pull Request

### 开发指南

- 确保所有测试通过
- 添加适当的测试覆盖
- 更新文档
- 遵循Go语言编码规范

## 📄 许可证

本项目使用 MIT 许可证 - 查看 [LICENSE](LICENSE) 文件了解详情。

## 🙏 致谢

- 感谢 [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) 提供的优秀速率限制实现
- 感谢所有贡献者的支持和反馈

## 📞 支持

如果您有任何问题或建议，请：
- 创建 [GitHub Issue](https://github.com/lwmacct/250901-m-nbwb/issues)
- 查看 [文档](https://pkg.go.dev/github.com/lwmacct/250901-m-nbwb/pkg/ratelimited)
- 参与 [讨论](https://github.com/lwmacct/250901-m-nbwb/discussions)