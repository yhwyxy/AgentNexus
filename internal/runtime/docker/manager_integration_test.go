package docker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/runtime/hostaccess"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// TestProviderManagerDrivesDockerProvider 用真实 SQLite 仓储把 DockerProvider 接进
// ProviderManager,验证 Provider 契约的正确实现:注册 -> EnsureReady 建容器并回写 ready
// 状态 -> Reconcile 幂等 -> Close 回收容器。Engine 侧仍是内存实现,不需要 daemon。
func TestProviderManagerDrivesDockerProvider(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := sqlite.NewServerRepository(db)
	policy, err := hostaccess.NewPolicy([]hostaccess.Entry{
		{Host: "/srv/data", Container: "/data"},
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	registry := server.NewService(repo).WithPolicy(policy)

	api := newFakeAPI()
	api.images[testImage] = true
	api.attachStayOpen = true
	provider := newTestProvider(t, api, func(options *Options) {
		options.Policy = policy
		options.ConnectHost = "127.0.0.1"
		options.PublishHost = "127.0.0.1"
		options.StopGrace = 5 * time.Second
	})

	spec := testDockerSpec(server.TransportStdio)
	// 走真实注册路径时环境变量要避开凭据启发式(密钥只允许经 credentialId 传入)。
	spec.Env = map[string]string{"LOG_LEVEL": "info"}

	created, err := registry.Register(ctx, server.RegisterInput{
		Name:      "weather",
		Transport: server.TransportStdio,
		Runtime: server.RuntimeSpec{
			Type:   server.RuntimeDocker,
			Docker: &spec,
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	manager, err := runtime.NewManager(repo, provider)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	instance, err := manager.EnsureReady(ctx, created)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if instance.Provider != string(server.RuntimeDocker) || instance.ExternalID == "" {
		t.Fatalf("instance = %#v", instance)
	}
	if len(api.created) != 1 || len(api.started) != 1 || api.started[0] != instance.ExternalID {
		t.Fatalf("engine created/started = %d/%d %v", len(api.created), len(api.started), api.started)
	}
	state := api.containerState(instance.ExternalID)
	if state == nil || !state.running {
		t.Fatalf("container state = %#v, want running", state)
	}
	if state.config.Image != testImage || state.config.Labels[labelServerID] != string(created.ID) {
		t.Fatalf("container config = %#v", state.config)
	}

	stored, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	// 管理器只负责"实例已就绪":ready 由上层连接成功后回写,这里断言 starting + 已观测版本。
	if stored.Status.Phase != server.PhaseStarting || stored.Status.ObservedRevision != created.Revision {
		t.Fatalf("persisted status = %#v, want starting@%d", stored.Status, created.Revision)
	}

	// Reconcile 只读巡检:容器仍在运行,不应产生新的容器。
	if err := manager.Reconcile(ctx, created.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(api.created) != 1 {
		t.Fatalf("Reconcile created %d containers, want 1", len(api.created))
	}

	// Close 必须按 Releaser 契约回收容器,并关闭 Engine 客户端。
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(api.removed) != 1 {
		t.Fatalf("removed containers = %v, want the managed one", api.removed)
	}
	if !api.closed {
		t.Fatal("provider Close did not close the engine client")
	}
}
