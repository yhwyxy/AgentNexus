package mcpclient

import (
	"context"
	"sync"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

type fakeSession struct {
	mu     sync.Mutex
	closed bool
}

func (s *fakeSession) ListTools(context.Context, string) ([]Tool, string, error) { return nil, "", nil }
func (s *fakeSession) CallTool(context.Context, CallRequest) (CallResult, error) {
	return CallResult{}, nil
}
func (s *fakeSession) Ping(context.Context) error { return nil }
func (s *fakeSession) Close() error               { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }
func (s *fakeSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type fakeConnector struct {
	mu    sync.Mutex
	calls int
	sess  *fakeSession
}

func (c *fakeConnector) Connect(context.Context, runtime.ConnectTarget) (Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.sess == nil {
		c.sess = &fakeSession{}
	}
	return c.sess, nil
}

func TestManagerCachesSessionAndInvalidatesByServer(t *testing.T) {
	connector := &fakeConnector{}
	manager := NewManager(connector)
	srv := server.Server{ID: "demo", Revision: 1, Spec: server.Spec{Limits: server.LimitSpec{MaxInFlight: 2}}}
	instance := runtime.Instance{ID: "demo-remote", Target: runtime.ConnectTarget{Transport: "streamable_http", URL: "http://example/mcp"}}

	first, err := manager.Acquire(context.Background(), srv, instance)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Acquire(context.Background(), srv, instance)
	if err != nil {
		t.Fatal(err)
	}
	if connector.calls != 1 || first.Session() != second.Session() {
		t.Fatalf("session was not cached: calls=%d", connector.calls)
	}
	first.Release()
	second.Release()
	manager.Invalidate(srv.ID, nil)
	if !connector.sess.closed {
		t.Fatal("invalidation did not close session")
	}
	third, err := manager.Acquire(context.Background(), srv, instance)
	if err != nil {
		t.Fatal(err)
	}
	third.Release()
	if connector.calls != 2 {
		t.Fatalf("calls after invalidation = %d", connector.calls)
	}
}

// 运行实例被替换后,旧实例的 session 必须关闭,不能常驻也不能复用。
type recordingConnector struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession
	calls    int
}

func (c *recordingConnector) Connect(_ context.Context, target runtime.ConnectTarget) (Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	session := &fakeSession{}
	if c.sessions == nil {
		c.sessions = make(map[string]*fakeSession)
	}
	c.sessions[target.URL] = session
	return session, nil
}

func (c *recordingConnector) sessionFor(url string) *fakeSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[url]
}

func TestAcquireClosesOtherInstanceSessions(t *testing.T) {
	connector := &recordingConnector{}
	manager := NewManager(connector)
	srv := server.Server{ID: "demo", Revision: 1, Spec: server.Spec{Limits: server.LimitSpec{MaxInFlight: 2}}}
	oldInstance := runtime.Instance{ID: "demo-process-100", Target: runtime.ConnectTarget{Transport: "stdio", URL: "old"}}
	newInstance := runtime.Instance{ID: "demo-process-200", Target: runtime.ConnectTarget{Transport: "stdio", URL: "new"}}

	stale, err := manager.Acquire(context.Background(), srv, oldInstance)
	if err != nil {
		t.Fatal(err)
	}
	stale.Release()

	fresh, err := manager.Acquire(context.Background(), srv, newInstance)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Release()

	if connector.calls != 2 {
		t.Fatalf("connector calls = %d, want 2", connector.calls)
	}
	if session := connector.sessionFor("old"); session == nil || !session.isClosed() {
		t.Fatal("session of the replaced instance was not closed")
	}
	if session := connector.sessionFor("new"); session == nil || session.isClosed() {
		t.Fatal("session of the current instance must stay open")
	}
}
