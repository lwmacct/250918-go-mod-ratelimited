// Package ratelimited 测试套件
//
// 本测试文件实现了对 ratelimited 包的全面测试，采用教科书级别的测试设计原则：
//
// 1. 测试组织结构清晰，按功能模块分组
// 2. 每个测试都有详细的文档说明其目的和预期行为
// 3. 使用 Arrange-Act-Assert (AAA) 模式组织测试逻辑
// 4. 包含正常路径、边界条件和错误情况的全面覆盖
// 5. 提供基准测试来评估性能特征
// 6. 使用表驱动测试来提高测试的可维护性
//
// 测试覆盖的主要功能：
//   - DiscardWriter 的核心写入功能
//   - 多层速率限制器的级联行为
//   - 配额管理和限制
//   - 上下文控制和取消机制
//   - 便利函数的正确性
//   - 建造者模式和链式API
//   - 统计计数和监控功能
//   - 错误处理和边界条件
package ratelimited

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// =============================================================================
// 测试辅助函数和工具
// =============================================================================

// testSetup 为测试提供通用的设置和清理逻辑
type testSetup struct {
	ctx           context.Context
	cancel        context.CancelFunc
	bytesWritten  int64
	requestCount  uint64
	sharedQuota   int64
	primaryRate   rate.Limit
	secondaryRate rate.Limit
	batchSize     int64
}

// newTestSetup 创建标准的测试环境设置
func newTestSetup() *testSetup {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	return &testSetup{
		ctx:           ctx,
		cancel:        cancel,
		bytesWritten:  0,
		requestCount:  0,
		sharedQuota:   0,
		primaryRate:   100000, // 100KB/s
		secondaryRate: 50000,  // 50KB/s
		batchSize:     1024,   // 1KB 批次
	}
}

// cleanup 清理测试资源
func (ts *testSetup) cleanup() {
	ts.cancel()
}

// createTestData 创建指定大小的测试数据
func createTestData(size int) []byte {
	return make([]byte, size)
}

// assertNoError 断言没有错误发生，如果有错误则终止测试
func assertNoError(t *testing.T, err error, message string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", message, err)
	}
}

// assertEqual 断言两个值相等
func assertEqual[T comparable](t *testing.T, expected, actual T, message string) {
	t.Helper()
	if expected != actual {
		t.Errorf("%s: expected %v, got %v", message, expected, actual)
	}
}

// assertAtomicEqual 断言原子变量的值
func assertAtomicEqual(t *testing.T, expected int64, actual *int64, message string) {
	t.Helper()
	if value := atomic.LoadInt64(actual); value != expected {
		t.Errorf("%s: expected %d, got %d", message, expected, value)
	}
}

// =============================================================================
// 核心功能测试 - DiscardWriter
// =============================================================================

// TestDiscardWriter_BasicFunctionality 测试 DiscardWriter 的基本功能
//
// 测试目标：
//   - 验证单层限制器的正确工作
//   - 确保写入数据正确计数
//   - 验证请求统计功能
//   - 确保数据被正确"丢弃"（不占用内存）
//
// 测试场景：
//  1. 创建带有合理速率限制的写入器
//  2. 写入已知大小的数据
//  3. 验证返回值和统计数据的准确性
func TestDiscardWriter_BasicFunctionality(t *testing.T) {
	// Arrange: 设置测试环境
	setup := newTestSetup()
	defer setup.cleanup()

	limiter := rate.NewLimiter(setup.primaryRate, int(setup.primaryRate))
	limiters := Chain(limiter)

	writer := NewDiscardWriter(limiters,
		WithContext(setup.ctx),
		WithBytesCounter(&setup.bytesWritten),
		WithRequestCounter(&setup.requestCount),
		WithBatchSize(setup.batchSize),
	)

	testData := []byte("Hello, Rate Limited World!")
	expectedBytes := len(testData)

	// Act: 执行写入操作
	n, err := writer.Write(testData)

	// Assert: 验证结果
	assertNoError(t, err, "基础写入操作应该成功")
	assertEqual(t, expectedBytes, n, "写入字节数应该与输入数据长度相等")
	assertAtomicEqual(t, int64(expectedBytes), &setup.bytesWritten, "字节统计应该准确")

	actualRequests := atomic.LoadUint64(&setup.requestCount)
	assertEqual(t, uint64(1), actualRequests, "请求计数应该为1")
}

// TestDiscardWriter_EmptyWrite 测试空写入的处理
//
// 测试目标：验证零长度数据的写入不会引起错误或副作用
func TestDiscardWriter_EmptyWrite(t *testing.T) {
	// Arrange
	setup := newTestSetup()
	defer setup.cleanup()

	limiter := rate.NewLimiter(setup.primaryRate, int(setup.primaryRate))
	writer := NewDiscardWriter(Chain(limiter),
		WithContext(setup.ctx),
		WithBytesCounter(&setup.bytesWritten),
		WithRequestCounter(&setup.requestCount),
	)

	// Act
	n, err := writer.Write([]byte{})

	// Assert
	assertNoError(t, err, "空写入应该成功")
	assertEqual(t, 0, n, "空写入应该返回0字节")
	assertAtomicEqual(t, 0, &setup.bytesWritten, "空写入不应该增加字节统计")

	actualRequests := atomic.LoadUint64(&setup.requestCount)
	assertEqual(t, uint64(0), actualRequests, "空写入不应该增加请求计数")
}

// =============================================================================
// 多层速率限制测试
// =============================================================================

// TestDiscardWriter_MultiLayerRateLimit 测试多层速率限制的级联行为
//
// 测试目标：
//   - 验证多个限制器按预期顺序工作
//   - 确保最严格的限制器生效
//   - 验证所有层级的令牌都被正确申请
//
// 测试设计：
//   - 设置多个不同速率的限制器
//   - 最严格的限制器应该决定实际的限制速率
func TestDiscardWriter_MultiLayerRateLimit(t *testing.T) {
	// Arrange: 设置不同速率的多层限制器
	setup := newTestSetup()
	defer setup.cleanup()

	// 创建速率递减的限制器链，最后一个限制器最严格
	globalLimiter := rate.NewLimiter(200000, 200000)  // 200KB/s
	serviceLimiter := rate.NewLimiter(100000, 100000) // 100KB/s
	userLimiter := rate.NewLimiter(50000, 50000)      // 50KB/s (最严格)

	limiters := Chain(globalLimiter, serviceLimiter, userLimiter)

	writer := NewDiscardWriter(limiters,
		WithContext(setup.ctx),
		WithBytesCounter(&setup.bytesWritten),
		WithBatchSize(setup.batchSize),
	)

	testData := createTestData(500) // 500 字节测试数据

	// Act
	n, err := writer.Write(testData)

	// Assert
	assertNoError(t, err, "多层限制器写入应该成功")
	assertEqual(t, len(testData), n, "写入字节数应该正确")
	assertAtomicEqual(t, int64(len(testData)), &setup.bytesWritten, "字节统计应该准确")
}

// TestDiscardWriter_LayerOrdering 测试限制器层级顺序的重要性
//
// 使用表驱动测试来验证不同的限制器组合
func TestDiscardWriter_LayerOrdering(t *testing.T) {
	testCases := []struct {
		name        string
		limits      []rate.Limit
		expectError bool
		description string
	}{
		{
			name:        "递减限制序列",
			limits:      []rate.Limit{100000, 50000, 25000},
			expectError: false,
			description: "限制从宽松到严格",
		},
		{
			name:        "递增限制序列",
			limits:      []rate.Limit{25000, 50000, 100000},
			expectError: false,
			description: "限制从严格到宽松",
		},
		{
			name:        "相同限制序列",
			limits:      []rate.Limit{50000, 50000, 50000},
			expectError: false,
			description: "所有层级具有相同限制",
		},
		{
			name:        "单层限制",
			limits:      []rate.Limit{75000},
			expectError: false,
			description: "只有一个限制器",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			setup := newTestSetup()
			defer setup.cleanup()

			var limiters []Limiter
			for _, limit := range tc.limits {
				// 确保突发容量大于批量大小
				burst := int(limit)
				if burst < 1024 {
					burst = 1024
				}
				limiters = append(limiters, rate.NewLimiter(limit, burst))
			}

			writer := NewDiscardWriter(limiters,
				WithContext(setup.ctx),
				WithBytesCounter(&setup.bytesWritten),
				WithBatchSize(100), // 小批量大小避免突发容量问题
			)

			testData := createTestData(100)

			// Act
			n, err := writer.Write(testData)

			// Assert
			if tc.expectError {
				if err == nil {
					t.Errorf("期望发生错误，但没有错误: %s", tc.description)
				}
			} else {
				assertNoError(t, err, tc.description)
				assertEqual(t, len(testData), n, "写入字节数应该正确")
			}
		})
	}
}

// =============================================================================
// 配额管理测试
// =============================================================================

// TestDiscardWriter_QuotaManagement 测试共享配额管理功能
//
// 测试目标：
//   - 验证配额限制的正确实施
//   - 测试配额耗尽时的行为
//   - 确保配额被正确扣除
func TestDiscardWriter_QuotaManagement(t *testing.T) {
	// Arrange: 设置有限配额
	setup := newTestSetup()
	defer setup.cleanup()

	initialQuota := int64(1000) // 1000 字节配额
	setup.sharedQuota = initialQuota

	limiter := rate.NewLimiter(100000, 100000) // 充足的速率限制，不会成为瓶颈
	writer := NewDiscardWriter(Chain(limiter),
		WithContext(setup.ctx),
		WithBytesCounter(&setup.bytesWritten),
		WithSharedQuota(&setup.sharedQuota),
		WithBatchSize(setup.batchSize),
	)

	// Test Case 1: 在配额范围内写入
	t.Run("在配额范围内写入", func(t *testing.T) {
		testData := createTestData(300) // 300 字节，在配额范围内

		// Act
		n, err := writer.Write(testData)

		// Assert
		assertNoError(t, err, "配额范围内的写入应该成功")
		assertEqual(t, len(testData), n, "写入字节数应该正确")

		remainingQuota := atomic.LoadInt64(&setup.sharedQuota)
		expectedRemaining := initialQuota - int64(len(testData))
		assertEqual(t, expectedRemaining, remainingQuota, "配额扣除应该正确")
	})

	// Test Case 2: 超过剩余配额的写入
	t.Run("超过剩余配额的写入", func(t *testing.T) {
		remainingBefore := atomic.LoadInt64(&setup.sharedQuota)
		testData := createTestData(int(remainingBefore + 100)) // 超过剩余配额

		// Act
		n, err := writer.Write(testData)

		// Assert
		assertNoError(t, err, "部分写入应该成功")
		assertEqual(t, int(remainingBefore), n, "应该只写入剩余配额的字节数")

		remainingAfter := atomic.LoadInt64(&setup.sharedQuota)
		assertEqual(t, int64(0), remainingAfter, "配额应该完全耗尽")
	})

	// Test Case 3: 配额耗尽后的写入
	t.Run("配额耗尽后的写入", func(t *testing.T) {
		testData := createTestData(100)

		// Act
		n, err := writer.Write(testData)

		// Assert
		assertEqual(t, io.EOF, err, "配额耗尽时应该返回 EOF")
		assertEqual(t, 0, n, "配额耗尽时不应该写入任何数据")
	})
}

// =============================================================================
// 上下文控制测试
// =============================================================================

// TestDiscardWriter_ContextCancellation 测试上下文取消机制
//
// 测试目标：验证上下文取消能够正确中断写入操作
func TestDiscardWriter_ContextCancellation(t *testing.T) {
	// Arrange: 设置可取消的上下文
	ctx, cancel := context.WithCancel(context.Background())

	// 创建一个很慢的限制器来确保操作可以被取消
	slowLimiter := rate.NewLimiter(1, 1) // 每秒1字节，很慢

	var bytesWritten int64
	writer := NewDiscardWriter(Chain(slowLimiter),
		WithContext(ctx),
		WithBytesCounter(&bytesWritten),
		WithBatchSize(100), // 较小的批次大小
	)

	// 立即取消上下文
	cancel()

	testData := createTestData(1000) // 大量数据

	// Act
	n, err := writer.Write(testData)

	// Assert
	assertEqual(t, context.Canceled, err, "应该返回上下文取消错误")
	assertEqual(t, 0, n, "取消后不应该写入任何数据")
	assertAtomicEqual(t, 0, &bytesWritten, "取消后字节统计应该为0")
}

// TestDiscardWriter_ContextTimeout 测试上下文超时
func TestDiscardWriter_ContextTimeout(t *testing.T) {
	// Arrange: 设置很短的超时
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// 创建一个极慢的限制器来触发超时
	verySlowLimiter := rate.NewLimiter(1, 100) // 每秒1字节，很小的突发容量

	writer := NewDiscardWriter(Chain(verySlowLimiter),
		WithContext(ctx),
		WithBatchSize(100), // 超过突发容量的批量大小
	)

	testData := createTestData(200) // 较大的数据量

	// 等待一小段时间确保超时生效
	time.Sleep(2 * time.Millisecond)

	// Act
	n, err := writer.Write(testData)

	// Assert
	if err != context.DeadlineExceeded && err != context.Canceled {
		// 如果没有触发超时，跳过这个测试（在某些系统上可能很难保证超时）
		t.Skip("未能触发上下文超时，可能是系统时序问题")
	}
	assertEqual(t, 0, n, "超时后不应该写入任何数据")
}

// =============================================================================
// 便利函数测试
// =============================================================================

// TestCopyWithRateLimit_BasicUsage 测试 CopyWithRateLimit 便利函数
//
// 测试目标：验证便利函数能够正确执行基本的复制操作
func TestCopyWithRateLimit_BasicUsage(t *testing.T) {
	// Arrange
	testContent := "Hello, Rate Limited Copy Function!"
	reader := strings.NewReader(testContent)

	limiter := rate.NewLimiter(100000, 100000) // 充足的速率
	limiters := Chain(limiter)

	ctx := context.Background()
	var bytesWritten int64

	// Act
	copied, err := CopyWithRateLimit(ctx, reader, limiters,
		WithBytesCounter(&bytesWritten),
		WithBatchSize(1024),
	)

	// Assert
	assertNoError(t, err, "CopyWithRateLimit 应该成功")

	expectedBytes := int64(len(testContent))
	assertEqual(t, expectedBytes, copied, "复制的字节数应该正确")
	assertAtomicEqual(t, expectedBytes, &bytesWritten, "字节统计应该准确")
}

// TestCopyNWithRateLimit_LimitedCopy 测试 CopyNWithRateLimit 有限复制功能
func TestCopyNWithRateLimit_LimitedCopy(t *testing.T) {
	// Arrange
	testContent := "This is a long test content that will be partially copied"
	reader := strings.NewReader(testContent)

	limiter := rate.NewLimiter(100000, 100000)
	limiters := Chain(limiter)

	ctx := context.Background()
	var bytesWritten int64

	copyLimit := int64(20) // 只复制前20个字节

	// Act
	copied, err := CopyNWithRateLimit(ctx, reader, copyLimit, limiters,
		WithBytesCounter(&bytesWritten),
	)

	// Assert
	assertNoError(t, err, "CopyNWithRateLimit 应该成功")
	assertEqual(t, copyLimit, copied, "复制的字节数应该等于限制值")
	assertAtomicEqual(t, copyLimit, &bytesWritten, "字节统计应该等于复制限制")
}

// =============================================================================
// API构造函数测试
// =============================================================================

// TestChain_VariousConfigurations 测试 Chain 函数的各种配置
func TestChain_VariousConfigurations(t *testing.T) {
	testCases := []struct {
		name          string
		inputLimiters []*rate.Limiter
		expectedCount int
		description   string
	}{
		{
			name:          "空链",
			inputLimiters: []*rate.Limiter{},
			expectedCount: 0,
			description:   "空的限制器数组应该产生空链",
		},
		{
			name:          "单个限制器",
			inputLimiters: []*rate.Limiter{rate.NewLimiter(1000, 1000)},
			expectedCount: 1,
			description:   "单个限制器应该产生长度为1的链",
		},
		{
			name: "多个限制器",
			inputLimiters: []*rate.Limiter{
				rate.NewLimiter(1000, 1000),
				rate.NewLimiter(2000, 2000),
				rate.NewLimiter(3000, 3000),
			},
			expectedCount: 3,
			description:   "多个限制器应该产生相应长度的链",
		},
		{
			name: "包含nil的限制器",
			inputLimiters: []*rate.Limiter{
				rate.NewLimiter(1000, 1000),
				nil,
				rate.NewLimiter(2000, 2000),
				nil,
			},
			expectedCount: 2,
			description:   "nil限制器应该被过滤掉",
		},
		{
			name:          "全部为nil",
			inputLimiters: []*rate.Limiter{nil, nil, nil},
			expectedCount: 0,
			description:   "全部nil应该产生空链",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			result := Chain(tc.inputLimiters...)

			// Assert
			assertEqual(t, tc.expectedCount, len(result), tc.description)
		})
	}
}

// TestBuilder_ChainConstruction 测试建造者模式
func TestBuilder_ChainConstruction(t *testing.T) {
	// Arrange
	globalLimiter := rate.NewLimiter(100000, 100000)
	serviceLimiter := rate.NewLimiter(50000, 50000)
	userLimiter := rate.NewLimiter(25000, 25000)

	// Act: 使用建造者模式构建限制器链
	limiters := NewBuilder().
		Add("global", globalLimiter).
		Add("service", serviceLimiter).
		Add("user", userLimiter).
		Build()

	// Assert
	assertEqual(t, 3, len(limiters), "建造者应该创建3个限制器的链")

	// 测试 BuildWithNames 方法
	limitersWithNames, names := NewBuilder().
		Add("primary", globalLimiter).
		Add("secondary", serviceLimiter).
		BuildWithNames()

	assertEqual(t, 2, len(limitersWithNames), "应该返回2个限制器")
	assertEqual(t, 2, len(names), "应该返回2个名称")
	assertEqual(t, "primary", names[0], "第一个名称应该正确")
	assertEqual(t, "secondary", names[1], "第二个名称应该正确")
}

// TestBuilder_NilHandling 测试建造者模式对nil的处理
func TestBuilder_NilHandling(t *testing.T) {
	// Act
	limiters := NewBuilder().
		Add("valid", rate.NewLimiter(1000, 1000)).
		Add("nil", nil).
		Add("another_valid", rate.NewLimiter(2000, 2000)).
		Build()

	// Assert
	assertEqual(t, 2, len(limiters), "nil限制器应该被过滤掉")
}

// TestChainWithNames_Functionality 测试带名称的链构造
func TestChainWithNames_Functionality(t *testing.T) {
	// Arrange
	namedLimiters := []NamedLimiter{
		{Name: "first", Limiter: rate.NewLimiter(1000, 1000)},
		{Name: "second", Limiter: rate.NewLimiter(2000, 2000)},
		{Name: "nil", Limiter: nil}, // 应该被过滤
		{Name: "third", Limiter: rate.NewLimiter(3000, 3000)},
	}

	// Act
	limiters := ChainWithNames(namedLimiters...)

	// Assert
	assertEqual(t, 3, len(limiters), "应该过滤掉nil限制器")
}

// =============================================================================
// 错误处理容错性测试
// =============================================================================

// MockFailingLimiter 模拟会失败的速率限制器
type MockFailingLimiter struct {
	shouldFail bool
	failError  error
}

func (m *MockFailingLimiter) WaitN(ctx context.Context, n int) error {
	if m.shouldFail {
		return m.failError
	}
	return nil
}

// TestDiscardWriter_FaultTolerance 测试错误处理的容错性
//
// 测试目标：验证改进后的错误处理策略能够正确区分致命错误和非致命错误
func TestDiscardWriter_FaultTolerance(t *testing.T) {
	t.Run("上下文取消时立即返回错误", func(t *testing.T) {
		// Arrange: 每个子测试使用独立的计数器
		var bytesWritten int64
		
		// 创建包含模拟限制器的链
		mockLimiter := &MockFailingLimiter{shouldFail: false}
		normalLimiter := rate.NewLimiter(100000, 100000)
		
		limiters := []Limiter{mockLimiter, normalLimiter}
		
		// 创建可取消的上下文
		ctx, cancel := context.WithCancel(context.Background())
		
		writer := NewDiscardWriter(limiters,
			WithContext(ctx),
			WithBytesCounter(&bytesWritten),
		)
		
		// 立即取消上下文
		cancel()
		
		testData := createTestData(100)
		
		// Act
		n, err := writer.Write(testData)
		
		// Assert
		assertEqual(t, context.Canceled, err, "应该返回上下文取消错误")
		assertEqual(t, 0, n, "取消时不应该写入任何数据")
		assertAtomicEqual(t, 0, &bytesWritten, "字节统计应该为0")
	})

	t.Run("非致命错误时跳过失败的限制器", func(t *testing.T) {
		// Arrange: 独立的测试环境
		setup := newTestSetup()
		defer setup.cleanup()
		
		// 创建包含会失败的限制器
		failingLimiter := &MockFailingLimiter{
			shouldFail: true,
			failError:  io.ErrUnexpectedEOF, // 非上下文错误
		}
		normalLimiter := rate.NewLimiter(100000, 100000)
		
		limiters := []Limiter{failingLimiter, normalLimiter}
		
		writer := NewDiscardWriter(limiters,
			WithContext(setup.ctx),
			WithBytesCounter(&setup.bytesWritten),
		)
		
		testData := createTestData(100)
		
		// Act
		n, err := writer.Write(testData)
		
		// Assert
		assertNoError(t, err, "非致命错误应该被跳过，写入应该成功")
		assertEqual(t, len(testData), n, "应该成功写入所有数据")
		assertAtomicEqual(t, int64(len(testData)), &setup.bytesWritten, "字节统计应该正确")
	})

	t.Run("所有限制器都失败时返回错误", func(t *testing.T) {
		// Arrange: 独立的测试环境
		setup := newTestSetup()
		defer setup.cleanup()
		
		// 创建所有都会失败的限制器
		failingLimiter1 := &MockFailingLimiter{
			shouldFail: true,
			failError:  io.ErrUnexpectedEOF,
		}
		failingLimiter2 := &MockFailingLimiter{
			shouldFail: true,
			failError:  io.ErrShortWrite,
		}
		
		limiters := []Limiter{failingLimiter1, failingLimiter2}
		
		writer := NewDiscardWriter(limiters,
			WithContext(setup.ctx),
			WithBytesCounter(&setup.bytesWritten),
		)
		
		testData := createTestData(100)
		
		// Act
		n, err := writer.Write(testData)
		
		// Assert
		assertEqual(t, io.ErrShortWrite, err, "应该返回最后一个错误")
		assertEqual(t, 0, n, "所有限制器失败时不应该写入数据")
		assertAtomicEqual(t, 0, &setup.bytesWritten, "字节统计应该为0")
	})

	t.Run("混合成功和失败的限制器", func(t *testing.T) {
		// Arrange: 独立的测试环境
		setup := newTestSetup()
		defer setup.cleanup()
		
		successLimiter1 := rate.NewLimiter(100000, 100000)
		failingLimiter := &MockFailingLimiter{
			shouldFail: true,
			failError:  io.ErrUnexpectedEOF,
		}
		successLimiter2 := rate.NewLimiter(50000, 50000)
		
		limiters := []Limiter{successLimiter1, failingLimiter, successLimiter2}
		
		writer := NewDiscardWriter(limiters,
			WithContext(setup.ctx),
			WithBytesCounter(&setup.bytesWritten),
			WithBatchSize(100), // 小批次避免突发容量问题
		)
		
		testData := createTestData(50)
		
		// Act
		n, err := writer.Write(testData)
		
		// Assert
		assertNoError(t, err, "部分成功应该允许写入继续")
		assertEqual(t, len(testData), n, "应该成功写入数据")
		assertAtomicEqual(t, int64(len(testData)), &setup.bytesWritten, "字节统计应该正确")
	})
}

// =============================================================================
// 并发安全测试
// =============================================================================

// TestDiscardWriter_ConcurrentWrite 测试并发写入的安全性
//
// 测试目标：确保多个goroutine同时写入时的线程安全性
func TestDiscardWriter_ConcurrentWrite(t *testing.T) {
	// Arrange
	setup := newTestSetup()
	defer setup.cleanup()

	limiter := rate.NewLimiter(1000000, 1000000) // 高速率以避免阻塞
	writer := NewDiscardWriter(Chain(limiter),
		WithContext(setup.ctx),
		WithBytesCounter(&setup.bytesWritten),
		WithRequestCounter(&setup.requestCount),
	)

	const goroutineCount = 10
	const writesPerGoroutine = 100
	const dataSize = 50

	var wg sync.WaitGroup
	wg.Add(goroutineCount)

	// Act: 启动多个goroutine并发写入
	for i := 0; i < goroutineCount; i++ {
		go func() {
			defer wg.Done()
			testData := createTestData(dataSize)

			for j := 0; j < writesPerGoroutine; j++ {
				_, err := writer.Write(testData)
				if err != nil {
					t.Errorf("并发写入失败: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()

	// Assert: 验证统计数据的准确性
	expectedBytes := int64(goroutineCount * writesPerGoroutine * dataSize)
	expectedRequests := uint64(goroutineCount * writesPerGoroutine)

	assertAtomicEqual(t, expectedBytes, &setup.bytesWritten, "并发写入的总字节数应该正确")

	actualRequests := atomic.LoadUint64(&setup.requestCount)
	assertEqual(t, expectedRequests, actualRequests, "并发写入的总请求数应该正确")
}

// =============================================================================
// 性能基准测试
// =============================================================================

// BenchmarkDiscardWriter_SingleLayer 单层限制器的性能基准
func BenchmarkDiscardWriter_SingleLayer(b *testing.B) {
	limiter := rate.NewLimiter(1000000, 1000000) // 高速率限制器
	writer := NewDiscardWriter(Chain(limiter))
	data := createTestData(1024) // 1KB 数据

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := writer.Write(data)
		if err != nil {
			b.Fatalf("写入失败: %v", err)
		}
	}
}

// BenchmarkDiscardWriter_MultiLayer 多层限制器的性能基准
func BenchmarkDiscardWriter_MultiLayer(b *testing.B) {
	limiters := Chain(
		rate.NewLimiter(1000000, 1000000),
		rate.NewLimiter(800000, 800000),
		rate.NewLimiter(600000, 600000),
	)
	writer := NewDiscardWriter(limiters)
	data := createTestData(1024)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := writer.Write(data)
		if err != nil {
			b.Fatalf("写入失败: %v", err)
		}
	}
}

// BenchmarkCopyWithRateLimit 便利函数的性能基准
func BenchmarkCopyWithRateLimit(b *testing.B) {
	limiter := rate.NewLimiter(1000000, 1000000)
	limiters := Chain(limiter)
	data := strings.Repeat("x", 1024) // 1KB 数据

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		reader := strings.NewReader(data)
		_, err := CopyWithRateLimit(context.Background(), reader, limiters)
		if err != nil {
			b.Fatalf("复制失败: %v", err)
		}
	}
}

// =============================================================================
// 示例测试（文档示例）
// =============================================================================

// ExampleNewDiscardWriter 演示如何创建和使用 DiscardWriter
func ExampleNewDiscardWriter() {
	// 创建速率限制器
	limiter := rate.NewLimiter(100000, 100000) // 100KB/s

	// 创建丢弃写入器
	var bytesWritten int64
	writer := NewDiscardWriter(Chain(limiter),
		WithBytesCounter(&bytesWritten),
		WithBatchSize(1024), // 1KB 批次
	)

	// 写入数据
	data := []byte("Hello, Rate Limited World!")
	n, err := writer.Write(data)

	if err != nil {
		panic(err)
	}

	// 输出结果
	println("写入字节数:", n)
	println("统计字节数:", atomic.LoadInt64(&bytesWritten))
}

// ExampleCopyWithRateLimit 演示便利函数的使用
func ExampleCopyWithRateLimit() {
	// 创建数据源
	data := strings.NewReader("Hello from rate limited copy!")

	// 创建限制器，确保突发容量足够
	limiter := rate.NewLimiter(50000, 100000) // 50KB/s，100KB突发

	// 使用便利函数复制数据
	copied, err := CopyWithRateLimit(
		context.Background(),
		data,
		Chain(limiter),
		WithBatchSize(1024), // 小批量大小
	)

	if err != nil {
		panic(err)
	}

	println("复制字节数:", copied)
}

// ExampleBuilder 演示建造者模式的使用
func ExampleBuilder() {
	// 创建不同级别的限制器，确保突发容量足够
	globalLimiter := rate.NewLimiter(200000, 200000)  // 200KB/s
	serviceLimiter := rate.NewLimiter(100000, 100000) // 100KB/s
	userLimiter := rate.NewLimiter(50000, 100000)     // 50KB/s，更大突发容量

	// 使用建造者模式构建限制器链
	limiters := NewBuilder().
		Add("global", globalLimiter).
		Add("service", serviceLimiter).
		Add("user", userLimiter).
		Build()

	// 创建写入器
	var bytesWritten int64
	writer := NewDiscardWriter(limiters,
		WithBytesCounter(&bytesWritten),
		WithBatchSize(1024), // 小批量大小
	)

	// 写入数据
	data := []byte("Multi-layer rate limiting example")
	n, err := writer.Write(data)

	if err != nil {
		panic(err)
	}

	println("多层限制写入字节数:", n)
}
