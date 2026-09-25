package process

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
)

// instanceState 是一个活动子进程的进程内状态。fd 只由 Provider 关闭;
// 会话拿到的 sessionStream 只做"本次连接结束"的通知。
type instanceState struct {
	instance runtime.Instance
	pid      int
	stdin    *os.File
	stdout   *os.File
	logs     *boundedBuffer

	sessionEnded atomic.Bool

	mu       sync.Mutex
	exited   bool
	exitErr  error
	exitedAt time.Time
}

// newStreams 生成连接用的标准流包装。仅本次连接有效:一旦 Close,
// 实例即视为不可用,ProviderManager 会 reap 并重新 Ensure 出新的进程实例。
func (s *instanceState) newStreams() *runtime.Streams {
	return &runtime.Streams{
		Stdin:  &sessionWriter{state: s, file: s.stdin},
		Stdout: &sessionReader{state: s, file: s.stdout},
	}
}

func (s *instanceState) markExited(err error) {
	s.mu.Lock()
	s.exited = true
	s.exitErr = err
	s.exitedAt = time.Now()
	s.mu.Unlock()
	// 关闭 Provider 侧的 fd:阻塞在读端上的会话 goroutine 会立刻返回,
	// 而不会在下一次连接时抢子进程的字节。
	s.closeFiles()
}

func (s *instanceState) closeFiles() {
	_ = s.stdin.Close()
	_ = s.stdout.Close()
}

func (s *instanceState) hasExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *instanceState) endSession() { s.sessionEnded.Store(true) }

func (s *instanceState) sessionClosed() bool { return s.sessionEnded.Load() }

func (s *instanceState) unavailableReason() error {
	if s.sessionClosed() {
		return fmt.Errorf("%w: instance %q standard streams are closed", ErrInstanceUnavailable, s.instance.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		if s.exitErr != nil {
			return fmt.Errorf("%w: process %d exited: %v", ErrInstanceUnavailable, s.pid, s.exitErr)
		}
		return fmt.Errorf("%w: process %d exited", ErrInstanceUnavailable, s.pid)
	}
	return nil
}

// sessionReader 包装子进程 stdout,Close 不关闭底层 fd。
type sessionReader struct {
	state  *instanceState
	file   *os.File
	closed atomic.Bool
}

func (r *sessionReader) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *sessionReader) Close() error {
	if r.closed.CompareAndSwap(false, true) {
		r.state.endSession()
	}
	return nil
}

// sessionWriter 包装子进程 stdin,Close 不关闭底层 fd。
type sessionWriter struct {
	state  *instanceState
	file   *os.File
	closed atomic.Bool
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	return w.file.Write(p)
}

func (w *sessionWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.state.endSession()
	}
	return nil
}
