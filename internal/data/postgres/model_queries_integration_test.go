package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	auditbiz "github.com/liangzai006/ani-model-service/internal/biz/audit"
	"github.com/liangzai006/ani-model-service/internal/data/postgres"
	"github.com/liangzai006/ani-model-service/internal/identity"
	"github.com/liangzai006/ani-model-service/internal/service"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func TestModelQueriesPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	tenant, other, modelID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, owner := range []string{tenant, other} {
		exec(`INSERT INTO public.models (tenant_id,id,model_id,name,source,status) VALUES ($1,$2,'filter-test','filter-test','upload','ready')`, owner, modelID)
	}
	for i, state := range []string{"ready", "error", "importing", "ready", "deleted", "pending"} {
		exec(`INSERT INTO public.model_versions (tenant_id,id,model_id,version,format,status,created_at) VALUES ($1,$2,$3,$4,'safetensors',$5,$6)`, tenant, uuid.NewString(), modelID, uuid.NewString(), state, time.Date(2026, 10, 21, 0, i, 0, 0, time.UTC))
	}
	// A matching version in another tenant must not affect count or pagination.
	exec(`INSERT INTO public.model_versions (tenant_id,id,model_id,version,format,status) VALUES ($1,$2,$3,'foreign','safetensors','ready')`, other, uuid.NewString(), modelID)
	catalog := postgres.NewCatalogStore(postgres.NewModelStore(tx))
	versions := postgres.NewVersionStore(tx)
	audit := postgres.NewAuditStore(tx)
	s := service.NewModelServiceWithDependencies(versions, versions, catalog, nil, nil)
	s.SetAuditStore(audit)
	principal := func(owner string) context.Context {
		return identity.WithPrincipal(ctx, identity.Principal{TenantID: owner, Actor: "integration", Workload: "governance", RequestID: uuid.NewString()})
	}
	for _, tc := range []struct {
		status string
		page   int32
		count  int
		total  int64
		state  string
	}{
		{"ready", 1, 1, 2, "ready"}, {"ready", 2, 1, 2, "ready"}, {"ready", 3, 0, 2, ""},
		{"failed", 1, 1, 1, "error"}, {"downloading", 1, 1, 1, "importing"}, {"pending", 1, 1, 1, "pending"}, {"", 1, 1, 5, "pending"},
	} {
		got, err := s.ListModelVersions(principal(tenant), &modelv1.ListModelVersionsRequest{ModelId: "filter-test", Status: tc.status, Page: &tc.page, PageSize: int32ptr(1)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != tc.count || got.Total != tc.total || (tc.count > 0 && got.Items[0].Status != tc.state) {
			t.Fatalf("case=%+v got=%v", tc, got)
		}
	}
	for _, state := range []string{"ready", "error", "pending"} {
		if err := audit.Insert(ctx, auditbiz.Event{TenantID: tenant, ModelID: modelID, Actor: "alice", Workload: "governance", RequestID: uuid.NewString(), Action: "model_import.create", AfterState: state}); err != nil {
			t.Fatal(err)
		}
	}
	if err := audit.Insert(ctx, auditbiz.Event{TenantID: other, ModelID: modelID, Actor: "foreign", Workload: "governance", RequestID: uuid.NewString(), Action: "model.create", AfterState: "ready"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListModelOperations(principal(tenant), &modelv1.ListModelOperationsRequest{ModelId: "filter-test", Page: int32ptr(1), PageSize: int32ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 3 || len(got.Items) != 2 || got.Items[0].Actor != "alice" || got.Items[0].Result != "accepted" || got.Items[1].Result != "failed" {
		t.Fatalf("history=%v", got)
	}
	got, err = s.ListModelOperations(principal(tenant), &modelv1.ListModelOperationsRequest{ModelId: modelID, Page: int32ptr(2), PageSize: int32ptr(2)})
	if err != nil || got.GetTotal() != 3 || len(got.GetItems()) != 1 || got.Items[0].Result != "succeeded" {
		t.Fatalf("history page 2=%v err=%v", got, err)
	}
	got, err = s.ListModelOperations(principal(tenant), &modelv1.ListModelOperationsRequest{ModelId: modelID, Page: int32ptr(3), PageSize: int32ptr(2)})
	if err != nil || got.GetTotal() != 3 || len(got.GetItems()) != 0 {
		t.Fatalf("empty page=%v err=%v", got, err)
	}
	// Legacy/import-before-binding events have only task identity. Resolve the
	// task within the same tenant when it is bound later, without losing history.
	taskID := uuid.NewString()
	exec(`INSERT INTO public.model_import_tasks (tenant_id,id,task_type,idempotency_key,status) VALUES ($1,$2,'huggingface',$3,'pending')`, tenant, taskID, taskID)
	if err := audit.Insert(ctx, auditbiz.Event{TenantID: tenant, TaskID: taskID, Actor: "legacy", Workload: "governance", RequestID: uuid.NewString(), Action: "model_import.create", AfterState: "pending"}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE public.model_import_tasks SET model_id=$3 WHERE tenant_id=$1 AND id=$2`, tenant, taskID, modelID)
	got, err = s.ListModelOperations(principal(tenant), &modelv1.ListModelOperationsRequest{ModelId: modelID})
	if err != nil || got.GetTotal() != 4 || len(got.GetItems()) != 4 || got.Items[0].Actor != "legacy" || got.Items[0].ModelId != modelID || got.Items[0].TaskId != taskID {
		t.Fatalf("late-bound task history=%v err=%v", got, err)
	}
}
