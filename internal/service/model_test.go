package service

import (
	"context"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/biz/model"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/biz/storage"
	workbiz "github.com/zhangzhe-ctrl/ani-model-service/internal/biz/work"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/identity"
)

type catalogFake struct{ created model.Record }

func (c *catalogFake) CreateModel(_ context.Context, tenant, id, name, display, desc, source string, caps []byte, idempotencyKey string) (model.Record, error) {
	c.created = model.Record{TenantID: tenant, ID: id, Name: name}
	return c.created, nil
}
func (c *catalogFake) GetModel(_ context.Context, tenant, id string) (model.Record, error) {
	return model.Record{TenantID: tenant, ID: id}, nil
}
func (c *catalogFake) ListModels(context.Context, string, string, int32) ([]model.Record, error) {
	return nil, nil
}
func (c *catalogFake) SoftDeleteModel(context.Context, string, string) error { return nil }

type referenceCheckerFake struct {
	referenced bool
	err        error
}

func (c referenceCheckerFake) HasActiveReferences(context.Context, string, string) (bool, error) {
	return c.referenced, c.err
}

type storageFake struct{}

func (storageFake) CreateUploadURL(context.Context, storage.UploadRequest) (storage.SignedURL, error) {
	return storage.SignedURL{URL: "https://upload", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (storageFake) ObjectExists(context.Context, string, string) (bool, error)   { return true, nil }
func (storageFake) VerifyChecksum(context.Context, string, string, string) error { return nil }
func (storageFake) CreateDownloadURL(context.Context, storage.DownloadRequest) (storage.SignedURL, error) {
	return storage.SignedURL{}, nil
}

type versionReaderFunc func(context.Context, string, string) (model.Version, error)

type externalVersionReaderFake struct{ version model.Version }

type importWorkFake struct{ got workbiz.Task }

func (f *importWorkFake) Create(_ context.Context, task workbiz.Task) (workbiz.Task, error) {
	f.got = task
	return task, nil
}

type importNotifierFake struct{ calls int }

func (f *importNotifierFake) Notify() { f.calls++ }

func (f versionReaderFunc) GetVersion(ctx context.Context, tenant, id string) (model.Version, error) {
	return f(ctx, tenant, id)
}
func (f externalVersionReaderFake) GetVersion(ctx context.Context, tenant, id string) (model.Version, error) {
	return f.version, nil
}
func (f externalVersionReaderFake) GetVersionByExternalRef(context.Context, string, string, string) (model.Version, error) {
	return f.version, nil
}

type versionCatalogFake struct {
	created model.Version
	ready   bool
}

func (f *versionCatalogFake) GetVersion(_ context.Context, tenant, id string) (model.Version, error) {
	if f.created.ID == id && f.created.TenantID == tenant {
		return f.created, nil
	}
	return model.Version{}, nil
}
func (f *versionCatalogFake) CreateVersion(_ context.Context, v model.Version) (model.Version, error) {
	f.created = v
	return v, nil
}
func (f *versionCatalogFake) ListVersions(context.Context, string, string, int32) ([]model.Version, error) {
	return nil, nil
}
func (f *versionCatalogFake) MarkReady(context.Context, string, string) error {
	f.ready = true
	return nil
}
func (f *versionCatalogFake) MarkError(context.Context, string, string, string) error { return nil }

type artifactFake struct{ created model.Artifact }

func (f *artifactFake) CreateArtifact(_ context.Context, a model.Artifact) (model.Artifact, error) {
	f.created = a
	return a, nil
}
func (f *artifactFake) GetArtifact(context.Context, string, string) (model.Artifact, error) {
	return f.created, nil
}

func TestGetModelVersionRequiresTrustedTenant(t *testing.T) {
	s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		t.Fatal("reader called")
		return model.Version{}, nil
	}))
	_, err := s.GetModelVersion(context.Background(), &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v"})
	if kratoserrors.Code(err) != 401 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestGetModelVersionChecksReadyArtifact(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "a", Workload: "w"})
	s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		return model.Version{TenantID: "t", ID: "v", ModelID: "m", Version: "1", Format: "gguf", Status: "pending"}, nil
	}))
	_, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v"})
	if kratoserrors.Code(err) != 412 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestGetModelVersionByExternalReference(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "a", Workload: "w"})
	checksum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s := NewModelService(externalVersionReaderFake{version: model.Version{TenantID: "t", ID: "version-uuid", ModelID: "model-uuid", ExternalModelID: "Qwen3-32B", Version: "v2", Format: "gguf", Status: "ready", ArtifactProvider: "minio", ArtifactReference: "t/Qwen3-32B/v2/model.gguf", ArtifactSHA256: checksum}})
	got, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelId: "Qwen3-32B", Version: "v2"})
	if err != nil {
		t.Fatalf("GetModelVersion() error = %v", err)
	}
	if got.GetModel().GetModelId() != "Qwen3-32B" || got.GetVersion().GetModelId() != "Qwen3-32B" || got.GetVersion().GetId() != "version-uuid" {
		t.Fatalf("response = %+v", got)
	}
}

func TestGetModelVersionRejectsMixedSelectors(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "a", Workload: "w"})
	s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		t.Fatal("reader called")
		return model.Version{}, nil
	}))
	_, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v", ModelId: "Qwen3-32B", Version: "v2"})
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestCreateModelRejectsCrossTenant(t *testing.T) {
	s := NewModelCatalogService(nil, &catalogFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.CreateModel(ctx, &modelv1.CreateModelRequest{TenantId: "tenant-b", Name: "demo"})
	if kratoserrors.Code(err) != 403 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestDeleteModelRejectsUnknownReferenceState(t *testing.T) {
	s := NewModelCatalogService(nil, &catalogFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.DeleteModel(ctx, &modelv1.DeleteModelRequest{TenantId: "tenant-a", ModelId: referenceModelUUID})
	if kratoserrors.Code(err) != 503 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestDeleteModelRejectsActiveInferenceReference(t *testing.T) {
	s := NewModelCatalogService(nil, &catalogFake{})
	s.SetInferenceReferenceChecker(referenceCheckerFake{referenced: true})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.DeleteModel(ctx, &modelv1.DeleteModelRequest{TenantId: "tenant-a", ModelId: referenceModelUUID})
	if kratoserrors.Code(err) != 409 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestGetUploadURLRejectsInvalidChecksum(t *testing.T) {
	s := NewModelDownloadService(nil, storageFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.GetUploadURL(ctx, &modelv1.GetUploadURLRequest{TenantId: "tenant-a", ModelId: "m", Version: "1", FileName: "model.bin", SizeBytes: 1, ChecksumSha256: "bad"})
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestImportModelPersistsProviderPayload(t *testing.T) {
	work := &importWorkFake{}
	s := NewModelImportService(work)
	notifier := &importNotifierFake{}
	s.SetImportNotifier(notifier)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	got, err := s.ImportModel(ctx, &modelv1.ImportModelRequest{
		TenantId:       "tenant-a",
		Source:         "huggingface",
		RepoId:         "org/model",
		Revision:       "main",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("ImportModel() error = %v", err)
	}
	if got.GetStatus() != workbiz.Pending || got.GetTaskType() != "huggingface" {
		t.Fatalf("response = %+v", got)
	}
	if work.got.Source != "huggingface" || work.got.RepoID != "org/model" || work.got.Revision != "main" || work.got.IdempotencyKey != "idem-1" {
		t.Fatalf("persisted task lost provider payload: %+v", work.got)
	}
	if notifier.calls != 1 {
		t.Fatalf("notifier calls=%d, want 1", notifier.calls)
	}
}

func TestImportModelPersistsOptionalVersionBinding(t *testing.T) {
	work := &importWorkFake{}
	s := NewModelImportService(work)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	modelID := "11111111-1111-1111-1111-111111111111"
	versionID := "22222222-2222-2222-2222-222222222222"
	got, err := s.ImportModel(ctx, &modelv1.ImportModelRequest{TenantId: "tenant-a", Source: "huggingface", RepoId: "org/model#model.gguf", IdempotencyKey: "idem-bind", ModelId: modelID, ModelVersionId: versionID})
	if err != nil {
		t.Fatalf("ImportModel() error = %v", err)
	}
	if work.got.ModelID != modelID || work.got.VersionID != versionID || got.GetModelId() != modelID || got.GetModelVersionId() != versionID {
		t.Fatalf("binding lost: %+v", work.got)
	}
}

func TestCreateModelVersionFinalizesUploadedArtifact(t *testing.T) {
	versions := &versionCatalogFake{}
	artifacts := &artifactFake{}
	s := NewModelVersionService(versions)
	s.catalog = &referenceCatalog{}
	s.storage = storageFake{}
	s.SetArtifactStore(artifacts)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	checksum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := s.CreateModelVersion(ctx, &modelv1.CreateModelVersionRequest{
		TenantId: "tenant-a", ModelId: referenceModelUUID, Version: "v1", Format: "gguf",
		StoragePath: "tenant-a/model-a/v1/model.gguf", SizeBytes: 42, ChecksumSha256: checksum,
	})
	if err != nil {
		t.Fatalf("CreateModelVersion() error = %v", err)
	}
	if !versions.ready || got.GetStatus() != "ready" || artifacts.created.Reference != "tenant-a/model-a/v1/model.gguf" || artifacts.created.SHA256 != checksum {
		t.Fatalf("version=%+v ready=%v artifact=%+v", got, versions.ready, artifacts.created)
	}
}

func TestGetModelVersionDistinguishesStoreFailure(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "service:ani-inference-service", Workload: "ani-inference-service"})
	for _, tc := range []struct {
		name  string
		cause error
		code  int
	}{
		{"missing", pgx.ErrNoRows, 404}, {"database-timeout", context.DeadlineExceeded, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) { return model.Version{}, tc.cause }))
			out, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v"})
			if out != nil || kratoserrors.Code(err) != tc.code {
				t.Fatalf("out=%v err=%v", out, err)
			}
		})
	}
}
