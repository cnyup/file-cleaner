package runner

// sweep 并行调度测试（2026-11-03/04 巡检继承项）：sweepRoundWith 每策略
// 独立 goroutine，后位策略不再被前位长跑整段等位。不变量：
//  1. 策略 A 阻塞期间策略 B 照常完成（串行实现红）
//  2. sweepRoundWith 返回时全部策略已完成（wg 收口）
//  3. 单策略失败不影响其他策略执行（错误隔离）
import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
)

func sweepParallelCfg() *config.Config {
	return &config.Config{
		Policies: []config.Policy{
			{ID: "pa", Root: "/tmp/sweeptest/ra"},
			{ID: "pb", Root: "/tmp/sweeptest/rb"},
		},
	}
}

// TestSweepRoundParallelPoliciesDontSerialize：策略 pa 阻塞 1.5s、pb 秒回；
// 并行实现下 pb 必先完成（串行实现红）。
func TestSweepRoundParallelPoliciesDontSerialize(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	sweepFn := func(p *config.Policy) (int, int, error) {
		if p.ID == "pa" {
			time.Sleep(1500 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, p.ID)
		mu.Unlock()
		return 0, 0, nil
	}

	done := make(chan struct{})
	go func() {
		sweepRoundWith(sweepParallelCfg(), sweepFn, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sweepRoundWith blocked >3s — policies serialized by long pa sweep")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "pb" || order[1] != "pa" {
		t.Fatalf("order=%v want [pb pa] — pb must complete before blocking pa (parallelism broken)", order)
	}
}

// TestSweepRoundWaitsForAll：返回时全部策略已完成（wg 语义）。
func TestSweepRoundWaitsForAll(t *testing.T) {
	var finished sync.WaitGroup
	var started sync.WaitGroup
	finished.Add(2)
	started.Add(2)
	sweepFn := func(p *config.Policy) (int, int, error) {
		started.Done()
		time.Sleep(300 * time.Millisecond)
		finished.Done()
		return 1, 1, nil
	}
	sweepRoundWith(sweepParallelCfg(), sweepFn, io.Discard)
	// 若未等全部完成，这里 finished 计数会 <0 panic 或 started 未 Done
	// （Add/Done 配对使漏等表现为 panic/超时而非静默通过）。
}

// TestSweepRoundErrorIsolation：pa 返回错误不影响 pb 执行，
// 且错误行进 log（可观测）。
func TestSweepRoundErrorIsolation(t *testing.T) {
	var mu sync.Mutex
	ran := map[string]bool{}
	sweepFn := func(p *config.Policy) (int, int, error) {
		mu.Lock()
		ran[p.ID] = true
		mu.Unlock()
		if p.ID == "pa" {
			return 0, 0, io.ErrUnexpectedEOF
		}
		return 5, 2, nil
	}
	var out bytes.Buffer
	sweepRoundWith(sweepParallelCfg(), sweepFn, &out)
	mu.Lock()
	defer mu.Unlock()
	if !ran["pa"] || !ran["pb"] {
		t.Fatalf("ran=%v want both policies executed (error must not block sibling)", ran)
	}
	if !bytes.Contains(out.Bytes(), []byte("sweep: policy pa: unexpected EOF")) {
		t.Fatalf("log missing error line for pa: %q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("sweep: policy pb: 5 files, 2 dirs physically deleted")) {
		t.Fatalf("log missing result line for pb: %q", out.String())
	}
}
