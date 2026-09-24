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
