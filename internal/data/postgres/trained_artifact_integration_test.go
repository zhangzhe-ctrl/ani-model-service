package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
)

func TestTrainedArtifactStoreCompletesAggregateIdempotently(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not configured")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	tenant := uuid.NewString()
	registrationID := uuid.NewString()
	store := NewTrainedArtifactStore(pool)
	session := modelbiz.TrainedArtifactRegistration{
		TenantID:           tenant,
		ID:                 registrationID,
		IdempotencyKey:     "training:" + registrationID,
		RequestFingerprint: "fingerprint-" + registrationID,
		ExecutionID:        "execution-" + registrationID,
		ModelName:          "trained-qwen-" + registrationID[:8],
		DisplayName:        "Trained Qwen",
		Description:        "integration test artifact",
		Capabilities:       []string{"text-generation"},
		Version:            "run-1",
		FileName:           "model.tar",
		Format:             "safetensors",
		StoragePath:        tenant + "/trained-artifacts/" + registrationID + "/model.tar",
		SizeBytes:          4,
		ChecksumSHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		EngineType:         "vllm",
		EngineImage:        "registry/vllm:cpu",
		StartupCommand:     "vllm serve /models --served-model-name trained-qwen",
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM public.trained_artifact_registrations WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.model_artifacts WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.model_versions WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM public.models WHERE tenant_id=$1`, tenant)
	})

	first, err := store.Prepare(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	replayed := session
	replayed.ID = uuid.NewString()
	gotReplay, err := store.Prepare(ctx, replayed)
	if err != nil {
		t.Fatal(err)
	}
	if gotReplay.ID != first.ID {
		t.Fatalf("replay registration id=%q, want %q", gotReplay.ID, first.ID)
	}
	if err := store.MarkUploaded(ctx, tenant, first.ID); err != nil {
		t.Fatal(err)
	}
	result, err := store.Complete(ctx, tenant, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != modelbiz.RegistrationReady || result.ModelID == "" || result.ModelVersionID == "" {
		t.Fatalf("complete result=%+v", result)
	}
	replayedResult, err := store.Complete(ctx, tenant, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayedResult != result {
		t.Fatalf("complete replay=%+v, want %+v", replayedResult, result)
	}

	var modelStatus, versionStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM public.models WHERE tenant_id=$1 AND id=$2`, tenant, result.ModelID).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM public.model_versions WHERE tenant_id=$1 AND id=$2`, tenant, result.ModelVersionID).Scan(&versionStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != modelbiz.VersionReady || versionStatus != modelbiz.VersionReady {
		t.Fatalf("statuses model=%q version=%q, want ready/ready", modelStatus, versionStatus)
	}
}
