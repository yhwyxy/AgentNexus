// MCP client 接口隔离官方 SDK，供 Gateway 和 Tool Sync 使用。
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

type Tool struct {
	Name         string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  json.RawMessage
}

type Content struct {
	Type string
	Text string
	Data json.RawMessage
}

type CallRequest struct {
	Name      string
	Arguments json.RawMessage
}

type CallResult struct {
	Content           []Content
	StructuredContent json.RawMessage
	IsError           bool
}

type Session interface {
	ListTools(context.Context, string) ([]Tool, string, error)
	CallTool(context.Context, CallRequest) (CallResult, error)
	Ping(context.Context) error
	Close() error
}

type Connector interface {
	Connect(context.Context, runtime.ConnectTarget) (Session, error)
}

type SessionLease interface {
	Session() Session
	Release()
}

type Manager interface {
	Acquire(context.Context, server.Server, runtime.Instance) (SessionLease, error)
	Invalidate(server.ID, error)
	Close(context.Context) error
}

type SessionManager struct {
	connector Connector
	mu        sync.Mutex
	entries   map[string]*entry
	closed    bool
}

type entry struct {
	session Session
	refs    int
	limit   chan struct{}
}

func NewManager(connector Connector) *SessionManager {
	return &SessionManager{connector: connector, entries: make(map[string]*entry)}
}

func (m *SessionManager) Acquire(ctx context.Context, srv server.Server, instance runtime.Instance) (SessionLease, error) {
	key := fmt.Sprintf("%s/%d/%s", srv.ID, srv.Revision, instance.ID)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("session manager is closed")
	}
	e := m.entries[key]
	if e == nil {
		m.mu.Unlock()
		s, err := m.connector.Connect(ctx, instance.Target)
		if err != nil {
			return nil, fmt.Errorf("connect MCP session: %w", err)
		}
		limit := srv.Spec.Limits.MaxInFlight
		if limit < 1 {
			limit = 1
		}
		m.mu.Lock()
		if existing := m.entries[key]; existing != nil {
			m.mu.Unlock()
			_ = s.Close()
			e = existing
		} else {
			e = &entry{session: s, limit: make(chan struct{}, limit)}
			m.entries[key] = e
			m.mu.Unlock()
		}
	} else {
		m.mu.Unlock()
	}
	select {
	case e.limit <- struct{}{}:
		m.mu.Lock()
		e.refs++
		m.mu.Unlock()
		return &lease{manager: m, key: key, entry: e}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type lease struct {
	manager *SessionManager
	key     string
	entry   *entry
	once    sync.Once
}

func (l *lease) Session() Session { return l.entry.session }

func (l *lease) Release() {
	l.once.Do(func() {
		<-l.entry.limit
		l.manager.mu.Lock()
		l.entry.refs--
		l.manager.mu.Unlock()
	})
}

func (m *SessionManager) Invalidate(serverID server.ID, _ error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.entries {
		if len(key) > len(serverID) && key[:len(serverID)] == string(serverID) && key[len(serverID)] == '/' {
			delete(m.entries, key)
			_ = e.session.Close()
		}
	}
}

func (m *SessionManager) Close(_ context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	entries := m.entries
	m.entries = make(map[string]*entry)
	m.mu.Unlock()
	var first error
	for _, e := range entries {
		if err := e.session.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
