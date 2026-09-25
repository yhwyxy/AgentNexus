package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/app"
	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

// 同步成功/失败都要留下审计事实：这是"目录何时变过、何时拉取失败"的唯一持久记录。
func TestLifecycleRecordsToolSyncAuditEvents(t *testing.T) {
	tests := []struct {
		name        string
		syncErr     error
		wantType    audit.EventType
		wantOutcome audit.Outcome
		wantCode    string
	}{
		{
			name:        "success publishes snapshot",
			wantType:    audit.EventToolSnapshotPublished,
			wantOutcome: audit.OutcomeSuccess,
		},
		{
			name:        "failure records sync_failed",
			syncErr:     errors.New("backend unreachable"),
			wantType:    audit.EventToolSyncFailed,
			wantOutcome: audit.OutcomeError,
			wantCode:    audit.CodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auditor := &recordingAuditor{}
			registry := &fakeRegistry{srv: server.Server{ID: "srv-1", Name: "weather"}}
			syncer := &fakeSyncer{started: make(chan server.ID, 1), fail: map[server.ID]error{"srv-1": tt.syncErr}}
			lifecycle := newTestLifecycle(t, syncer, registry, func(opts *app.LifecycleOptions) {
				opts.Auditor = auditor
			})

			lifecycle.Trigger("srv-1")
			waitForID(t, syncer.started, "srv-1")
			closeLifecycle(t, lifecycle) // Close 等待 worker 退出，因此此后断言是确定的

			event, ok := auditor.find(tt.wantType)
			if !ok {
				t.Fatalf("no %q event in %#v", tt.wantType, auditor.recorded())
			}
			if event.Outcome != tt.wantOutcome || event.ErrorCode != tt.wantCode {
				t.Errorf("outcome/code = %q/%q, want %q/%q", event.Outcome, event.ErrorCode, tt.wantOutcome, tt.wantCode)
			}
			if event.AssetID != "srv-1" || event.AssetName != "weather" {
				t.Errorf("asset = %q/%q, want srv-1/weather", event.AssetID, event.AssetName)
			}
			if tt.wantType == audit.EventToolSnapshotPublished && event.Target == "" {
				t.Error("snapshot event has no target snapshot id")
			}
			// 后台同步没有请求上下文：actor 与 request_id 必须留空。
			if event.ActorName != "" || event.RequestID != "" {
				t.Errorf("actor/request = %q/%q, want empty", event.ActorName, event.RequestID)
			}
		})
	}
}

// 关停取消在飞同步时，事件仍必须落库（这正是用不可取消 ctx 写审计的原因）。
func TestLifecycleRecordsCancelledSync(t *testing.T) {
	auditor := &recordingAuditor{}
	registry := &fakeRegistry{srv: server.Server{ID: "srv-1", Name: "weather"}}
	syncer := &fakeSyncer{
		started:  make(chan server.ID, 1),
		canceled: make(chan server.ID, 1),
		block:    make(chan struct{}),
	}
	lifecycle := newTestLifecycle(t, syncer, registry, func(opts *app.LifecycleOptions) {
		opts.Auditor = auditor
	})

	lifecycle.Trigger("srv-1")
	waitForID(t, syncer.started, "srv-1")
	closeLifecycle(t, lifecycle)

	select {
	case <-syncer.canceled:
	case <-time.After(testTimeout):
		t.Fatal("in-flight sync was not cancelled by shutdown")
	}
	event, ok := auditor.find(audit.EventToolSyncFailed)
	if !ok {
		t.Fatalf("cancelled sync recorded no event: %#v", auditor.recorded())
	}
	if event.Outcome != audit.OutcomeCancelled || event.ErrorCode != "" {
		t.Errorf("outcome/code = %q/%q, want cancelled with empty code", event.Outcome, event.ErrorCode)
	}
}

// 审计写入失败不得影响同步与注册：审计是旁路事实，不是前置条件。
func TestLifecycleSurvivesAuditFailure(t *testing.T) {
	registry := &fakeRegistry{srv: server.Server{ID: "srv-1", Name: "weather"}}
	syncer := &fakeSyncer{started: make(chan server.ID, 1)}
	lifecycle := newTestLifecycle(t, syncer, registry, func(opts *app.LifecycleOptions) {
		opts.Auditor = failingAuditor{}
	})

	if _, err := lifecycle.Register(context.Background(), server.RegisterInput{Name: "weather"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForID(t, syncer.started, "srv-1")
	closeLifecycle(t, lifecycle)
}

type failingAuditor struct{}

func (failingAuditor) Record(context.Context, audit.Input) error {
	return errors.New("audit storage unavailable")
}
