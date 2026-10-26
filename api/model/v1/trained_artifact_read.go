package modelv1

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ValidateFor binds a read receipt to the original key/execution. It does not
// interpret MISSING (or a NotFound RPC) as permission to resend a write.
func (r *GetTrainedArtifactRegistrationResponse) ValidateFor(in *GetTrainedArtifactRegistrationRequest) error {
	invalid := func() error { return fmt.Errorf("invalid original registration observation") }
	if in == nil || r == nil || r.Registration == nil || r.UploadedObject == nil || !factsID(in.TenantId) || in.IdempotencyKey == "" || in.ExpectedExecutionId == "" || len(in.ProtoReflect().GetUnknown()) != 0 || len(r.ProtoReflect().GetUnknown()) != 0 {
		return invalid()
	}
	original, observation := r.Registration, r.UploadedObject
	if len(original.ProtoReflect().GetUnknown()) != 0 || len(observation.ProtoReflect().GetUnknown()) != 0 || !factsID(original.RegistrationId) || original.IdempotencyKey != in.IdempotencyKey || original.ExecutionId != in.ExpectedExecutionId || original.SizeBytes <= 0 || original.Format != "safetensors" || original.FileName != "model.tar" || !factsPath(original.StoragePath) || !strings.HasPrefix(original.StoragePath, in.TenantId+"/") || original.Version == "" {
		return invalid()
	}
	if raw, err := hex.DecodeString(original.ChecksumSha256); err != nil || len(raw) != 32 {
		return invalid()
	}
	switch original.Status {
	case "pending", "uploaded", "failed", "aborted":
	case "ready":
		if !factsID(original.ModelId) || !factsID(original.ModelVersionId) {
			return invalid()
		}
	default:
		return invalid()
	}
	if original.TargetModelId != "" && !factsID(original.TargetModelId) || original.TargetModelId == "" && original.ModelName == "" {
		return invalid()
	}
	if !in.VerifyUploadedObject {
		if observation.State != TrainedArtifactObjectObservation_STATE_NOT_REQUESTED || observation.SizeBytes != 0 || observation.Sha256 != "" {
			return invalid()
		}
		return nil
	}
	switch observation.State {
	case TrainedArtifactObjectObservation_STATE_MISSING, TrainedArtifactObjectObservation_STATE_UNAVAILABLE:
		if observation.SizeBytes != 0 || observation.Sha256 != "" {
			return invalid()
		}
	case TrainedArtifactObjectObservation_STATE_VERIFIED:
		if observation.SizeBytes != original.SizeBytes || !strings.EqualFold(observation.Sha256, original.ChecksumSha256) {
			return invalid()
		}
	case TrainedArtifactObjectObservation_STATE_MISMATCH:
		if observation.SizeBytes < 0 || !factsDigest(observation.Sha256) {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}
