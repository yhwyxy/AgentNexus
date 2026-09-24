// Tool 定义、快照和路由属于领域模型，不依赖 SQL 或 MCP SDK。
package tool

import (
	"encoding/json"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

type SnapshotState string

const (
	SnapshotBuilding   SnapshotState = "building"
	SnapshotActive     SnapshotState = "active"
	SnapshotSuperseded SnapshotState = "superseded"
	SnapshotFailed     SnapshotState = "failed"
)

type Definition struct {
	ID           string
	ServerID     server.ID
	ServerName   string
	BackendName  string
	PublicName   string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  json.RawMessage
	SchemaDigest string
}

type Route struct {
	ServerID    server.ID
	ServerName  string
	BackendName string
	PublicName  string
	SnapshotID  string
}

type Snapshot struct {
	ID            string
	ServerID      server.ID
	Revision      int64
	Generation    int64
	State         SnapshotState
	ToolCount     int
	CatalogDigest string
	ErrorMessage  string
	CreatedAt     time.Time
	ActivatedAt   *time.Time
	Tools         []Definition
}

type SnapshotReplacement struct {
	ID            string
	ServerID      server.ID
	Revision      int64
	CatalogDigest string
	Tools         []Definition
}
