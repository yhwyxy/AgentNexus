package app_test

import (
	"context"
	"sync"

	"github.com/yhwyxy/AgentNexus/internal/audit"
)

// recordingAuditor 在内存中收集审计事件，用于断言发射点的行为。
// 它只实现 app.AuditRecorder，不校验事件合法性——校验是 audit.Recorder 的职责。
type recordingAuditor struct {
	mu     sync.Mutex
	events []audit.Input
}

func (a *recordingAuditor) Record(_ context.Context, in audit.Input) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.events = append(a.events, in)

	return nil
}

func (a *recordingAuditor) recorded() []audit.Input {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]audit.Input(nil), a.events...)
}

// find 返回第一类匹配 eventType 的事件。
func (a *recordingAuditor) find(eventType audit.EventType) (audit.Input, bool) {
	for _, event := range a.recorded() {
		if event.Type == eventType {
			return event, true
		}
	}

	return audit.Input{}, false
}
