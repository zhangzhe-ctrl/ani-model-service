package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
	workbiz "github.com/liangzai006/ani-model-service/internal/biz/work"
)

func TestWorkStoreLeaseRecoveryPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tenant, modelID, taskID := uuid.New(), uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, "INSERT INTO public.models(tenant_id,id,name,source,status) VALUES($1,$2,$3,'upload','pending')", tenant, modelID, "it-"+taskID.String())
	if err != nil {
		t.Fatal(err)
	}
	store := NewWorkStore(pool)
	_, err = store.Create(ctx, workTask(tenant, modelID, taskID))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, tenant.String(), taskID.String(), "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Renew(ctx, tenant.String(), taskID.String(), "worker-a", claimed.LeaseEpoch, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, tenant.String(), taskID.String(), "worker-old", claimed.LeaseEpoch); err == nil {
		t.Fatal("stale owner unexpectedly completed task")
	}
	if err := store.Complete(ctx, tenant.String(), taskID.String(), "worker-a", claimed.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
}

// An interrupted process leaves an importing row leased; after expiry the
// next process must be able to claim it with a newer fencing epoch.
func TestWorkStoreExpiredLeaseCanBeReclaimedPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tenant, modelID, taskID := uuid.New(), uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, "INSERT INTO public.models(tenant_id,id,name,source,status) VALUES($1,$2,$3,'upload','pending')", tenant, modelID, "reclaim-"+taskID.String())
	if err != nil {
		t.Fatal(err)
	}
	store := NewWorkStore(pool)
	created, err := store.Create(ctx, workTask(tenant, modelID, taskID))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, tenant.String(), created.ID, "crashed-worker", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	second, err := store.Claim(ctx, tenant.String(), created.ID, "restarted-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.LeaseEpoch <= first.LeaseEpoch || second.LeaseOwner != "restarted-worker" {
		t.Fatalf("reclaim=%+v first=%+v", second, first)
	}
	if err := store.Complete(ctx, tenant.String(), created.ID, "crashed-worker", first.LeaseEpoch); err == nil {
		t.Fatal("stale crashed worker completed task")
	}
	if err := store.Complete(ctx, tenant.String(), created.ID, "restarted-worker", second.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestWorkStoreRemoteImportWithoutModelBindingPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant, taskID := uuid.New(), uuid.New()
	store := NewWorkStore(pool)
	task := workbiz.Task{TenantID: tenant.String(), ID: taskID.String(), TaskType: "huggingface", Source: "huggingface", RepoID: "org/model", Revision: "main", IdempotencyKey: "remote-" + taskID.String()}
	created, err := store.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if created.ModelID != "" || created.VersionID != "" || created.Status != workbiz.Pending {
		t.Fatalf("unexpected unbound import task: %+v", created)
	}
	replay, err := store.Create(ctx, task)
	if err != nil || replay.ID != created.ID {
		t.Fatalf("same idempotency request did not replay original task: task=%+v err=%v", replay, err)
	}
	task.RepoID = "org/other-model"
	if _, err := store.Create(ctx, task); err == nil {
		t.Fatal("different payload unexpectedly replayed")
	} else if err == pgx.ErrNoRows {
		t.Fatalf("conflict returned not-found: %v", err)
	}
}

func TestWorkStoreGetAndRetryFailedPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant, modelID, taskID := uuid.New(), uuid.New(), uuid.New()
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.model_import_tasks WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.models WHERE tenant_id=$1", tenant)
	}()
	if _, err = pool.Exec(ctx, "INSERT INTO public.models(tenant_id,id,name,source,status) VALUES($1,$2,$3,'upload','pending')", tenant, modelID, "retry-"+taskID.String()); err != nil {
		t.Fatal(err)
	}
	versionID := uuid.New()
	versions := NewVersionStore(pool)
	if _, err = versions.CreateVersion(ctx, modelbiz.Version{TenantID: tenant.String(), ID: versionID.String(), ModelID: modelID.String(), Version: "v1", Format: "gguf"}); err != nil {
		t.Fatal(err)
	}
	store := NewWorkStore(pool)
	created, err := store.Create(ctx, workbiz.Task{TenantID: tenant.String(), ModelID: modelID.String(), VersionID: versionID.String(), ID: taskID.String(), TaskType: "upload", Source: "upload", IdempotencyKey: "retry-" + taskID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE public.model_versions SET status='error', error_message='checksum mismatch' WHERE tenant_id=$1 AND id=$2", tenant, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE public.model_import_tasks SET status='failed', attempt_count=5, progress_pct=73, error_message='checksum mismatch' WHERE tenant_id=$1 AND id=$2", tenant, taskID); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Get(ctx, tenant.String(), created.ID)
	if err != nil || failed.Status != workbiz.Failed || failed.ProgressPct != 73 || failed.ErrorMessage != "checksum mismatch" {
		t.Fatalf("get failed task=%+v err=%v", failed, err)
	}
	retried, err := store.RetryFailed(ctx, tenant.String(), created.ID)
	if err != nil || retried.Status != workbiz.Pending || retried.AttemptCount != 0 || retried.ProgressPct != 0 || retried.ErrorMessage != "" {
		t.Fatalf("retry task=%+v err=%v", retried, err)
	}
	var versionStatus, versionError string
	if err := pool.QueryRow(ctx, "SELECT status, error_message FROM public.model_versions WHERE tenant_id=$1 AND id=$2", tenant, versionID).Scan(&versionStatus, &versionError); err != nil {
		t.Fatal(err)
	}
	if versionStatus != "pending" || versionError != "" {
		t.Fatalf("retry version status=%q error=%q, want pending and empty", versionStatus, versionError)
	}
}

func TestVersionStoreMarkErrorAggregatePostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.model_import_tasks WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.models WHERE tenant_id=$1", tenant)
	}()
	versions := NewVersionStore(pool)
	work := NewWorkStore(pool)
	var versionStatus string
	createModel := func(name, status string) uuid.UUID {
		modelID := uuid.New()
		if _, err := pool.Exec(ctx, "INSERT INTO public.models(tenant_id,id,name,source,status) VALUES($1,$2,$3,'upload',$4)", tenant, modelID, name, status); err != nil {
			t.Fatal(err)
		}
		return modelID
	}
	createVersion := func(modelID uuid.UUID, version, status string) uuid.UUID {
		versionID := uuid.New()
		if _, err := versions.CreateVersion(ctx, modelbiz.Version{TenantID: tenant.String(), ID: versionID.String(), ModelID: modelID.String(), Version: version, Format: "gguf"}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE public.model_versions SET status=$3 WHERE tenant_id=$1 AND id=$2", tenant, versionID, status); err != nil {
			t.Fatal(err)
		}
		return versionID
	}
	createLease := func(modelID, versionID uuid.UUID) uuid.UUID {
		taskID := uuid.New()
		if _, err := work.Create(ctx, workbiz.Task{TenantID: tenant.String(), ModelID: modelID.String(), VersionID: versionID.String(), ID: taskID.String(), TaskType: "upload", Source: "upload", IdempotencyKey: "aggregate-" + taskID.String()}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE public.model_import_tasks SET status='importing', lease_owner='worker', lease_epoch=1, lease_until=clock_timestamp()+interval '1 minute' WHERE tenant_id=$1 AND id=$2", tenant, taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}

	withoutReady := createModel("error-"+uuid.NewString(), "pending")
	withoutReadyVersion := createVersion(withoutReady, "v1", "pending")
	withoutReadyTask := createLease(withoutReady, withoutReadyVersion)
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), withoutReadyTask.String(), withoutReadyVersion.String(), "worker", 1, "provider failed"); err != nil {
		t.Fatal(err)
	}
	var modelStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM public.models WHERE tenant_id=$1 AND id=$2", tenant, withoutReady).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != "error" {
		t.Fatalf("model without ready version = %q, want error", modelStatus)
	}

	withReady := createModel("ready-"+uuid.NewString(), "ready")
	failingVersion := createVersion(withReady, "v1", "pending")
	_ = createVersion(withReady, "v2", "ready")
	failingTask := createLease(withReady, failingVersion)
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), failingTask.String(), failingVersion.String(), "worker", 1, "provider failed"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM public.models WHERE tenant_id=$1 AND id=$2", tenant, withReady).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != "ready" {
		t.Fatalf("model with ready version = %q, want ready", modelStatus)
	}
	readyWithoutVersionModel := createModel("ready-without-version-"+uuid.NewString(), "ready")
	readyWithoutVersion := createVersion(readyWithoutVersionModel, "v1", "pending")
	readyWithoutVersionTask := createLease(readyWithoutVersionModel, readyWithoutVersion)
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), readyWithoutVersionTask.String(), readyWithoutVersion.String(), "worker", 1, "provider failed"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM public.models WHERE tenant_id=$1 AND id=$2", tenant, readyWithoutVersionModel).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != "ready" {
		t.Fatalf("ready model without ready version = %q, want ready", modelStatus)
	}
	readyTarget := createVersion(withReady, "v3", "ready")
	readyTask := createLease(withReady, readyTarget)
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), readyTask.String(), readyTarget.String(), "worker", 1, "must not overwrite"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ready version transition err=%v, want pgx.ErrNoRows", err)
	}

	deletedModel := createModel("deleted-"+uuid.NewString(), "pending")
	deletedVersion := createVersion(deletedModel, "v1", "pending")
	deletedTask := createLease(deletedModel, deletedVersion)
	alreadyDeletedVersion := createVersion(deletedModel, "v2", "deleted")
	alreadyDeletedTask := createLease(deletedModel, alreadyDeletedVersion)
	if _, err := pool.Exec(ctx, "UPDATE public.models SET status='deleted' WHERE tenant_id=$1 AND id=$2", tenant, deletedModel); err != nil {
		t.Fatal(err)
	}
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), deletedTask.String(), deletedVersion.String(), "worker", 1, "must not overwrite model"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM public.models WHERE tenant_id=$1 AND id=$2", tenant, deletedModel).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != "deleted" {
		t.Fatalf("deleted model status=%q, want deleted", modelStatus)
	}

	deletedVersion = alreadyDeletedVersion
	deletedTask = alreadyDeletedTask
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), deletedTask.String(), deletedVersion.String(), "worker", 1, "must not overwrite"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted version transition err=%v, want pgx.ErrNoRows", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM public.model_versions WHERE tenant_id=$1 AND id=$2", tenant, deletedVersion).Scan(&versionStatus); err != nil {
		t.Fatal(err)
	}
	if versionStatus != "deleted" {
		t.Fatalf("deleted version status=%q, want deleted", versionStatus)
	}

	if err := versions.MarkErrorAndMaybeModel(ctx, uuid.New().String(), withoutReadyTask.String(), withoutReadyVersion.String(), "worker", 1, "cross tenant"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-tenant transition err=%v, want pgx.ErrNoRows", err)
	}
	staleModel := createModel("stale-"+uuid.NewString(), "pending")
	staleVersion := createVersion(staleModel, "v1", "pending")
	staleTask := createLease(staleModel, staleVersion)
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), staleTask.String(), staleVersion.String(), "old-worker", 1, "stale lease"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale lease transition err=%v, want pgx.ErrNoRows", err)
	}
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), staleTask.String(), staleVersion.String(), "worker", 1, "provider failed"); err != nil {
		t.Fatalf("current lease transition err=%v", err)
	}
	expiredModel := createModel("expired-"+uuid.NewString(), "pending")
	expiredVersion := createVersion(expiredModel, "v1", "pending")
	expiredTask := createLease(expiredModel, expiredVersion)
	if _, err := pool.Exec(ctx, "UPDATE public.model_import_tasks SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, expiredTask); err != nil {
		t.Fatal(err)
	}
	if err := versions.MarkErrorAndMaybeModel(ctx, tenant.String(), expiredTask.String(), expiredVersion.String(), "worker", 1, "expired lease"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired lease transition err=%v, want pgx.ErrNoRows", err)
	}
}

func TestVersionStoreMarkErrorConcurrentReadyPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant, modelID := uuid.New(), uuid.New()
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.model_import_tasks WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.models WHERE tenant_id=$1", tenant)
	}()
	if _, err := pool.Exec(ctx, "INSERT INTO public.models(tenant_id,id,name,source,status) VALUES($1,$2,$3,'upload','pending')", tenant, modelID, "race-"+modelID.String()); err != nil {
		t.Fatal(err)
	}
	versions := NewVersionStore(pool)
	work := NewWorkStore(pool)
	failVersion, readyVersion := uuid.New(), uuid.New()
	for _, versionID := range []uuid.UUID{failVersion, readyVersion} {
		if _, err := versions.CreateVersion(ctx, modelbiz.Version{TenantID: tenant.String(), ID: versionID.String(), ModelID: modelID.String(), Version: versionID.String(), Format: "gguf", ArtifactSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewArtifactStore(pool).CreateArtifact(ctx, modelbiz.Artifact{TenantID: tenant.String(), ID: uuid.NewString(), ModelVersionID: readyVersion.String(), Provider: "storage", Reference: "obj://race", Format: "gguf", SizeBytes: 1, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	taskID := uuid.New()
	if _, err := work.Create(ctx, workbiz.Task{TenantID: tenant.String(), ID: taskID.String(), ModelID: modelID.String(), VersionID: failVersion.String(), TaskType: "upload", Source: "upload", IdempotencyKey: "race-" + taskID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE public.model_import_tasks SET status='importing', lease_owner='worker', lease_epoch=1, lease_until=clock_timestamp()+interval '1 minute' WHERE tenant_id=$1 AND id=$2", tenant, taskID); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, "SELECT 1 FROM public.models WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, modelID); err != nil {
		t.Fatal(err)
	}
	finalizeErr := make(chan error, 1)
	go func() {
		finalizeErr <- versions.MarkErrorAndMaybeModel(ctx, tenant.String(), taskID.String(), failVersion.String(), "worker", 1, "provider failed")
	}()
	// Let the finalizer update the version and wait on the model lock, then
	// commit a ready sibling while that lock is held.
	time.Sleep(50 * time.Millisecond)
	if n, err := New(blocker).MarkModelVersionReady(ctx, MarkModelVersionReadyParams{TenantID: uuidType(tenant), ID: uuidType(readyVersion)}); err != nil || n != 1 {
		t.Fatalf("ready transition n=%d err=%v", n, err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finalizeErr; err != nil {
		t.Fatal(err)
	}
	var modelStatus, failStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM public.models WHERE tenant_id=$1 AND id=$2", tenant, modelID).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM public.model_versions WHERE tenant_id=$1 AND id=$2", tenant, failVersion).Scan(&failStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != "ready" || failStatus != "error" {
		t.Fatalf("concurrent ready/error statuses model=%q failed_version=%q", modelStatus, failStatus)
	}
}

func workTask(t, m, id uuid.UUID) workbiz.Task {
	return workbiz.Task{TenantID: t.String(), ModelID: m.String(), ID: id.String(), TaskType: "upload", Source: "upload", IdempotencyKey: "it-" + id.String()}
}
