package tool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

type CatalogInvalidator interface {
	Invalidate()
}

type SyncResult struct {
	Snapshot Snapshot
	Changed  bool
}

type SyncService struct {
	servers   server.Repository
	provider  runtime.Provider
	clients   mcpclient.Manager
	snapshots Repository
	catalog   CatalogInvalidator
	newID     func() string
}

func NewSyncService(servers server.Repository, provider runtime.Provider, clients mcpclient.Manager, snapshots Repository, catalog CatalogInvalidator) *SyncService {
	return &SyncService{
		servers: servers, provider: provider, clients: clients,
		snapshots: snapshots, catalog: catalog, newID: uuid.NewString,
	}
}

func (s *SyncService) WithIDGenerator(newID func() string) *SyncService {
	s.newID = newID
	return s
}

func (s *SyncService) Sync(ctx context.Context, id server.ID) (SyncResult, error) {
	srv, err := s.servers.GetByID(ctx, id)
	if err != nil {
		return SyncResult{}, fmt.Errorf("get server for tool sync: %w", err)
	}
	if !srv.Enabled || srv.Spec.DesiredState != server.DesiredRunning {
		return SyncResult{}, fmt.Errorf("%w: server must be enabled and desired running", ErrInvalidTool)
	}
	if s.provider == nil || s.provider.Type() != server.RuntimeRemote || srv.Spec.Runtime.Type != server.RuntimeRemote {
		return SyncResult{}, fmt.Errorf("%w: remote runtime provider required", ErrInvalidTool)
	}
	instance, err := s.provider.Ensure(ctx, srv)
	if err != nil {
		return SyncResult{}, fmt.Errorf("ensure remote runtime for tool sync: %w", err)
	}
	lease, err := s.clients.Acquire(ctx, srv, instance)
	if err != nil {
		return SyncResult{}, fmt.Errorf("acquire MCP session for tool sync: %w", err)
	}
	defer lease.Release()

	remoteTools, err := listAllTools(ctx, lease.Session(), srv.Spec.Timeouts.List)
	if err != nil {
		return SyncResult{}, fmt.Errorf("list remote tools: %w", err)
	}
	definitions, digest, err := NormalizeTools(srv.ID, srv.Name, remoteTools, s.newID)
	if err != nil {
		return SyncResult{}, err
	}
	previous, err := s.snapshots.GetActiveSnapshot(ctx, srv.ID)
	if err != nil && !errors.Is(err, ErrSnapshotNotFound) {
		return SyncResult{}, fmt.Errorf("get active tool snapshot: %w", err)
	}
	if err == nil && previous.Revision == srv.Revision && previous.CatalogDigest == digest {
		return SyncResult{Snapshot: previous}, nil
	}

	replacement, err := s.snapshots.ReplaceSnapshot(ctx, SnapshotReplacement{
		ID: s.newID(), ServerID: srv.ID, Revision: srv.Revision,
		CatalogDigest: digest, Tools: definitions,
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("replace active tool snapshot: %w", err)
	}
	s.catalog.Invalidate()
	return SyncResult{Snapshot: replacement, Changed: true}, nil
}

func listAllTools(ctx context.Context, session mcpclient.Session, timeout time.Duration) ([]mcpclient.Tool, error) {
	var tools []mcpclient.Tool
	cursor := ""
	seen := make(map[string]struct{})
	for {
		pageCtx := ctx
		cancel := func() {}
		if timeout > 0 {
			pageCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		page, next, err := session.ListTools(pageCtx, cursor)
		cancel()
		if err != nil {
			return nil, err
		}
		tools = append(tools, page...)
		if next == "" {
			return tools, nil
		}
		if _, exists := seen[next]; exists {
			return nil, fmt.Errorf("%w: repeated pagination cursor", ErrInvalidTool)
		}
		seen[next] = struct{}{}
		cursor = next
	}
}
