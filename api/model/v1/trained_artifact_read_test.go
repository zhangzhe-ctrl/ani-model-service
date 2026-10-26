package modelv1

import (
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestOriginalRegistrationObservationBindsKeyAndExecution(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	id := "22222222-2222-4222-8222-222222222222"
	request := &GetTrainedArtifactRegistrationRequest{TenantId: tenant, IdempotencyKey: "original-key", ExpectedExecutionId: "original-execution", VerifyUploadedObject: true}
	result := &GetTrainedArtifactRegistrationResponse{Registration: &TrainedArtifactRegistration{RegistrationId: id, IdempotencyKey: request.IdempotencyKey, ExecutionId: request.ExpectedExecutionId, ModelName: "original-target", Version: "original-version", FileName: "model.tar", Format: "safetensors", SizeBytes: 5, ChecksumSha256: strings.Repeat("a", 64), StoragePath: tenant + "/trained-artifacts/" + id + "/model.tar", Status: "pending"}, UploadedObject: &TrainedArtifactObjectObservation{State: TrainedArtifactObjectObservation_STATE_VERIFIED, SizeBytes: 5, Sha256: strings.Repeat("a", 64)}}
	if err := result.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*GetTrainedArtifactRegistrationResponse)
	}{
		{"key", func(r *GetTrainedArtifactRegistrationResponse) { r.Registration.IdempotencyKey = "new-key" }},
		{"execution", func(r *GetTrainedArtifactRegistrationResponse) { r.Registration.ExecutionId = "other-execution" }},
		{"scope", func(r *GetTrainedArtifactRegistrationResponse) { r.Registration.StoragePath = "other/path/model.tar" }},
		{"proof bytes", func(r *GetTrainedArtifactRegistrationResponse) { r.UploadedObject.SizeBytes++ }},
		{"proof digest", func(r *GetTrainedArtifactRegistrationResponse) { r.UploadedObject.Sha256 = strings.Repeat("b", 64) }},
		{"unknown proof", func(r *GetTrainedArtifactRegistrationResponse) {
			r.UploadedObject.State = TrainedArtifactObjectObservation_State(99)
		}},
		{"ready result missing IDs", func(r *GetTrainedArtifactRegistrationResponse) { r.Registration.Status = "ready" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := proto.Clone(result).(*GetTrainedArtifactRegistrationResponse)
			test.change(changed)
			if err := changed.ValidateFor(request); err == nil {
				t.Fatal("changed original observation accepted")
			}
		})
	}
	for _, state := range []TrainedArtifactObjectObservation_State{TrainedArtifactObjectObservation_STATE_MISSING, TrainedArtifactObjectObservation_STATE_UNAVAILABLE} {
		changed := proto.Clone(result).(*GetTrainedArtifactRegistrationResponse)
		changed.UploadedObject = &TrainedArtifactObjectObservation{State: state}
		if err := changed.ValidateFor(request); err != nil {
			t.Fatal(err)
		}
	}
}
