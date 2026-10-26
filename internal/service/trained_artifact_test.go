package service

import (
	"context"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/biz/storage"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

const trainedArtifactTenant = "tenant-trained"

type trainedArtifactRegistrarFake struct {
	session       model.TrainedArtifactRegistration
	prepareCalls  int
	uploadedCalls int
	completeCalls int
	failReason    string
	abortReason   string
}

func (f *trainedArtifactRegistrarFake) Prepare(_ context.Context, in model.TrainedArtifactRegistration) (model.TrainedArtifactRegistration, error) {
	f.prepareCalls++
	if f.session.ID != "" {
		if f.session.RequestFingerprint != in.RequestFingerprint {
			return model.TrainedArtifactRegistration{}, model.ErrRegistrationConflict
		}
		return f.session, nil
	}
	f.session = in
	f.session.Status = model.RegistrationPending
	return f.session, nil
}

func (f *trainedArtifactRegistrarFake) Get(context.Context, string, string) (model.TrainedArtifactRegistration, error) {
	if f.session.ID == "" {
		return model.TrainedArtifactRegistration{}, model.ErrRegistrationNotFound
	}
	return f.session, nil
}

func (f *trainedArtifactRegistrarFake) MarkUploaded(context.Context, string, string) error {
	f.uploadedCalls++
	f.session.Status = model.RegistrationUploaded
	return nil
}

func (f *trainedArtifactRegistrarFake) Complete(context.Context, string, string) (model.TrainedArtifactRegistrationResult, error) {
	f.completeCalls++
	f.session.Status = model.RegistrationReady
	f.session.ModelID = "model-trained"
	f.session.ModelVersionID = "version-trained"
	return model.TrainedArtifactRegistrationResult{
		RegistrationID: f.session.ID,
		ModelID:        f.session.ModelID,
		ModelVersionID: f.session.ModelVersionID,
		Status:         f.session.Status,
		StoragePath:    f.session.StoragePath,
		SizeBytes:      f.session.SizeBytes,
		ChecksumSHA256: f.session.ChecksumSHA256,
	}, nil
}

func (f *trainedArtifactRegistrarFake) Fail(context.Context, string, string, string) error {
	f.failReason = "failed"
	return nil
}

func (f *trainedArtifactRegistrarFake) Abort(context.Context, string, string, string) error {
	f.abortReason = "aborted"
	return nil
}

type trainedArtifactStorageFake struct {
	exists       bool
	verification storage.ObjectVerification
	verifyCalls  int
}

func (f *trainedArtifactStorageFake) CreateUploadURL(context.Context, storage.UploadRequest) (storage.SignedURL, error) {
	return storage.SignedURL{URL: "https://storage.example/upload", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *trainedArtifactStorageFake) ObjectExists(context.Context, string, string) (bool, error) {
	return f.exists, nil
}

func (f *trainedArtifactStorageFake) VerifyChecksum(context.Context, string, string, string) error {
	return nil
}

func (f *trainedArtifactStorageFake) CreateDownloadURL(context.Context, storage.DownloadRequest) (storage.SignedURL, error) {
	return storage.SignedURL{}, nil
}

func (f *trainedArtifactStorageFake) VerifyObject(context.Context, string, string, int64, string) (storage.ObjectVerification, error) {
	f.verifyCalls++
	return f.verification, nil
}

func trainedArtifactContext() context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{
		TenantID: trainedArtifactTenant,
		Actor:    "modeldev",
		Workload: "modeldev-service",
	})
}

func trainedArtifactRequest() *modelv1.PrepareTrainedArtifactRequest {
	return &modelv1.PrepareTrainedArtifactRequest{
		TenantId:       trainedArtifactTenant,
		ModelName:      "trained-qwen",
		DisplayName:    "Trained Qwen",
		Description:    "A trained Qwen artifact",
		Capabilities:   []string{"text-generation"},
		Version:        "run-1",
		FileName:       "model.tar",
		Format:         "safetensors",
		SizeBytes:      4,
		ChecksumSha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		IdempotencyKey: "trained-1",
		EngineType:     "vllm",
		EngineImage:    "registry/vllm:cpu",
		StartupCommand: "vllm serve /models --served-model-name trained-qwen",
		ExecutionId:    "run-1",
	}
}

func TestPrepareTrainedArtifactRequiresCompleteRuntime(t *testing.T) {
	s := NewModelServiceWithDependencies(nil, nil, nil, &trainedArtifactStorageFake{}, nil)
	s.SetTrainedArtifactRegistrar(&trainedArtifactRegistrarFake{})

	in := trainedArtifactRequest()
	in.EngineImage = ""
	_, err := s.PrepareTrainedArtifact(trainedArtifactContext(), in)
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%d err=%v, want 400", kratoserrors.Code(err), err)
	}
}

func TestPrepareTrainedArtifactRequiresNewModelMetadata(t *testing.T) {
	s := NewModelServiceWithDependencies(nil, nil, nil, &trainedArtifactStorageFake{}, nil)
	s.SetTrainedArtifactRegistrar(&trainedArtifactRegistrarFake{})

	in := trainedArtifactRequest()
	in.DisplayName = ""
	_, err := s.PrepareTrainedArtifact(trainedArtifactContext(), in)
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%d err=%v, want 400", kratoserrors.Code(err), err)
	}
}

func TestPrepareTrainedArtifactRejectsInvalidInheritedRuntime(t *testing.T) {
	reader := versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		return model.Version{
			TenantID: trainedArtifactTenant,
			ID:       "source-version",
			Status:   model.VersionPending,
		}, nil
	})
	s := NewModelServiceWithDependencies(reader, nil, nil, &trainedArtifactStorageFake{}, nil)
	s.SetTrainedArtifactRegistrar(&trainedArtifactRegistrarFake{})

	in := trainedArtifactRequest()
	in.EngineType = ""
	in.EngineImage = ""
	in.StartupCommand = ""
	in.SourceModelVersionId = "source-version"
	_, err := s.PrepareTrainedArtifact(trainedArtifactContext(), in)
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%d err=%v, want 400", kratoserrors.Code(err), err)
	}
}

func TestPrepareTrainedArtifactReplaysIdempotentlyWithoutChangingObjectPath(t *testing.T) {
	registrar := &trainedArtifactRegistrarFake{}
	s := NewModelServiceWithDependencies(nil, nil, nil, &trainedArtifactStorageFake{}, nil)
	s.SetTrainedArtifactRegistrar(registrar)

	first, err := s.PrepareTrainedArtifact(trainedArtifactContext(), trainedArtifactRequest())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	second, err := s.PrepareTrainedArtifact(trainedArtifactContext(), trainedArtifactRequest())
	if err != nil {
		t.Fatalf("replay prepare: %v", err)
	}
	if first.GetRegistrationId() != second.GetRegistrationId() || first.GetStoragePath() != second.GetStoragePath() {
		t.Fatalf("replay changed registration: first=%+v second=%+v", first, second)
	}
	if registrar.prepareCalls != 2 {
		t.Fatalf("prepare calls=%d, want 2", registrar.prepareCalls)
	}
}

func TestPrepareTrainedArtifactRejectsChangedPayloadForSameIdempotencyKey(t *testing.T) {
	registrar := &trainedArtifactRegistrarFake{}
	s := NewModelServiceWithDependencies(nil, nil, nil, &trainedArtifactStorageFake{}, nil)
	s.SetTrainedArtifactRegistrar(registrar)

	first := trainedArtifactRequest()
	if _, err := s.PrepareTrainedArtifact(trainedArtifactContext(), first); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	second := trainedArtifactRequest()
	second.ExecutionId = "different-execution"
	_, err := s.PrepareTrainedArtifact(trainedArtifactContext(), second)
	if kratoserrors.Code(err) != 409 {
		t.Fatalf("code=%d err=%v, want 409", kratoserrors.Code(err), err)
	}
}

func TestCompleteTrainedArtifactRejectsObjectIntegrityMismatch(t *testing.T) {
	registrar := &trainedArtifactRegistrarFake{}
	storageFake := &trainedArtifactStorageFake{
		exists:       true,
		verification: storage.ObjectVerification{SizeBytes: 3, SHA256: "wrong", Matches: false},
	}
	s := NewModelServiceWithDependencies(nil, nil, nil, storageFake, nil)
	s.SetTrainedArtifactRegistrar(registrar)

	_, err := s.PrepareTrainedArtifact(trainedArtifactContext(), trainedArtifactRequest())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	_, err = s.CompleteTrainedArtifact(trainedArtifactContext(), &modelv1.CompleteTrainedArtifactRequest{
		TenantId: trainedArtifactTenant, RegistrationId: registrar.session.ID,
	})
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%d err=%v, want 400", kratoserrors.Code(err), err)
	}
	if registrar.failReason == "" || registrar.completeCalls != 0 || storageFake.verifyCalls != 1 {
		t.Fatalf("failure handling = fail=%q complete=%d verify=%d", registrar.failReason, registrar.completeCalls, storageFake.verifyCalls)
	}
}

func TestCompleteTrainedArtifactReturnsReceipt(t *testing.T) {
	registrar := &trainedArtifactRegistrarFake{}
	storageFake := &trainedArtifactStorageFake{
		exists:       true,
		verification: storage.ObjectVerification{SizeBytes: 4, Matches: true},
	}
	s := NewModelServiceWithDependencies(nil, nil, nil, storageFake, nil)
	s.SetTrainedArtifactRegistrar(registrar)

	_, err := s.PrepareTrainedArtifact(trainedArtifactContext(), trainedArtifactRequest())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	got, err := s.CompleteTrainedArtifact(trainedArtifactContext(), &modelv1.CompleteTrainedArtifactRequest{
		TenantId: trainedArtifactTenant, RegistrationId: registrar.session.ID,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got.GetModelId() != "model-trained" || got.GetModelVersionId() != "version-trained" || got.GetModelVersionStatus() != model.RegistrationReady {
		t.Fatalf("receipt=%+v", got)
	}
	if registrar.uploadedCalls != 1 {
		t.Fatalf("uploaded calls=%d, want 1", registrar.uploadedCalls)
	}
}

func TestCompleteTrainedArtifactRequiresTenantPrincipal(t *testing.T) {
	s := NewModelService(nil)
	_, err := s.CompleteTrainedArtifact(context.Background(), &modelv1.CompleteTrainedArtifactRequest{TenantId: trainedArtifactTenant, RegistrationId: "registration"})
	if kratoserrors.Code(err) != 401 {
		t.Fatalf("err=%v code=%d, want unauthenticated", err, kratoserrors.Code(err))
	}
}
