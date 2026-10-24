package service

import (
	"context"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/liangzai006/ani-model-service/internal/biz/idempotency"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/biz/storage"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *ModelService) GetTrainedArtifactRegistration(ctx context.Context, in *modelv1.GetTrainedArtifactRegistrationRequest) (*modelv1.GetTrainedArtifactRegistrationResponse, error) {
	principal, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if in == nil || len(in.ProtoReflect().GetUnknown()) != 0 || idempotency.ValidateKey(in.GetIdempotencyKey()) != nil || in.GetExpectedExecutionId() == "" {
		return nil, kratoserrors.BadRequest("INVALID_ARGUMENT", "original key and expected execution are required")
	}
	reader, ok := s.trained.(model.TrainedArtifactRegistrationReader)
	if !ok {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "registration reader is not configured")
	}
	original, err := reader.GetByKey(ctx, principal.TenantID, in.GetIdempotencyKey())
	if err != nil {
		return nil, mapTrainedRegistration(err)
	}
	if original.TenantID != principal.TenantID || original.IdempotencyKey != in.GetIdempotencyKey() || original.ExecutionID != in.GetExpectedExecutionId() {
		return nil, kratoserrors.Conflict("IDEMPOTENCY_CONFLICT", "original registration scope differs")
	}
	observation := &modelv1.TrainedArtifactObjectObservation{State: modelv1.TrainedArtifactObjectObservation_STATE_NOT_REQUESTED}
	if in.GetVerifyUploadedObject() {
		observation = s.observeTrainedArtifactObject(ctx, original)
	}
	out := &modelv1.GetTrainedArtifactRegistrationResponse{Registration: registrationProto(original), UploadedObject: observation}
	if out.ValidateFor(in) != nil {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "original registration observation failed integrity validation")
	}
	return out, nil
}

func (s *ModelService) GetTrainedArtifactUploadURL(ctx context.Context, in *modelv1.GetTrainedArtifactUploadURLRequest) (*modelv1.GetTrainedArtifactUploadURLResponse, error) {
	principal, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if in == nil || len(in.ProtoReflect().GetUnknown()) != 0 || in.GetRegistrationId() == "" || idempotency.ValidateKey(in.GetIdempotencyKey()) != nil || in.GetExpectedExecutionId() == "" || in.GetExpectedStoragePath() == "" {
		return nil, kratoserrors.BadRequest("INVALID_ARGUMENT", "original registration, key, execution and path are required")
	}
	if s.trained == nil || s.storage == nil {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "trained artifact dependency is not configured")
	}
	original, err := s.trained.Get(ctx, principal.TenantID, in.GetRegistrationId())
	if err != nil {
		return nil, mapTrainedRegistration(err)
	}
	if original.TenantID != principal.TenantID || original.ID != in.GetRegistrationId() || original.IdempotencyKey != in.GetIdempotencyKey() || original.ExecutionID != in.GetExpectedExecutionId() || original.StoragePath != in.GetExpectedStoragePath() {
		return nil, kratoserrors.Conflict("IDEMPOTENCY_CONFLICT", "original registration scope differs")
	}
	if original.Status != model.RegistrationPending {
		return nil, kratoserrors.Conflict("REGISTRATION_TERMINAL", "original upload window is not pending")
	}
	ticket, err := s.storage.CreateUploadURL(ctx, storage.UploadRequest{TenantID: principal.TenantID, ObjectRef: original.StoragePath, ContentType: "application/x-tar", SizeBytes: original.SizeBytes, ChecksumSHA256: original.ChecksumSHA256})
	if err != nil || storage.ValidateSignedURL(ticket, now()) != nil {
		return nil, kratoserrors.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "original upload ticket is unavailable")
	}
	return &modelv1.GetTrainedArtifactUploadURLResponse{RegistrationId: original.ID, StoragePath: original.StoragePath, UploadUrl: ticket.URL, ExpiresAt: timestamppb.New(ticket.ExpiresAt)}, nil
}

func (s *ModelService) observeTrainedArtifactObject(ctx context.Context, original model.TrainedArtifactRegistration) *modelv1.TrainedArtifactObjectObservation {
	result := &modelv1.TrainedArtifactObjectObservation{State: modelv1.TrainedArtifactObjectObservation_STATE_UNAVAILABLE}
	if s.storage == nil {
		return result
	}
	exists, err := s.storage.ObjectExists(ctx, original.TenantID, original.StoragePath)
	if err != nil {
		return result
	}
	if !exists {
		result.State = modelv1.TrainedArtifactObjectObservation_STATE_MISSING
		return result
	}
	verifier, ok := s.storage.(storage.ObjectVerifier)
	if !ok {
		return result
	}
	actual, err := verifier.VerifyObject(ctx, original.TenantID, original.StoragePath, original.SizeBytes, original.ChecksumSHA256)
	if err != nil {
		return result
	}
	result.SizeBytes, result.Sha256 = actual.SizeBytes, actual.SHA256
	result.State = modelv1.TrainedArtifactObjectObservation_STATE_MISMATCH
	if actual.Matches {
		result.State = modelv1.TrainedArtifactObjectObservation_STATE_VERIFIED
	}
	return result
}

func registrationProto(r model.TrainedArtifactRegistration) *modelv1.TrainedArtifactRegistration {
	return &modelv1.TrainedArtifactRegistration{RegistrationId: r.ID, IdempotencyKey: r.IdempotencyKey, ExecutionId: r.ExecutionID, TargetModelId: r.TargetModelID, ModelName: r.ModelName, DisplayName: r.DisplayName, Description: r.Description, Capabilities: append([]string{}, r.Capabilities...), SourceModelVersionId: r.SourceModelVersionID, Version: r.Version, FileName: r.FileName, Format: r.Format, SizeBytes: r.SizeBytes, ChecksumSha256: r.ChecksumSHA256, StoragePath: r.StoragePath, Status: r.Status, ModelId: r.ModelID, ModelVersionId: r.ModelVersionID, EngineType: r.EngineType, EngineImage: r.EngineImage, StartupCommand: r.StartupCommand, RequestFingerprint: r.RequestFingerprint, ErrorMessage: r.ErrorMessage}
}
