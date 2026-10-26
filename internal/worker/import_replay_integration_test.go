package worker_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	storagedomain "github.com/liangzai006/ani-model-service/internal/biz/storage"
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

// TestImportReplayExistingBucket uses an existing dedicated test tenant and ready
// version. It writes a new import task and reuploads the same provider object;
// it leaves evidence in PostgreSQL/MinIO and never deletes resources.
func TestImportReplayExistingBucket(t *testing.T) {
	if os.Getenv("MODEL_IMPORT_E2E") != "1" {
		t.Skip("MODEL_IMPORT_E2E not enabled")
	}
	required := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tenant := required("E2E_TENANT_ID")
	modelID, versionName := required("E2E_MODEL_ID"), required("E2E_VERSION")
	repoID, revision := required("E2E_REPO_ID"), required("E2E_REVISION")
	endpoint, access, secret := required("E2E_MINIO_ENDPOINT"), required("E2E_MINIO_ACCESS"), required("E2E_MINIO_SECRET")
	secure := os.Getenv("E2E_MINIO_INSECURE") != "1"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := required("E2E_DSN")
	pool, err := pgxpool.New(ctx, dsn)
	must(err)
	defer pool.Close()
	storage, err := storagedata.NewMinIOTenantBucketAdapter(endpoint, access, secret, secure)
	must(err)
	// Require the bucket to exist before starting the worker.
	bucketCheck, err := storagedata.NewMinIOAdapter(endpoint, access, secret, tenant, secure)
	must(err)
	must(bucketCheck.CheckBucket(ctx))
	versions := postgres.NewVersionStore(pool)
	workStore := postgres.NewWorkStore(pool)
	providers := importer.Registry{"huggingface": importer.NewHuggingFaceAdapter("", nil)}
	modelService := service.NewModelServiceWithDependencies(versions, versions, postgres.NewCatalogStore(postgres.NewModelStore(pool)), storage, workStore)
	modelService.SetArtifactStore(postgres.NewArtifactStore(pool))
	modelService.SetAuditStore(postgres.NewAuditStore(pool))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	importWorker := &worker.Worker{Store: workStore, Binder: postgres.NewImportBinder(pool, providers), Executor: worker.ImportExecutor{Providers: providers, Storage: storage, Artifacts: postgres.NewArtifactStore(pool), Checksums: versions, Logger: logger}, Finalizer: versions, TenantID: tenant, Owner: "e2e-" + uuid.NewString(), PollInterval: 100 * time.Millisecond, Lease: 30 * time.Second, MaxAttempts: 3, Logger: logger}
	modelService.SetImportNotifier(importWorker)
	workerCtx, workerCancel := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- importWorker.Run(workerCtx) }()
	defer func() { workerCancel(); <-workerDone }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(identity.WithPrincipal(ctx, identity.Principal{TenantID: tenant, Actor: "e2e", Workload: "e2e", RequestID: "e2e-existing-request"}), req)
	}))
	modelv1.RegisterModelServiceServer(server, modelService)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.DialContext(ctx, listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	must(err)
	defer conn.Close()
	client := modelv1.NewModelServiceClient(conn)
	ready, err := client.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: tenant, ModelId: modelID, Version: versionName})
	must(err)
	request := &modelv1.ImportModelRequest{TenantId: tenant, ModelId: modelID, ModelVersionId: ready.GetVersion().GetId(), Source: "huggingface", RepoId: repoID, Revision: revision, IdempotencyKey: "e2e-existing-replay-" + uuid.NewString()}
	task, err := client.ImportModel(ctx, request)
	must(err)
	var status string
	for {
		err = pool.QueryRow(ctx, "select status from public.model_import_tasks where tenant_id=$1 and id=$2", tenant, task.GetTaskId()).Scan(&status)
		must(err)
		if status == "completed" {
			break
		}
		if status == "failed" {
			t.Fatalf("import reached failed terminal state: %s", task.GetTaskId())
		}
		if ctx.Err() != nil {
			must(ctx.Err())
		}
		time.Sleep(200 * time.Millisecond)
	}
	replay, err := client.ImportModel(ctx, request)
	must(err)
	if replay.GetTaskId() != task.GetTaskId() {
		t.Fatal("same request created another import task")
	}
	after, err := client.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: tenant, ModelId: modelID, Version: versionName})
	must(err)
	if after.GetVersion().GetId() != ready.GetVersion().GetId() || after.GetVersion().GetChecksumSha256() != ready.GetVersion().GetChecksumSha256() {
		t.Fatal("replay changed immutable version facts")
	}
	download, err := client.GetModelDownloadURL(ctx, &modelv1.GetModelDownloadURLRequest{TenantId: tenant, ModelVersionId: ready.GetVersion().GetId()})
	must(err)
	httpClient := &http.Client{Timeout: 15 * time.Second}
	fetch := func(url string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal("cannot build download request")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal("download transport failed (URL redacted)")
		}
		return resp
	}
	resp := fetch(download.GetDownloadUrl())
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	must(err)
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	if resp.StatusCode != http.StatusOK || sum != ready.GetVersion().GetChecksumSha256() {
		t.Fatalf("download status=%d checksum=%s", resp.StatusCode, sum)
	}
	shortURL, err := storage.CreateDownloadURL(ctx, storagedomain.DownloadRequest{TenantID: tenant, ObjectRef: ready.GetVersion().GetStoragePath(), TTL: time.Second})
	must(err)
	// Prove the same URL works before expiry; transport failures are never evidence of expiry.
	resp = fetch(shortURL.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh URL status=%d", resp.StatusCode)
	}
	time.Sleep(time.Until(shortURL.ExpiresAt.Add(time.Second)))
	resp = fetch(shortURL.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired URL status=%d", resp.StatusCode)
	}
	t.Logf("tenant=%s task=%s status=%s reused_version=%s bytes=%d sha256=%s expired_status=%d", tenant, task.GetTaskId(), status, ready.GetVersion().GetId(), len(body), sum, resp.StatusCode)
}
