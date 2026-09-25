// 审计领域模型：事件只表达"谁、在哪个请求里、对哪个资产/目标做了什么、结果如何"，
// 不依赖 net/http、SQL、MCP SDK 或 slog。
//
// 两类事实的来源不同：
//   - 请求日志（edge 层）负责每次请求的 status/duration/request_id/principal;
//   - 审计事件（本包）负责状态变更与调用这类"值得长期留存"的事实。
//
// Record 的唯一落库点是 Recorder；领域包只通过消费者定义的 observer 接口发射事件。
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// Outcome 是事件的粗粒度结果。取值按详细设计 §11.2 的 "审计 outcome=error/cancelled"，
// 细粒度原因放在 ErrorCode（§11.1 的内部错误码）。
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeError     Outcome = "error"
	OutcomeCancelled Outcome = "cancelled"
)

func (o Outcome) valid() bool {
	return o == OutcomeSuccess || o == OutcomeError || o == OutcomeCancelled
}

// EventType 是审计事件族。v0.1 只有这九类，未知类型一律拒绝（宁缺勿滥）。
type EventType string

const (
	// EventServerRegistered 管理 API 注册成功（asset 已持久化）。
	EventServerRegistered EventType = "server.registered"
	// EventServerRefreshRequested 手工 refresh-tools 被受理（异步同步尚未开始）。
	EventServerRefreshRequested EventType = "server.refresh_requested"
	// EventToolSnapshotPublished Tool 快照原子替换成功（目录已可路由新工具）。
	EventToolSnapshotPublished EventType = "tool.snapshot_published"
	// EventToolSyncFailed 一次 Tool 同步失败（后端不可达、协议错误、校验失败）。
	EventToolSyncFailed EventType = "tool.sync_failed"
	// EventRuntimeStarted 新实例就绪（含关停后重启恢复）。
	EventRuntimeStarted EventType = "runtime.started"
	// EventRuntimeRestarted 陈旧实例被替换为新实例。
	EventRuntimeRestarted EventType = "runtime.restarted"
	// EventRuntimeStopped 活动实例被停止。
	EventRuntimeStopped EventType = "runtime.stopped"
	// EventRuntimeFailed Runtime 协调失败，Server 进入 degraded。
	EventRuntimeFailed EventType = "runtime.failed"
	// EventToolCalled 一次 tools/call 完成（含失败与取消）。
	EventToolCalled EventType = "tool.called"
)

var eventTypes = map[EventType]struct{}{
	EventServerRegistered:       {},
	EventServerRefreshRequested: {},
	EventToolSnapshotPublished:  {},
	EventToolSyncFailed:         {},
	EventRuntimeStarted:         {},
	EventRuntimeRestarted:       {},
	EventRuntimeStopped:         {},
	EventRuntimeFailed:          {},
	EventToolCalled:             {},
}

// ErrInvalidEvent 表示事件本身非法（未知类型/非法 outcome/Detail 不是 JSON 对象或过大）。
// 这是编程错误：调用方应记日志并修正发射点，而不是重试。
var ErrInvalidEvent = errors.New("invalid audit event")

// 审计事实。字段全部已脱敏：没有任何自由文本的 message，失败原因只由 ErrorCode 表达，
// 需要细节时放 Detail（已按 deny-list 清理）。
type Event struct {
	ID         string
	OccurredAt time.Time // UTC
	Type       EventType
	Outcome    Outcome

	ActorName string // 调用者名；系统发起（Runtime/同步）为空
	ActorRole string // admin|agent；系统发起为空
	RequestID string // 触发的请求 ID；后台巡检为空

	AssetID   string // 强绑定的资产；无则空
	AssetName string // 冗余保存，资产删除后事件仍可读
	Target    string // 工具 public name / runtime instance ID
	RuntimeID string

	ErrorCode  string          // 详细设计 §11.1 的码；成功与 cancelled 为空
	DurationMs int64           // 只在语义上有耗时的事件（Tool 调用）填写
	Detail     json.RawMessage // 已脱敏的 JSON 对象，默认 {}
}

// Input 是 Record 的输入：与 Event 同形，去掉由 Recorder 补齐的 ID 与 OccurredAt。
type Input struct {
	Type    EventType
	Outcome Outcome

	ActorName string
	ActorRole string
	RequestID string

	AssetID   string
	AssetName string
	Target    string
	RuntimeID string

	ErrorCode  string
	DurationMs int64
	Detail     json.RawMessage
}

// Repository 只追加审计事件。查询 API 属于后续切片，v0.1 不需要读取路径。
type Repository interface {
	Append(context.Context, Event) error
}

// Event 列表：本包只依赖单一落库点，避免出现第二套审计写入路径。
const (
	// maxDetailBytes 是 Detail 序列化后的上限。超过就拒绝而不是截断，
	// 避免留下半截事实。
	maxDetailBytes = 4 << 10
	// maxDetailDepth 限制递归深度，防御畸形嵌套输入。
	maxDetailDepth = 16
)

// Recorder 校验、补齐并落库审计事件。构造后不可变，可并发使用。
type Recorder struct {
	repo  Repository
	newID func() string
	now   func() time.Time
}

func NewRecorder(repo Repository) *Recorder {
	return &Recorder{repo: repo, newID: uuid.NewString, now: time.Now}
}

func (r *Recorder) WithIDGenerator(newID func() string) *Recorder {
	r.newID = newID
	return r
}

func (r *Recorder) WithClock(now func() time.Time) *Recorder {
	r.now = now
	return r
}

// Record 写入一条事件。它从不改变业务结果：非法输入返回 ErrInvalidEvent，
// 存储错误原样返回，两者的处理方式（记日志、不失败请求）由调用方决定。
func (r *Recorder) Record(ctx context.Context, in Input) error {
	if _, ok := eventTypes[in.Type]; !ok {
		return fmt.Errorf("%w: unsupported event type %q", ErrInvalidEvent, in.Type)
	}
	if !in.Outcome.valid() {
		return fmt.Errorf("%w: unsupported outcome %q", ErrInvalidEvent, in.Outcome)
	}
	if in.DurationMs < 0 {
		return fmt.Errorf("%w: duration must not be negative", ErrInvalidEvent)
	}
	detail, err := redactDetail(in.Detail)
	if err != nil {
		return err
	}
	event := Event{
		ID:         r.newID(),
		OccurredAt: r.now().UTC(),
		Type:       in.Type,
		Outcome:    in.Outcome,
		ActorName:  in.ActorName,
		ActorRole:  in.ActorRole,
		RequestID:  in.RequestID,
		AssetID:    in.AssetID,
		AssetName:  in.AssetName,
		Target:     in.Target,
		RuntimeID:  in.RuntimeID,
		ErrorCode:  in.ErrorCode,
		DurationMs: in.DurationMs,
		Detail:     detail,
	}
	if err := r.repo.Append(ctx, event); err != nil {
		return fmt.Errorf("append audit event %q: %w", in.Type, err)
	}
	return nil
}

// redactDetail 把 Detail 规范化为已脱敏的 JSON 对象：
//   - nil/空 → {};
//   - 必须是 JSON 对象（数组与标量都拒绝，事件详情是键值集合）;
//   - deny-list 键（大小写与 -/_ 不敏感）连同其值整体删除;
//   - 递归深度与序列化大小有上限。
//
// deny-list 是防止 endpoint、宿主机路径、命令行、env、凭证进入审计数据的结构性防线：
// 新增事件字段时，若不希望它落进审计表，就把键名加进 deny 集合。
func redactDetail(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: detail must be valid JSON: %v", ErrInvalidEvent, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: detail must be a single JSON value", ErrInvalidEvent)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: detail must be a JSON object", ErrInvalidEvent)
	}
	redacted, err := redactValue(object, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("%w: encode detail: %v", ErrInvalidEvent, err)
	}
	if len(encoded) > maxDetailBytes {
		return nil, fmt.Errorf("%w: detail exceeds %d bytes", ErrInvalidEvent, maxDetailBytes)
	}
	return encoded, nil
}

func redactValue(value any, depth int) (any, error) {
	if depth > maxDetailDepth {
		return nil, fmt.Errorf("%w: detail nesting exceeds %d levels", ErrInvalidEvent, maxDetailDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, nested := range typed {
			if isSensitiveKey(key) {
				continue
			}
			scrubbed, err := redactValue(nested, depth+1)
			if err != nil {
				return nil, err
			}
			cleaned[key] = scrubbed
		}
		return cleaned, nil
	case []any:
		cleaned := make([]any, 0, len(typed))
		for _, nested := range typed {
			scrubbed, err := redactValue(nested, depth+1)
			if err != nil {
				return nil, err
			}
			cleaned = append(cleaned, scrubbed)
		}
		return cleaned, nil
	default:
		return value, nil
	}
}

// sensitiveKeys 是 deny-list 的规范化形态（小写、去掉 - 与 _）。
var sensitiveKeys = map[string]struct{}{
	"authorization": {}, "apikey": {}, "secret": {}, "secrets": {}, "token": {},
	"tokens": {}, "password": {}, "credential": {}, "credentials": {},
	"endpoint": {}, "url": {}, "uri": {}, "command": {}, "args": {}, "argv": {},
	"env": {}, "envvars": {}, "headers": {}, "workingdir": {},
	"hostpath": {}, "path": {},
}

func isSensitiveKey(key string) bool {
	normalized := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '_' || c == '-':
			continue
		case c >= 'A' && c <= 'Z':
			normalized = append(normalized, c+'a'-'A')
		default:
			normalized = append(normalized, c)
		}
	}
	_, denied := sensitiveKeys[string(normalized)]
	return denied
}
