package model

import (
	"context"
	"errors"
	"time"
)

var (
	ErrRegistrationNotFound = errors.New("trained artifact registration not found")
	ErrRegistrationConflict = errors.New("trained artifact registration conflicts with existing request")
	ErrRegistrationTerminal = errors.New("trained artifact registration is terminal")
)

const (
	RegistrationPending  = "pending"
	RegistrationUploaded = "uploaded"
	RegistrationReady    = "ready"
	RegistrationFailed   = "failed"
	RegistrationAborted  = "aborted"
)

type TrainedArtifactRegistration struct {
	TenantID, ID, IdempotencyKey, RequestFingerprint, ExecutionID string
	TargetModelID, ModelName, DisplayName, Description            string
	Capabilities                                                  []string
	SourceModelVersionID, Version, FileName, Format               string
	StoragePath, ChecksumSHA256                                   string
	SizeBytes                                                     int64
	EngineType, EngineImage, StartupCommand                       string
	Status, ErrorMessage, ModelID, ModelVersionID                 string
	CreatedAt, UpdatedAt                                          time.Time
}

type TrainedArtifactRegistrationResult struct {
	RegistrationID, ModelID, ModelVersionID, Status, StoragePath, ChecksumSHA256 string
	SizeBytes                                                                    int64
}

type TrainedArtifactRegistrar interface {
	Prepare(context.Context, TrainedArtifactRegistration) (TrainedArtifactRegistration, error)
	Get(context.Context, string, string) (TrainedArtifactRegistration, error)
	MarkUploaded(context.Context, string, string) error
	Complete(context.Context, string, string) (TrainedArtifactRegistrationResult, error)
	Fail(context.Context, string, string, string) error
	Abort(context.Context, string, string, string) error
}

// Read-only original-key reconciliation is separate from dispatch permission.
// Not found never authorizes the caller to resend an uncertain write.
type TrainedArtifactRegistrationReader interface {
	GetByKey(context.Context, string, string) (TrainedArtifactRegistration, error)
}
