package service

import (
	"context"
	"errors"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	workbiz "github.com/liangzai006/ani-model-service/internal/biz/work"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

type importTaskReaderFake struct {
	task workbiz.Task
	err  error
}

type importTaskLogReaderFake struct {
	logs      string
	err       error
	namespace string
	jobName   string
}

func (f *importTaskLogReaderFake) Logs(_ context.Context, namespace, jobName string) (string, error) {
	f.namespace, f.jobName = namespace, jobName
	return f.logs, f.err
}

func (f importTaskReaderFake) Get(context.Context, string, string) (workbiz.Task, error) {
	return f.task, f.err
}

type importTaskRetrierFake struct {
	task       workbiz.Task
	err        error
	tenant, id string
}

func (f *importTaskRetrierFake) RetryFailed(_ context.Context, tenant, id string) (workbiz.Task, error) {
	f.tenant, f.id = tenant, id
	return f.task, f.err
}

func TestGetImportTaskReturnsProgressAndError(t *testing.T) {
	created := time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC)
	s := NewModelImportService(nil)
	s.SetImportTaskReader(importTaskReaderFake{task: workbiz.Task{
		TenantID: "tenant-a", ID: "11111111-1111-1111-1111-111111111111", Source: "huggingface", Status: workbiz.Failed,
		AttemptCount: 3, ProgressPct: 42, ErrorMessage: "checksum mismatch", CreatedAt: created,
	}})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	got, err := s.GetImportTask(ctx, &modelv1.GetImportTaskRequest{TenantId: "tenant-a", TaskId: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	task := got.GetTask()
	if task.GetStatus() != workbiz.Failed || task.GetProgressPct() != 42 || task.GetErrorMessage() != "checksum mismatch" || task.GetAttemptCount() != 3 || task.GetCreatedAt().AsTime() != created {
		t.Fatalf("task response = %+v", task)
	}
}

func TestCompletedImportTaskDoesNotExposeStaleError(t *testing.T) {
	s := NewModelImportService(nil)
	s.SetImportTaskReader(importTaskReaderFake{task: workbiz.Task{
		TenantID: "tenant-a", ID: "11111111-1111-1111-1111-111111111111", Source: "modelscope", Status: workbiz.Completed,
		ProgressPct: 100, ErrorMessage: "previous attempt failed", CompletedAt: time.Now().UTC(),
	}})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	got, err := s.GetImportTask(ctx, &modelv1.GetImportTaskRequest{TenantId: "tenant-a", TaskId: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetTask().GetStatus() != workbiz.Completed || got.GetTask().GetErrorMessage() != "" {
		t.Fatalf("completed task response = %+v; want no error", got.GetTask())
	}
}

func TestGetImportTaskLogsReturnsCurrentAttemptTailAndStatus(t *testing.T) {
	reader := &importTaskLogReaderFake{logs: "downloaded 42%\n"}
	s := NewModelImportService(nil)
	s.SetImportTaskReader(importTaskReaderFake{task: workbiz.Task{
		TenantID: "tenant-a", ID: "11111111-1111-1111-1111-111111111111", Status: workbiz.Importing,
		AttemptCount: 3,
	}})
	s.SetImportTaskLogReader(reader, "ani-model")
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	got, err := s.GetImportTaskLogs(ctx, &modelv1.GetImportTaskLogsRequest{TenantId: "tenant-a", TaskId: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetTaskId() != "11111111-1111-1111-1111-111111111111" || got.GetStatus() != workbiz.Importing || got.GetLogs() != "downloaded 42%\n" || !got.GetAvailable() {
		t.Fatalf("logs response = %+v", got)
	}
	if reader.namespace != "ani-model" || reader.jobName != "ani-import-11111111111111111111111111111111-a3" {
		t.Fatalf("log target = %s/%s", reader.namespace, reader.jobName)
	}
}

func TestGetImportTaskLogsReturnsUnavailableWhenJobWasCleanedUp(t *testing.T) {
	s := NewModelImportService(nil)
	s.SetImportTaskReader(importTaskReaderFake{task: workbiz.Task{TenantID: "tenant-a", ID: "11111111-1111-1111-1111-111111111111", Status: workbiz.Completed, AttemptCount: 1}})
	s.SetImportTaskLogReader(&importTaskLogReaderFake{err: errors.New("import job has no pod")}, "ani-model")
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	_, err := s.GetImportTaskLogs(ctx, &modelv1.GetImportTaskLogsRequest{TenantId: "tenant-a", TaskId: "11111111-1111-1111-1111-111111111111"})
	if kratoserrors.Code(err) != 409 || kratoserrors.Reason(err) != "IMPORT_LOGS_UNAVAILABLE" {
		t.Fatalf("error = %v, code=%d reason=%q", err, kratoserrors.Code(err), kratoserrors.Reason(err))
	}
}

func TestRetryImportTaskResetsFailedTask(t *testing.T) {
	retrier := &importTaskRetrierFake{task: workbiz.Task{TenantID: "tenant-a", ID: "11111111-1111-1111-1111-111111111111", Status: workbiz.Pending, AttemptCount: 0}}
	s := NewModelImportService(nil)
	s.SetImportTaskRetrier(retrier)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	got, err := s.RetryImportTask(ctx, &modelv1.RetryImportTaskRequest{TenantId: "tenant-a", TaskId: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetTask().GetStatus() != workbiz.Pending || got.GetTask().GetAttemptCount() != 0 || retrier.tenant != "tenant-a" || retrier.id != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("retry response=%+v tenant=%q id=%q", got, retrier.tenant, retrier.id)
	}
}

func TestRetryImportTaskUsesRequestTenantWithoutPrincipal(t *testing.T) {
	// SECURITY: Without a principal, the request must fail authentication.
	s := NewModelImportService(nil)
	_, err := s.RetryImportTask(context.Background(), &modelv1.RetryImportTaskRequest{TenantId: "tenant-a", TaskId: "task-a"})
	if kratoserrors.Code(err) != 401 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}
