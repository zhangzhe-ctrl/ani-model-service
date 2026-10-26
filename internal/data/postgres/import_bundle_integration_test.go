package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
)

func bundleImportFixture(tenant, key string) modelbiz.BundleImport {
	return modelbiz.BundleImport{
		TenantID: tenant, IdempotencyKey: key,
		Name: "bundle-test-model", DisplayName: "Bundle Test", Description: "integration test",
		ExternalModelID: "org/bundle-test", Capabilities: []string{"text-generation"},
		Version: "v1", Format: "safetensors", EngineType: "vllm", EngineImage: "registry/vllm:test",
		StartupCommand: "vllm serve /models --served-model-name bundle-test",
		Provider:       "huggingface", RepoID: "org/bundle-test", Revision: "main",
	}
}

func TestBundleImportStorePostgres(t *testing.T) {
	dsn := os.Getenv("ANI_DATABASE_DSN")
	if dsn == "" {
		t.Skip("ANI_DATABASE_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.NewString()
	otherTenant := uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM public.model_import_tasks WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.model_versions WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.models WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.model_import_tasks WHERE tenant_id=$1`, otherTenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.model_versions WHERE tenant_id=$1`, otherTenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.models WHERE tenant_id=$1`, otherTenant)
	})
	store := NewBundleImportStore(pool)
	in := bundleImportFixture(tenant, "bundle-"+uuid.NewString())
	first, err := store.ImportBundle(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var counts struct{ models, versions, tasks int }
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.models WHERE tenant_id=$1`, tenant).Scan(&counts.models); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.model_versions WHERE tenant_id=$1`, tenant).Scan(&counts.versions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.model_import_tasks WHERE tenant_id=$1`, tenant).Scan(&counts.tasks); err != nil {
		t.Fatal(err)
	}
	if counts.models != 1 || counts.versions != 1 || counts.tasks != 1 {
		t.Fatalf("counts=%+v", counts)
	}
	var modelSource, modelRepo, modelFingerprint string
	if err := pool.QueryRow(ctx, `SELECT source, source_repo_id, request_fingerprint FROM public.models WHERE tenant_id=$1 AND id=$2`, tenant, first.Model.ID).Scan(&modelSource, &modelRepo, &modelFingerprint); err != nil {
		t.Fatal(err)
	}
	var startupCommand, versionFingerprint string
	if err := pool.QueryRow(ctx, `SELECT startup_command, request_fingerprint FROM public.model_versions WHERE tenant_id=$1 AND id=$2`, tenant, first.Version.ID).Scan(&startupCommand, &versionFingerprint); err != nil {
		t.Fatal(err)
	}
	var taskSource, taskRepo, taskRevision, taskFingerprint string
	if err := pool.QueryRow(ctx, `SELECT source, repo_id, revision, request_fingerprint FROM public.model_import_tasks WHERE tenant_id=$1 AND id=$2`, tenant, first.TaskID).Scan(&taskSource, &taskRepo, &taskRevision, &taskFingerprint); err != nil {
		t.Fatal(err)
	}
	if modelSource != in.Provider || modelRepo != in.RepoID || taskSource != in.Provider || taskRepo != in.RepoID || taskRevision != in.Revision {
		t.Fatalf("source fields model=%q/%q task=%q/%q/%q", modelSource, modelRepo, taskSource, taskRepo, taskRevision)
	}
	if startupCommand != in.StartupCommand || startupCommand == "" || modelFingerprint == "" || modelFingerprint != versionFingerprint || modelFingerprint != taskFingerprint {
		t.Fatalf("runtime/fingerprint fields command=%q fingerprints=%q/%q/%q", startupCommand, modelFingerprint, versionFingerprint, taskFingerprint)
	}
	replay, err := store.ImportBundle(ctx, in)
	if err != nil || replay.Model.ID != first.Model.ID || replay.Version.ID != first.Version.ID || replay.TaskID != first.TaskID {
		t.Fatalf("replay=%+v err=%v first=%+v", replay, err, first)
	}
	conflict := in
	conflict.StartupCommand += " --port 8000"
	if _, err := store.ImportBundle(ctx, conflict); err == nil {
		t.Fatal("changed startup command unexpectedly replayed")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("conflict error=%v", err)
		}
	}
	partial := in
	partial.IdempotencyKey = "partial-" + uuid.NewString()
	partial.Name = "partial-" + uuid.NewString()
	partial.ExternalModelID = "org/" + partial.Name
	partialModelID := uuid.NewString()
	partialFingerprint := "partial-fingerprint"
	if _, err := pool.Exec(ctx, `INSERT INTO public.models
 (tenant_id, id, model_id, name, display_name, description, source, source_repo_id,
  capabilities, status, idempotency_key, request_fingerprint)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10,$11)`, tenant, partialModelID, partial.ExternalModelID,
		partial.Name, partial.DisplayName, partial.Description, partial.Provider, partial.RepoID,
		[]byte(`["text-generation"]`), partial.IdempotencyKey, partialFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportBundle(ctx, partial); err == nil {
		t.Fatal("partial aggregate unexpectedly completed")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("partial aggregate error=%v", err)
		}
	}
	var partialName string
	if err := pool.QueryRow(ctx, `SELECT name FROM public.models WHERE tenant_id=$1 AND id=$2`, tenant, partialModelID).Scan(&partialName); err != nil {
		t.Fatal(err)
	}
	if partialName != partial.Name {
		t.Fatalf("partial model was overwritten: got %q want %q", partialName, partial.Name)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.model_import_tasks WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, partial.IdempotencyKey).Scan(&counts.tasks); err != nil {
		t.Fatal(err)
	}
	if counts.tasks != 0 {
		t.Fatalf("failed transaction left task rows: %d", counts.tasks)
	}
	var partialVersions, partialTasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.model_versions WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, partial.IdempotencyKey).Scan(&partialVersions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.model_import_tasks WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, partial.IdempotencyKey).Scan(&partialTasks); err != nil {
		t.Fatal(err)
	}
	if partialVersions != 0 || partialTasks != 0 {
		t.Fatalf("partial aggregate gained rows: versions=%d tasks=%d", partialVersions, partialTasks)
	}

	other := in
	other.TenantID = otherTenant
	otherResult, err := store.ImportBundle(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if otherResult.Model.ID == first.Model.ID || otherResult.Version.ID == first.Version.ID || otherResult.TaskID == first.TaskID {
		t.Fatalf("tenant-isolated IDs unexpectedly match: first=%+v other=%+v", first, otherResult)
	}

	concurrent := in
	concurrent.IdempotencyKey = "concurrent-" + uuid.NewString()
	concurrent.Name = "concurrent-" + uuid.NewString()
	concurrent.ExternalModelID = "org/" + concurrent.Name
	const workers = 8
	results := make(chan modelbiz.BundleImportResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, callErr := store.ImportBundle(ctx, concurrent)
			results <- result
			errs <- callErr
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	var modelID, versionID, taskID string
	for callErr := range errs {
		if callErr != nil {
			t.Fatal(callErr)
		}
	}
	for result := range results {
		if modelID == "" {
			modelID, versionID, taskID = result.Model.ID, result.Version.ID, result.TaskID
			continue
		}
		if result.Model.ID != modelID || result.Version.ID != versionID || result.TaskID != taskID {
			t.Fatalf("concurrent IDs differ: %+v", result)
		}
	}
}
