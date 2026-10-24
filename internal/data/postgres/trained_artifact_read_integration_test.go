package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/liangzai006/ani-model-service/internal/data/postgres"
	storagedata "github.com/liangzai006/ani-model-service/internal/data/storage"
	"github.com/liangzai006/ani-model-service/internal/identity"
	"github.com/liangzai006/ani-model-service/internal/service"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/protobuf/proto"
)

func TestTrainedArtifactOriginalKeyReadAndCopyProofPostgres(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant, execution := uuid.NewString(), uuid.NewString()
	key := "modeldev:registration:" + execution
	t.Cleanup(func() {
		for _, table := range []string{"trained_artifact_registrations", "model_artifacts", "model_versions", "models"} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM public.`+table+` WHERE tenant_id=$1`, tenant)
		}
	})
	correct := []byte("original merged model bytes")
	h := sha256.Sum256(correct)
	digest := hex.EncodeToString(h[:])
	var mutex sync.Mutex
	var uploaded []byte
	writes := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			writes++
			w.WriteHeader(405)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/models/"+tenant+"/trained-artifacts/") || !strings.HasSuffix(r.URL.Path, "/model.tar") {
			t.Errorf("wrong controlled target %s", r.URL)
			w.WriteHeader(404)
			return
		}
		if uploaded == nil {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(uploaded)))
		w.Header().Set("ETag", `"not-the-sha256"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		if r.Method == http.MethodGet {
			_, _ = w.Write(uploaded)
		}
	}))
	defer endpoint.Close()
	storage, err := storagedata.NewMinIOAdapter(strings.TrimPrefix(endpoint.URL, "http://"), "access", "secret", "models", false)
	if err != nil {
		t.Fatal(err)
	}
	app := service.NewModelServiceWithDependencies(nil, nil, nil, storage, nil)
	registrar := postgres.NewTrainedArtifactStore(pool)
	app.SetTrainedArtifactRegistrar(registrar)
	principal := identity.WithPrincipal(ctx, identity.Principal{TenantID: tenant, Actor: "modeldev:integration", Workload: "ani-modeldev-service"})
	prepare := &modelv1.PrepareTrainedArtifactRequest{TenantId: tenant, ModelName: "original-model-" + execution[:8], DisplayName: "Original model", Description: "real PG registration test", Capabilities: []string{"text-generation"}, Version: "original-version", FileName: "model.tar", Format: "safetensors", SizeBytes: int64(len(correct)), ChecksumSha256: digest, ExecutionId: execution, IdempotencyKey: key, EngineType: "vllm", EngineImage: "registry/vllm@sha256:" + strings.Repeat("a", 64), StartupCommand: "vllm serve /models"}
	first, err := app.PrepareTrainedArtifact(principal, prepare)
	if err != nil {
		t.Fatal(err)
	}
	lookup := &modelv1.GetTrainedArtifactRegistrationRequest{TenantId: tenant, IdempotencyKey: key, ExpectedExecutionId: execution}
	read, err := app.GetTrainedArtifactRegistration(principal, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.ValidateFor(lookup); err != nil {
		t.Fatal(err)
	}
	if read.Registration.RegistrationId != first.RegistrationId || read.Registration.StoragePath != first.StoragePath || read.Registration.Status != "pending" || read.UploadedObject.State != modelv1.TrainedArtifactObjectObservation_STATE_NOT_REQUESTED {
		t.Fatalf("wrong original read %v", read)
	}
	row := func() string {
		t.Helper()
		var raw string
		if err := pool.QueryRow(ctx, `SELECT row_to_json(r)::text FROM public.trained_artifact_registrations r WHERE tenant_id=$1 AND id=$2`, tenant, first.RegistrationId).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := row()
	verify := proto.Clone(lookup).(*modelv1.GetTrainedArtifactRegistrationRequest)
	verify.VerifyUploadedObject = true
	for _, test := range []struct {
		name  string
		bytes []byte
		state modelv1.TrainedArtifactObjectObservation_State
	}{{"missing is only an observation", nil, modelv1.TrainedArtifactObjectObservation_STATE_MISSING}, {"different actual bytes", []byte("different model bytes"), modelv1.TrainedArtifactObjectObservation_STATE_MISMATCH}, {"actual original bytes", correct, modelv1.TrainedArtifactObjectObservation_STATE_VERIFIED}} {
		t.Run(test.name, func(t *testing.T) {
			mutex.Lock()
			uploaded = test.bytes
			mutex.Unlock()
			got, err := app.GetTrainedArtifactRegistration(principal, verify)
			if err != nil {
				t.Fatal(err)
			}
			if err := got.ValidateFor(verify); err != nil {
				t.Fatal(err)
			}
			if got.UploadedObject.State != test.state || got.Registration.Status != "pending" {
				t.Fatalf("proof=%v", got)
			}
			if row() != before {
				t.Fatal("read-only proof changed registration")
			}
		})
	}
	unavailable := service.NewModelService(nil)
	unavailable.SetTrainedArtifactRegistrar(registrar)
	got, err := unavailable.GetTrainedArtifactRegistration(principal, verify)
	if err != nil || got.UploadedObject.State != modelv1.TrainedArtifactObjectObservation_STATE_UNAVAILABLE {
		t.Fatalf("missing storage capability=%v err=%v", got, err)
	}
	ticketRequest := &modelv1.GetTrainedArtifactUploadURLRequest{TenantId: tenant, RegistrationId: first.RegistrationId, IdempotencyKey: key, ExpectedExecutionId: execution, ExpectedStoragePath: first.StoragePath}
	for range 2 {
		ticket, err := app.GetTrainedArtifactUploadURL(principal, ticketRequest)
		if err != nil || ticket.RegistrationId != first.RegistrationId || ticket.StoragePath != first.StoragePath || ticket.UploadUrl == "" {
			t.Fatalf("original ticket identity/path or URL is invalid: %v", err)
		}
	}
	wrongTicket := proto.Clone(ticketRequest).(*modelv1.GetTrainedArtifactUploadURLRequest)
	wrongTicket.ExpectedStoragePath += "-changed"
	if _, err := app.GetTrainedArtifactUploadURL(principal, wrongTicket); kratoserrors.Code(err) != 409 {
		t.Fatalf("changed ticket path err=%v", err)
	}
	wrongKey := proto.Clone(lookup).(*modelv1.GetTrainedArtifactRegistrationRequest)
	wrongKey.IdempotencyKey += "-missing"
	if _, err := app.GetTrainedArtifactRegistration(principal, wrongKey); kratoserrors.Code(err) != 404 {
		t.Fatalf("missing key err=%v", err)
	}
	wrongExecution := proto.Clone(lookup).(*modelv1.GetTrainedArtifactRegistrationRequest)
	wrongExecution.ExpectedExecutionId = uuid.NewString()
	if _, err := app.GetTrainedArtifactRegistration(principal, wrongExecution); kratoserrors.Code(err) != 409 {
		t.Fatalf("cross execution err=%v", err)
	}
	foreign := uuid.NewString()
	foreignContext := identity.WithPrincipal(ctx, identity.Principal{TenantID: foreign, Actor: "modeldev:integration", Workload: "ani-modeldev-service"})
	foreignLookup := proto.Clone(lookup).(*modelv1.GetTrainedArtifactRegistrationRequest)
	foreignLookup.TenantId = foreign
	if _, err := app.GetTrainedArtifactRegistration(foreignContext, foreignLookup); kratoserrors.Code(err) != 404 {
		t.Fatalf("foreign tenant err=%v", err)
	}
	changed := proto.Clone(prepare).(*modelv1.PrepareTrainedArtifactRequest)
	changed.ChecksumSha256 = strings.Repeat("b", 64)
	if _, err := app.PrepareTrainedArtifact(principal, changed); kratoserrors.Code(err) != 409 {
		t.Fatalf("changed content same key err=%v", err)
	}
	if row() != before {
		t.Fatal("lookup/ticket/conflict mutated original row")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.trained_artifact_registrations WHERE tenant_id=$1`, tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("registration count=%d err=%v", count, err)
	}
	mutex.Lock()
	if writes != 0 {
		t.Errorf("read/ticket sent %d object writes", writes)
	}
	if !bytes.Equal(uploaded, correct) {
		t.Fatal("unexpected external fixture bytes")
	}
	mutex.Unlock()
	complete, err := app.CompleteTrainedArtifact(principal, &modelv1.CompleteTrainedArtifactRequest{TenantId: tenant, RegistrationId: first.RegistrationId, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := app.GetTrainedArtifactRegistration(principal, verify)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Registration.Status != "ready" || ready.Registration.ModelId != complete.ModelId || ready.Registration.ModelVersionId != complete.ModelVersionId || ready.UploadedObject.State != modelv1.TrainedArtifactObjectObservation_STATE_VERIFIED {
		t.Fatalf("lost completion response read=%v", ready)
	}
	if _, err := app.GetTrainedArtifactUploadURL(principal, ticketRequest); kratoserrors.Code(err) != 409 {
		t.Fatalf("READY session issued another upload ticket: %v", err)
	}
}
