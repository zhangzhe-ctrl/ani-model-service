package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
)

type TrainedArtifactStore struct{ pool *pgxpool.Pool }

func NewTrainedArtifactStore(pool *pgxpool.Pool) *TrainedArtifactStore {
	return &TrainedArtifactStore{pool: pool}
}

const trainedArtifactColumns = `tenant_id, id, idempotency_key, request_fingerprint,
 execution_id, target_model_id, model_name, display_name, description, capabilities,
 source_model_version_id, version, file_name, format, storage_path, size_bytes,
 checksum_sha256, engine_type, engine_image, startup_command, status, error_message,
 model_id, model_version_id, created_at, updated_at`

func (s *TrainedArtifactStore) Prepare(ctx context.Context, in modelbiz.TrainedArtifactRegistration) (modelbiz.TrainedArtifactRegistration, error) {
	tenant, id, err := parseRegistrationIDs(in.TenantID, in.ID)
	if err != nil {
		return modelbiz.TrainedArtifactRegistration{}, err
	}
	var existing modelbiz.TrainedArtifactRegistration
	err = scanTrainedArtifact(s.pool.QueryRow(ctx, `SELECT `+trainedArtifactColumns+` FROM public.trained_artifact_registrations WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, in.IdempotencyKey), &existing)
	if err == nil {
		if existing.RequestFingerprint != in.RequestFingerprint {
			return modelbiz.TrainedArtifactRegistration{}, modelbiz.ErrRegistrationConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return modelbiz.TrainedArtifactRegistration{}, err
	}
	var target any
	if in.TargetModelID != "" {
		targetID, parseErr := uuid.Parse(in.TargetModelID)
		if parseErr != nil {
			return modelbiz.TrainedArtifactRegistration{}, fmt.Errorf("target_model_id: %w", parseErr)
		}
		target = targetID
	}
	var source any
	if in.SourceModelVersionID != "" {
		sourceID, parseErr := uuid.Parse(in.SourceModelVersionID)
		if parseErr != nil {
			return modelbiz.TrainedArtifactRegistration{}, fmt.Errorf("source_model_version_id: %w", parseErr)
		}
		source = sourceID
	}
	caps, err := json.Marshal(in.Capabilities)
	if err != nil {
		return modelbiz.TrainedArtifactRegistration{}, err
	}
	row := s.pool.QueryRow(ctx, `INSERT INTO public.trained_artifact_registrations
 (tenant_id,id,idempotency_key,request_fingerprint,execution_id,target_model_id,model_name,display_name,description,capabilities,source_model_version_id,version,file_name,format,storage_path,size_bytes,checksum_sha256,engine_type,engine_image,startup_command,status)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,'pending')
 RETURNING `+trainedArtifactColumns, tenant, id, in.IdempotencyKey, in.RequestFingerprint, in.ExecutionID, target, in.ModelName, in.DisplayName, in.Description, caps, source, in.Version, in.FileName, in.Format, in.StoragePath, in.SizeBytes, in.ChecksumSHA256, in.EngineType, in.EngineImage, in.StartupCommand)
	var result modelbiz.TrainedArtifactRegistration
	if err := scanTrainedArtifact(row, &result); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return s.Prepare(ctx, in)
		}
		return modelbiz.TrainedArtifactRegistration{}, err
	}
	return result, nil
}

func (s *TrainedArtifactStore) Get(ctx context.Context, tenantID, registrationID string) (modelbiz.TrainedArtifactRegistration, error) {
	tenant, id, err := parseRegistrationIDs(tenantID, registrationID)
	if err != nil {
		return modelbiz.TrainedArtifactRegistration{}, err
	}
	var result modelbiz.TrainedArtifactRegistration
	err = scanTrainedArtifact(s.pool.QueryRow(ctx, `SELECT `+trainedArtifactColumns+` FROM public.trained_artifact_registrations WHERE tenant_id=$1 AND id=$2`, tenant, id), &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return modelbiz.TrainedArtifactRegistration{}, modelbiz.ErrRegistrationNotFound
	}
	return result, err
}

func (s *TrainedArtifactStore) GetByKey(ctx context.Context, tenantID, key string) (modelbiz.TrainedArtifactRegistration, error) {
	tenant, err := uuid.Parse(tenantID)
	if err != nil {
		return modelbiz.TrainedArtifactRegistration{}, err
	}
	var result modelbiz.TrainedArtifactRegistration
	err = scanTrainedArtifact(s.pool.QueryRow(ctx, `SELECT `+trainedArtifactColumns+` FROM public.trained_artifact_registrations WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, key), &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return modelbiz.TrainedArtifactRegistration{}, modelbiz.ErrRegistrationNotFound
	}
	return result, err
}

func (s *TrainedArtifactStore) MarkUploaded(ctx context.Context, tenantID, registrationID string) error {
	tenant, id, err := parseRegistrationIDs(tenantID, registrationID)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE public.trained_artifact_registrations
 SET status='uploaded',updated_at=clock_timestamp(),error_message=''
 WHERE tenant_id=$1 AND id=$2 AND status='pending'`, tenant, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	current, err := s.Get(ctx, tenantID, registrationID)
	if err != nil {
		return err
	}
	if current.Status == modelbiz.RegistrationUploaded || current.Status == modelbiz.RegistrationReady {
		return nil
	}
	return modelbiz.ErrRegistrationTerminal
}

func (s *TrainedArtifactStore) Complete(ctx context.Context, tenantID, registrationID string) (result modelbiz.TrainedArtifactRegistrationResult, err error) {
	tenant, id, parseErr := parseRegistrationIDs(tenantID, registrationID)
	if parseErr != nil {
		return result, parseErr
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
			_ = s.Fail(context.WithoutCancel(ctx), tenantID, registrationID, err.Error())
		}
	}()
	var session modelbiz.TrainedArtifactRegistration
	if err = scanTrainedArtifact(tx.QueryRow(ctx, `SELECT `+trainedArtifactColumns+` FROM public.trained_artifact_registrations WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id), &session); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = modelbiz.ErrRegistrationNotFound
		}
		return result, err
	}
	if session.Status == modelbiz.RegistrationReady {
		return registrationResult(session), tx.Commit(ctx)
	}
	if session.Status == modelbiz.RegistrationFailed || session.Status == modelbiz.RegistrationAborted {
		return result, modelbiz.ErrRegistrationTerminal
	}

	modelID := session.TargetModelID
	if modelID == "" {
		modelID = uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO public.models (tenant_id,id,model_id,name,display_name,description,source,capabilities,status,total_size_bytes) VALUES ($1,$2,$3,$4,$5,$6,'upload',$7,'pending',0)`, tenant, uuid.MustParse(modelID), session.ModelName, session.ModelName, session.DisplayName, session.Description, mustRegistrationJSON(session.Capabilities))
	} else {
		var locked uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM public.models WHERE tenant_id=$1 AND id=$2 AND status <> 'deleted' FOR UPDATE`, tenant, uuid.MustParse(modelID)).Scan(&locked)
	}
	if err != nil {
		return result, mapRegistrationWriteError(err)
	}
	versionID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO public.model_versions (tenant_id,id,model_id,version,format,status,size_bytes,checksum_sha256,engine_type,engine_image,startup_command) VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$8,$9,$10)`, tenant, uuid.MustParse(versionID), uuid.MustParse(modelID), session.Version, session.Format, session.SizeBytes, session.ChecksumSHA256, session.EngineType, session.EngineImage, session.StartupCommand)
	if err != nil {
		return result, mapRegistrationWriteError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.model_artifacts (tenant_id,id,model_version_id,provider,reference,format,size_bytes,sha256) VALUES ($1,$2,$3,'upload',$4,$5,$6,$7)`, tenant, uuid.New(), uuid.MustParse(versionID), session.StoragePath, session.Format, session.SizeBytes, session.ChecksumSHA256)
	if err != nil {
		return result, mapRegistrationWriteError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE public.models SET total_size_bytes=total_size_bytes+$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, tenant, uuid.MustParse(modelID), session.SizeBytes); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE public.model_versions SET status='ready',updated_at=clock_timestamp(),error_message='' WHERE tenant_id=$1 AND id=$2`, tenant, uuid.MustParse(versionID)); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE public.models SET status='ready',updated_at=clock_timestamp(),error_message='' WHERE tenant_id=$1 AND id=$2`, tenant, uuid.MustParse(modelID)); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE public.trained_artifact_registrations SET status='ready',model_id=$3,model_version_id=$4,updated_at=clock_timestamp(),error_message='' WHERE tenant_id=$1 AND id=$2`, tenant, id, uuid.MustParse(modelID), uuid.MustParse(versionID)); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	session.ModelID, session.ModelVersionID, session.Status = modelID, versionID, modelbiz.RegistrationReady
	return registrationResult(session), nil
}

func (s *TrainedArtifactStore) Fail(ctx context.Context, tenantID, registrationID, message string) error {
	tenant, id, err := parseRegistrationIDs(tenantID, registrationID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE public.trained_artifact_registrations SET status='failed',error_message=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND status IN ('pending','uploaded')`, tenant, id, message)
	return err
}

func (s *TrainedArtifactStore) Abort(ctx context.Context, tenantID, registrationID, reason string) error {
	tenant, id, err := parseRegistrationIDs(tenantID, registrationID)
	if err != nil {
		return err
	}
	commandTag, err := s.pool.Exec(ctx, `UPDATE public.trained_artifact_registrations SET status='aborted',error_message=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND status IN ('pending','uploaded')`, tenant, id, reason)
	if err != nil {
		return err
	}
	if commandTag.RowsAffected() == 0 {
		_, err = s.Get(ctx, tenantID, registrationID)
		if errors.Is(err, modelbiz.ErrRegistrationNotFound) {
			return err
		}
		return modelbiz.ErrRegistrationTerminal
	}
	return nil
}

func registrationResult(s modelbiz.TrainedArtifactRegistration) modelbiz.TrainedArtifactRegistrationResult {
	return modelbiz.TrainedArtifactRegistrationResult{RegistrationID: s.ID, ModelID: s.ModelID, ModelVersionID: s.ModelVersionID, Status: s.Status, StoragePath: s.StoragePath, SizeBytes: s.SizeBytes, ChecksumSHA256: s.ChecksumSHA256}
}

func parseRegistrationIDs(tenantID, registrationID string) (uuid.UUID, uuid.UUID, error) {
	tenant, err := uuid.Parse(tenantID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("tenant_id: %w", err)
	}
	id, err := uuid.Parse(registrationID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("registration_id: %w", err)
	}
	return tenant, id, nil
}

func mustRegistrationJSON(values []string) []byte {
	b, _ := json.Marshal(values)
	return b
}

func mapRegistrationWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return modelbiz.ErrRegistrationConflict
	}
	return err
}

type trainedArtifactScanner interface {
	Scan(...any) error
}

func scanTrainedArtifact(row trainedArtifactScanner, out *modelbiz.TrainedArtifactRegistration) error {
	var tenant, id, target, source, modelID, versionID pgtype.UUID
	var caps []byte
	err := row.Scan(&tenant, &id, &out.IdempotencyKey, &out.RequestFingerprint, &out.ExecutionID, &target, &out.ModelName, &out.DisplayName, &out.Description, &caps, &source, &out.Version, &out.FileName, &out.Format, &out.StoragePath, &out.SizeBytes, &out.ChecksumSHA256, &out.EngineType, &out.EngineImage, &out.StartupCommand, &out.Status, &out.ErrorMessage, &modelID, &versionID, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return err
	}
	out.TenantID = uuid.UUID(tenant.Bytes).String()
	out.ID = uuid.UUID(id.Bytes).String()
	if target.Valid {
		out.TargetModelID = uuid.UUID(target.Bytes).String()
	}
	if source.Valid {
		out.SourceModelVersionID = uuid.UUID(source.Bytes).String()
	}
	if modelID.Valid {
		out.ModelID = uuid.UUID(modelID.Bytes).String()
	}
	if versionID.Valid {
		out.ModelVersionID = uuid.UUID(versionID.Bytes).String()
	}
	_ = json.Unmarshal(caps, &out.Capabilities)
	return nil
}
