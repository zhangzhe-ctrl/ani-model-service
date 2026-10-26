package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/google/uuid"
	"github.com/liangzai006/ani-model-service/internal/biz/idempotency"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/biz/storage"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

var trainedChecksumPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func (s *ModelService) PrepareTrainedArtifact(ctx context.Context, in *modelv1.PrepareTrainedArtifactRequest) (*modelv1.PrepareTrainedArtifactResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.trained == nil || s.storage == nil {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "trained artifact registration is not configured")
	}
	if in.GetIdempotencyKey() == "" || idempotency.ValidateKey(in.GetIdempotencyKey()) != nil {
		return nil, kratoserrors.New(400, "INVALID_ARGUMENT", "valid idempotency_key is required")
	}
	if in.GetVersion() == "" || strings.ContainsAny(in.GetVersion(), "\r\n") || in.GetFileName() != "model.tar" || in.GetFormat() != "safetensors" || in.GetSizeBytes() <= 0 || !trainedChecksumPattern.MatchString(in.GetChecksumSha256()) {
		return nil, kratoserrors.New(400, "INVALID_ARGUMENT", "model.tar, safetensors, size_bytes and checksum_sha256 are required")
	}
	if in.GetTargetModelId() == "" && model.ValidateModelName(in.GetModelName()) != nil {
		return nil, kratoserrors.New(400, "INVALID_ARGUMENT", "model_name is required for a new model")
	}
	if in.GetTargetModelId() == "" && (strings.TrimSpace(in.GetDisplayName()) == "" || strings.TrimSpace(in.GetDescription()) == "") {
		return nil, kratoserrors.New(400, "INVALID_ARGUMENT", "display_name and description are required for a new model")
	}
	if in.GetTargetModelId() == "" && len(in.GetCapabilities()) == 0 {
		return nil, kratoserrors.New(400, "INVALID_ARGUMENT", "capabilities are required for a new model")
	}
	for _, capability := range in.GetCapabilities() {
		if strings.TrimSpace(capability) == "" {
			return nil, kratoserrors.New(400, "INVALID_ARGUMENT", "capabilities cannot contain empty values")
		}
	}
	var targetModelID string
	if in.GetTargetModelId() != "" {
		owner, resolveErr := s.resolveModel(ctx, p.TenantID, in.GetTargetModelId())
		if resolveErr != nil {
			return nil, resolveErr
		}
		targetModelID = owner.ID
	}
	engineType, engineImage, startupCommand, err := s.resolveTrainedRuntime(ctx, p.TenantID, in)
	if err != nil {
		return nil, err
	}
	registrationID := newUUID()
	storagePath := fmt.Sprintf("%s/trained-artifacts/%s/model.tar", p.TenantID, registrationID)
	fingerprintBytes, _ := json.Marshal(struct {
		TargetModelID, ModelName, DisplayName, Description, Version, Format, Checksum, EngineType, EngineImage, StartupCommand, SourceVersion, ExecutionID string
		Capabilities                                                                                                                                       []string
		SizeBytes                                                                                                                                          int64
	}{targetModelID, in.GetModelName(), in.GetDisplayName(), in.GetDescription(), in.GetVersion(), in.GetFormat(), in.GetChecksumSha256(), engineType, engineImage, startupCommand, in.GetSourceModelVersionId(), in.GetExecutionId(), in.GetCapabilities(), in.GetSizeBytes()})
	session, err := s.trained.Prepare(ctx, model.TrainedArtifactRegistration{
		TenantID: p.TenantID, ID: registrationID, IdempotencyKey: in.GetIdempotencyKey(), RequestFingerprint: idempotency.Fingerprint(fingerprintBytes), ExecutionID: in.GetExecutionId(), TargetModelID: targetModelID,
		ModelName: in.GetModelName(), DisplayName: in.GetDisplayName(), Description: in.GetDescription(), Capabilities: in.GetCapabilities(), SourceModelVersionID: in.GetSourceModelVersionId(), Version: in.GetVersion(), FileName: in.GetFileName(), Format: in.GetFormat(), StoragePath: storagePath, SizeBytes: in.GetSizeBytes(), ChecksumSHA256: in.GetChecksumSha256(), EngineType: engineType, EngineImage: engineImage, StartupCommand: startupCommand,
	})
	if err != nil {
		return nil, mapTrainedRegistration(err)
	}
	u, err := s.storage.CreateUploadURL(ctx, storage.UploadRequest{TenantID: p.TenantID, ObjectRef: session.StoragePath, ContentType: "application/x-tar", SizeBytes: session.SizeBytes, ChecksumSHA256: session.ChecksumSHA256})
	if err != nil {
		return nil, kratoserrors.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
	}
	if err := storage.ValidateSignedURL(u, now()); err != nil {
		return nil, kratoserrors.New(502, "STORAGE_INVALID_URL", err.Error())
	}
	return &modelv1.PrepareTrainedArtifactResponse{RegistrationId: session.ID, UploadUrl: u.URL, StoragePath: session.StoragePath, ExpiresAt: timestamppb.New(u.ExpiresAt)}, nil
}

func (s *ModelService) CompleteTrainedArtifact(ctx context.Context, in *modelv1.CompleteTrainedArtifactRequest) (*modelv1.CompleteTrainedArtifactResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.trained == nil || s.storage == nil {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "trained artifact registration is not configured")
	}
	session, err := s.trained.Get(ctx, p.TenantID, in.GetRegistrationId())
	if err != nil {
		return nil, mapTrainedRegistration(err)
	}
	if in.GetIdempotencyKey() != "" && in.GetIdempotencyKey() != session.IdempotencyKey {
		return nil, kratoserrors.New(409, "IDEMPOTENCY_CONFLICT", "idempotency_key does not match registration")
	}
	if session.Status == model.RegistrationReady {
		return trainedResultResponse(sessionResult(session)), nil
	}
	exists, err := s.storage.ObjectExists(ctx, p.TenantID, session.StoragePath)
	if err != nil {
		return nil, kratoserrors.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
	}
	if !exists {
		_ = s.trained.Fail(context.WithoutCancel(ctx), p.TenantID, session.ID, "uploaded object was not found")
		return nil, kratoserrors.NotFound("STORAGE_OBJECT_NOT_FOUND", "uploaded object was not found")
	}
	verifier, ok := s.storage.(storage.ObjectVerifier)
	if !ok {
		return nil, kratoserrors.New(503, "STORAGE_SIZE_VERIFICATION_UNAVAILABLE", "storage adapter does not support size and digest verification")
	}
	verification, err := verifier.VerifyObject(ctx, p.TenantID, session.StoragePath, session.SizeBytes, session.ChecksumSHA256)
	if err != nil {
		return nil, kratoserrors.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
	}
	if !verification.Matches {
		_ = s.trained.Fail(context.WithoutCancel(ctx), p.TenantID, session.ID, "uploaded object size or checksum does not match")
		return nil, kratoserrors.New(400, "OBJECT_INTEGRITY_MISMATCH", "uploaded object size or checksum does not match")
	}
	if err := s.trained.MarkUploaded(ctx, p.TenantID, session.ID); err != nil {
		return nil, mapTrainedRegistration(err)
	}
	result, err := s.trained.Complete(ctx, p.TenantID, session.ID)
	if err != nil {
		return nil, mapTrainedRegistration(err)
	}
	return trainedResultResponse(result), nil
}

func (s *ModelService) AbortTrainedArtifact(ctx context.Context, in *modelv1.AbortTrainedArtifactRequest) (*modelv1.AbortTrainedArtifactResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.trained == nil {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "trained artifact registration is not configured")
	}
	if err := s.trained.Abort(ctx, p.TenantID, in.GetRegistrationId(), in.GetReason()); err != nil {
		return nil, mapTrainedRegistration(err)
	}
	return &modelv1.AbortTrainedArtifactResponse{}, nil
}

func (s *ModelService) resolveTrainedRuntime(ctx context.Context, tenant string, in *modelv1.PrepareTrainedArtifactRequest) (string, string, string, error) {
	provided := 0
	for _, value := range []string{in.GetEngineType(), in.GetEngineImage(), in.GetStartupCommand()} {
		if strings.TrimSpace(value) != "" {
			provided++
		}
	}
	var source *model.Version
	if in.GetSourceModelVersionId() != "" {
		if s.reader == nil {
			return "", "", "", kratoserrors.New(503, "MODEL_STORE_UNAVAILABLE", "source model version lookup is not configured")
		}
		v, err := s.reader.GetVersion(ctx, tenant, in.GetSourceModelVersionId())
		if err != nil {
			return "", "", "", mapDomain(err)
		}
		if v.Status != model.VersionReady {
			return "", "", "", kratoserrors.New(400, "INVALID_ARGUMENT", "source_model_version_id is not ready")
		}
		source = &v
	}
	if provided == 3 {
		if err := model.ValidateEngineDefaults(in.GetEngineType(), in.GetEngineImage(), in.GetStartupCommand()); err != nil {
			return "", "", "", kratoserrors.New(400, "INVALID_ARGUMENT", "invalid inference runtime")
		}
		return in.GetEngineType(), in.GetEngineImage(), in.GetStartupCommand(), nil
	}
	if provided != 0 || source == nil {
		return "", "", "", kratoserrors.New(400, "INVALID_ARGUMENT", "complete inference runtime or source_model_version_id is required")
	}
	if model.ValidateEngineDefaults(source.EngineType, source.EngineImage, source.StartupCommand) != nil {
		return "", "", "", kratoserrors.New(400, "INVALID_ARGUMENT", "source_model_version_id does not have a ready inference runtime")
	}
	return source.EngineType, source.EngineImage, source.StartupCommand, nil
}

func trainedResultResponse(result model.TrainedArtifactRegistrationResult) *modelv1.CompleteTrainedArtifactResponse {
	return &modelv1.CompleteTrainedArtifactResponse{RegistrationId: result.RegistrationID, ModelId: result.ModelID, ModelVersionId: result.ModelVersionID, ModelVersionStatus: result.Status, StoragePath: result.StoragePath, SizeBytes: result.SizeBytes, ChecksumSha256: result.ChecksumSHA256}
}

func sessionResult(session model.TrainedArtifactRegistration) model.TrainedArtifactRegistrationResult {
	return model.TrainedArtifactRegistrationResult{RegistrationID: session.ID, ModelID: session.ModelID, ModelVersionID: session.ModelVersionID, Status: session.Status, StoragePath: session.StoragePath, SizeBytes: session.SizeBytes, ChecksumSHA256: session.ChecksumSHA256}
}

func mapTrainedRegistration(err error) error {
	switch {
	case errors.Is(err, model.ErrRegistrationNotFound):
		return kratoserrors.NotFound("REGISTRATION_NOT_FOUND", err.Error())
	case errors.Is(err, model.ErrRegistrationConflict):
		return kratoserrors.New(409, "REGISTRATION_CONFLICT", err.Error())
	case errors.Is(err, model.ErrRegistrationTerminal):
		return kratoserrors.New(409, "REGISTRATION_TERMINAL", err.Error())
	default:
		return mapStore(err)
	}
}

func newUUID() string { return uuid.NewString() }

var now = time.Now
