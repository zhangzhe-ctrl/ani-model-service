package worker_test

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/liangzai006/ani-model-service/internal/data/importer"
	"github.com/liangzai006/ani-model-service/internal/data/postgres"
	storagedata "github.com/liangzai006/ani-model-service/internal/data/storage"
	"github.com/liangzai006/ani-model-service/internal/identity"
	"github.com/liangzai006/ani-model-service/internal/service"
	"github.com/liangzai006/ani-model-service/internal/worker"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestImportManifestRepositoryE2E is an explicit opt-in acceptance test for
// the complete repository path. It leaves the dedicated tenant's task,
// artifact, and object as durable evidence.
func TestImportManifestRepositoryE2E(t *testing.T) {
	if os.Getenv("MODEL_IMPORT_MANIFEST_E2E") != "1" {
		t.Skip("MODEL_IMPORT_MANIFEST_E2E not enabled")
	}
	required := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	tenant := os.Getenv("E2E_TENANT_ID")
	if tenant == "" {
		tenant = uuid.NewString()
	}
	dsn := required("E2E_DSN")
	endpoint, access, secret := required("E2E_MINIO_ENDPOINT"), required("E2E_MINIO_ACCESS"), required("E2E_MINIO_SECRET")
	secure := os.Getenv("E2E_MINIO_INSECURE") != "1"

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	storage, err := storagedata.NewMinIOTenantBucketAdapter(endpoint, access, secret, secure)
	if err != nil {
		t.Fatal(err)
	}

	modelID, versionID := uuid.New(), uuid.New()
	externalModelID := "manifest-e2e-" + modelID.String()[:8]
	versionName := "main-" + versionID.String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.models(tenant_id,id,model_id,name,source,status)
		VALUES($1,$2,$3,$3,'huggingface','pending')`, tenant, modelID, externalModelID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.model_versions(tenant_id,id,model_id,version,format,status,checksum_sha256)
		VALUES($1,$2,$3,$4,'pytorch','pending','')`, tenant, versionID, modelID, versionName); err != nil {
		t.Fatal(err)
	}

	workStore := postgres.NewWorkStore(pool)
	versions := postgres.NewVersionStore(pool)
	providers := importer.Registry{"huggingface": importer.NewHuggingFaceAdapter("", nil)}
	modelService := service.NewModelServiceWithDependencies(versions, versions, postgres.NewCatalogStore(postgres.NewModelStore(pool)), storage, workStore)
	modelService.SetArtifactStore(postgres.NewArtifactStore(pool))
	modelService.SetAuditStore(postgres.NewAuditStore(pool))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	importWorker := &worker.Worker{
		Store: workStore, Binder: postgres.NewImportBinder(pool, providers),
		Executor:  worker.ImportExecutor{Providers: providers, Storage: storage, Artifacts: postgres.NewArtifactStore(pool), Checksums: versions, Logger: logger},
		Finalizer: versions, TenantID: tenant, Owner: "manifest-e2e-" + uuid.NewString(),
		PollInterval: 100 * time.Millisecond, Lease: 30 * time.Second, MaxAttempts: 3, Logger: logger,
	}
	modelService.SetImportNotifier(importWorker)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(identity.WithPrincipal(ctx, identity.Principal{TenantID: tenant, Actor: "manifest-e2e", Workload: "manifest-e2e", RequestID: "manifest-e2e"}), req)
	}))
	modelv1.RegisterModelServiceServer(server, modelService)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.DialContext(ctx, listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := modelv1.NewModelServiceClient(conn)

	workerCtx, workerCancel := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- importWorker.Run(workerCtx) }()
	firstWorkerStopped := false
	defer func() {
		if !firstWorkerStopped {
			workerCancel()
			<-workerDone
		}
	}()

	created, err := client.ImportModel(ctx, &modelv1.ImportModelRequest{
		TenantId: tenant, ModelId: externalModelID, ModelVersionId: versionID.String(),
		Source: "huggingface", RepoId: "sshleifer/tiny-gpt2", Revision: "main",
		IdempotencyKey: "manifest-e2e-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForManifestTask(t, ctx, client, tenant, created.GetTaskId(), "completed")
	if completed.GetProgressPct() != 100 || completed.GetErrorMessage() != "" || completed.GetCompletedAt() == nil {
		t.Fatalf("completed task fields=%+v", completed)
	}

	var status, reference, checksum string
	var size int64
	if err := pool.QueryRow(ctx, `
		SELECT v.status, a.reference, a.sha256, a.size_bytes
		FROM public.model_versions v JOIN public.model_artifacts a
		  ON a.tenant_id=v.tenant_id AND a.model_version_id=v.id
		WHERE v.tenant_id=$1 AND v.id=$2`, tenant, versionID).Scan(&status, &reference, &checksum, &size); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || reference == "" || len(checksum) != 64 || size <= 0 {
		t.Fatalf("ready facts status=%s reference=%s checksum=%s size=%d", status, reference, checksum, size)
	}

	download, err := client.GetModelDownloadURL(ctx, &modelv1.GetModelDownloadURLRequest{TenantId: tenant, ModelVersionId: versionID.String()})
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Get(download.GetDownloadUrl())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || int64(len(body)) != size {
		t.Fatalf("download status=%d bytes=%d want=%d", resp.StatusCode, len(body), size)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != checksum {
		t.Fatalf("download checksum=%s want=%s", got, checksum)
	}
	entries := tarEntries(t, body)
	want := []string{"config.json", "merges.txt", "pytorch_model.bin", "special_tokens_map.json", "tokenizer_config.json", "vocab.json"}
	for _, name := range want {
		if !contains(entries, name) {
			t.Fatalf("manifest archive missing %q: %v", name, entries)
		}
	}

	workerCancel()
	<-workerDone
	firstWorkerStopped = true
	if _, err := pool.Exec(ctx, `UPDATE public.model_import_tasks SET status='failed', attempt_count=3, progress_pct=61, error_message='e2e retry probe', completed_at=NULL WHERE tenant_id=$1 AND id=$2`, tenant, created.GetTaskId()); err != nil {
		t.Fatal(err)
	}
	retried, err := client.RetryImportTask(ctx, &modelv1.RetryImportTaskRequest{TenantId: tenant, TaskId: created.GetTaskId()})
	if err != nil {
		t.Fatal(err)
	}
	if retried.GetTask().GetStatus() != "pending" || retried.GetTask().GetAttemptCount() != 0 || retried.GetTask().GetProgressPct() != 0 || retried.GetTask().GetErrorMessage() != "" {
		t.Fatalf("retry response=%+v", retried.GetTask())
	}

	workerCtx, secondWorkerCancel := context.WithCancel(ctx)
	secondWorkerDone := make(chan error, 1)
	go func() { secondWorkerDone <- importWorker.Run(workerCtx) }()
	defer func() {
		secondWorkerCancel()
		<-secondWorkerDone
	}()
	final := waitForManifestTask(t, ctx, client, tenant, created.GetTaskId(), "completed")
	t.Logf("tenant=%s task=%s status=%s reference=%s bytes=%d sha256=%s entries=%d retry_status=%s", tenant, created.GetTaskId(), final.GetStatus(), reference, size, checksum, len(entries), retried.GetTask().GetStatus())
}

func waitForManifestTask(t *testing.T, ctx context.Context, client modelv1.ModelServiceClient, tenant, taskID, want string) *modelv1.ImportTask {
	t.Helper()
	for {
		got, err := client.GetImportTask(ctx, &modelv1.GetImportTaskRequest{TenantId: tenant, TaskId: taskID})
		if err != nil {
			t.Fatal(err)
		}
		task := got.GetTask()
		if task.GetStatus() == want {
			return task
		}
		if task.GetStatus() == "failed" {
			t.Fatalf("task reached failed state: %+v", task)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func tarEntries(t *testing.T, body []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytesReader(body))
	var entries []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, header.Name)
	}
	sort.Strings(entries)
	return entries
}

func bytesReader(body []byte) io.Reader { return &sliceReader{body: body} }

type sliceReader struct {
	body []byte
	pos  int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(p, r.body[r.pos:])
	r.pos += n
	return n, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
