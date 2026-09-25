package docker

import (
	"errors"
	"io"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
)

// instanceState 是 Provider 在内存里持有的单实例运行态:容器 ID、attach 连接与标准流包装。
// 与 process 包同构:容器归属由 Provider 记忆,连接(attach)只是借用,
// 关闭会话不会停容器,但会让实例失效,由 ProviderManager 的 Inspect 失败路径负责收敛。
type instanceState struct {
	instance runtime.Instance

	containerID string

	mu     sync.Mutex
	resp   *client.HijackedResponse // stdio 专用:attach 连接;http 实例为 nil
	reader *io.PipeReader           // stdio 专用:解复用后的 stdout
	writer *io.PipeWriter           // stdio 专用:解复用写入端

	attached     bool
	detached     bool
	detachCause  error
	sessionEnded bool
}

// unavailable 返回实例不可用的原因;nil 表示进程内状态仍可用。
func (s *instanceState) unavailable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.detached && !s.sessionEnded {
		return nil
	}
	if s.detachCause != nil {
		return errors.Join(ErrInstanceUnavailable, s.detachCause)
	}
	return ErrInstanceUnavailable
}

// newStreams 暴露给 MCP 会话的标准流包装。只在 stdio 实例上调用。
func (s *instanceState) newStreams() runtime.Streams {
	return runtime.Streams{
		Stdin:  &streamWriter{state: s},
		Stdout: &streamReader{state: s, reader: s.reader},
	}
}

// startDemux 把 attach 的复用流按 Docker 帧格式解复用:stdout 进管道给 MCP 会话,
// stderr 丢弃(容器日志仍可由 daemon 的日志驱动读取,见 Logs)。
// 解复用结束意味着 attach 断开,实例随即可用性失效。
func (s *instanceState) startDemux() {
	s.mu.Lock()
	reader := s.resp.Reader
	writer := s.writer
	s.mu.Unlock()

	go func() {
		_, err := stdcopy.StdCopy(writer, io.Discard, reader)
		if err != nil && errors.Is(err, io.ErrClosedPipe) {
			err = nil
		}
		s.markDetached(err)
	}()
}

// endSession 结束本次 MCP 会话:关闭解复用管道与 attach 连接。
// 幂等;容器本身保持运行,后续 Ensure 会重新 attach 到同一个容器。
func (s *instanceState) endSession(cause error) {
	s.mu.Lock()
	if s.sessionEnded {
		s.mu.Unlock()
		return
	}
	s.sessionEnded = true
	s.detached = true
	if s.detachCause == nil {
		s.detachCause = cause
	}
	reader, writer, resp := s.reader, s.writer, s.resp
	s.reader, s.writer, s.resp = nil, nil, nil
	s.mu.Unlock()

	if writer != nil {
		_ = writer.CloseWithError(cause)
	}
	if reader != nil {
		_ = reader.CloseWithError(cause)
	}
	if resp != nil {
		resp.Close()
	}
}

// markDetached 记录 attach 断开的事实,并把管道读取端收尾;幂等。
func (s *instanceState) markDetached(cause error) {
	s.mu.Lock()
	if s.detached {
		s.mu.Unlock()
		return
	}
	s.detached = true
	s.detachCause = cause
	writer := s.writer
	s.writer = nil
	s.mu.Unlock()

	if writer != nil {
		_ = writer.CloseWithError(cause)
	}
}

type streamWriter struct {
	state *instanceState
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.state.mu.Lock()
	resp := w.state.resp
	w.state.mu.Unlock()
	if resp == nil {
		return 0, ErrInstanceUnavailable
	}
	return resp.Conn.Write(p)
}

// Close 只结束本次会话,不关闭容器侧连接之外的东西(进程 fd / 容器由 Provider 持有)。
func (w *streamWriter) Close() error {
	w.state.endSession(nil)
	return nil
}

type streamReader struct {
	state  *instanceState
	reader *io.PipeReader
}

func (r *streamReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *streamReader) Close() error {
	r.state.endSession(nil)
	return nil
}
