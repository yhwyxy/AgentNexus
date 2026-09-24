package tool

import (
	"context"
	"errors"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

var (
	ErrSnapshotNotFound = errors.New("tool snapshot not found")
	ErrToolNotFound     = errors.New("tool not found")
	ErrInvalidTool      = errors.New("invalid tool definition")
	ErrCatalogConflict  = errors.New("catalog public name conflict")
)

// Repository 持久化工具快照，并按对外可见规则查询聚合目录。
type Repository interface {
	ReplaceSnapshot(context.Context, SnapshotReplacement) (Snapshot, error)
	GetActiveSnapshot(context.Context, server.ID) (Snapshot, error)
	ListAggregated(context.Context) ([]Definition, error)
	ResolvePublicName(context.Context, string) (Route, error)
}
