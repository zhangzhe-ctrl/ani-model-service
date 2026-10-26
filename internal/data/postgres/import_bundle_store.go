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
	"github.com/liangzai006/ani-model-service/internal/biz/idempotency"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
)

// BundleImportStore persists the model, version, and task aggregate in one
// PostgreSQL transaction. External providers and workers remain outside this
// adapter; they only observe the durable task after commit.
type BundleImportStore struct{ pool *pgxpool.Pool }

var errBundleAggregateMissing = errors.New("model bundle aggregate is not present")

func NewBundleImportStore(pool *pgxpool.Pool) *BundleImportStore {
	return &BundleImportStore{pool: pool}
}

func (s *BundleImportStore) ImportBundle(ctx context.Context, in modelbiz.BundleImport) (modelbiz.BundleImportResult, error) {
	if err := in.Validate(); err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	tenant, err := uuid.Parse(in.TenantID)
	if err != nil {
		return modelbiz.BundleImportResult{}, fmt.Errorf("tenant_id: %w", err)
	}
	fingerprint, err := bundleFingerprint(in)
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	ids := bundleIDs{tenant: tenant, model: uuid.New(), version: uuid.New(), task: uuid.New()}
	// A completed aggregate is a pure replay. Check it in a read-only
	// transaction before opening the write transaction used for first import.
	if replay, replayErr := s.replayBundle(ctx, in, fingerprint); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, errBundleAggregateMissing) {
		return modelbiz.BundleImportResult{}, replayErr
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	result, err := s.importBundleTx(ctx, tx, in, fingerprint, ids)
	if err == nil {
		if err = tx.Commit(ctx); err == nil {
			return result, nil
		}
	}
	_ = tx.Rollback(ctx)
	// A concurrent request may win the idempotency insert. Re-read the
	// committed aggregate so both callers receive the same durable IDs.
	if isUniqueViolation(err) {
		if replay, replayErr := s.replayBundle(ctx, in, fingerprint); replayErr == nil {
			return replay, nil
		}
	}
	return modelbiz.BundleImportResult{}, err
}

type bundleIDs struct {
	tenant, model, version, task uuid.UUID
}

func (s *BundleImportStore) importBundleTx(ctx context.Context, tx pgx.Tx, in modelbiz.BundleImport, fingerprint string, ids bundleIDs) (modelbiz.BundleImportResult, error) {
	q := New(tx)
	modelRow, modelErr := q.GetBundleModelByIdempotency(ctx, GetBundleModelByIdempotencyParams{TenantID: uuidType(ids.tenant), IdempotencyKey: in.IdempotencyKey})
	versionRow, versionErr := q.GetBundleVersionByIdempotency(ctx, GetBundleVersionByIdempotencyParams{TenantID: uuidType(ids.tenant), IdempotencyKey: in.IdempotencyKey})
	taskRow, taskErr := q.GetBundleTaskByIdempotency(ctx, GetBundleTaskByIdempotencyParams{TenantID: uuidType(ids.tenant), IdempotencyKey: in.IdempotencyKey})
	modelExists, versionExists, taskExists := modelErr == nil, versionErr == nil, taskErr == nil
	for _, lookupErr := range []error{modelErr, versionErr, taskErr} {
		if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
			return modelbiz.BundleImportResult{}, lookupErr
		}
	}
	if modelExists || versionExists || taskExists {
		if !(modelExists && versionExists && taskExists) || modelRow.RequestFingerprint != fingerprint || versionRow.RequestFingerprint != fingerprint || taskRow.RequestFingerprint != fingerprint {
			return modelbiz.BundleImportResult{}, bundleConflict()
		}
		return bundleResult(in, modelRow, versionRow, taskRow.ID, taskRow.TaskType, taskRow.Status), nil
	}

	modelRow, err := q.CreateBundleModel(ctx, CreateBundleModelParams{
		TenantID: uuidType(ids.tenant), ID: uuidType(ids.model), ModelID: in.ExternalModelID,
		Name: in.Name, DisplayName: in.DisplayName, Description: in.Description, Source: in.Provider,
		SourceRepoID: in.RepoID, Capabilities: mustJSON(in.Capabilities), IdempotencyKey: in.IdempotencyKey,
		RequestFingerprint: fingerprint,
	})
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	versionRow, err = q.CreateBundleVersion(ctx, CreateBundleVersionParams{
		TenantID: uuidType(ids.tenant), ID: uuidType(ids.version), ModelID: uuidType(ids.model), Version: in.Version,
		Format: in.Format, EngineType: in.EngineType, EngineImage: in.EngineImage, StartupCommand: in.StartupCommand,
		StartupArgs: []byte("[]"), IdempotencyKey: in.IdempotencyKey, RequestFingerprint: fingerprint,
	})
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	createdTask, err := q.CreateBundleTask(ctx, CreateBundleTaskParams{
		TenantID: uuidType(ids.tenant), ID: uuidType(ids.task), ModelID: uuidType(ids.model), ModelVersionID: uuidType(ids.version),
		TaskType: in.Provider, Source: in.Provider, RepoID: in.RepoID, Revision: in.Revision,
		IdempotencyKey: in.IdempotencyKey, RequestFingerprint: fingerprint,
	})
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	return bundleResult(in, modelRow, versionRow, createdTask.ID, createdTask.TaskType, createdTask.Status), nil
}

func (s *BundleImportStore) replayBundle(ctx context.Context, in modelbiz.BundleImport, fingerprint string) (modelbiz.BundleImportResult, error) {
	tenant, err := uuid.Parse(in.TenantID)
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return modelbiz.BundleImportResult{}, err
	}
	q := New(tx)
	modelRow, modelErr := q.GetBundleModelByIdempotency(ctx, GetBundleModelByIdempotencyParams{TenantID: uuidType(tenant), IdempotencyKey: in.IdempotencyKey})
	versionRow, versionErr := q.GetBundleVersionByIdempotency(ctx, GetBundleVersionByIdempotencyParams{TenantID: uuidType(tenant), IdempotencyKey: in.IdempotencyKey})
	taskRow, taskErr := q.GetBundleTaskByIdempotency(ctx, GetBundleTaskByIdempotencyParams{TenantID: uuidType(tenant), IdempotencyKey: in.IdempotencyKey})
	if modelErr != nil || versionErr != nil || taskErr != nil {
		_ = tx.Rollback(ctx)
		if modelErr != nil && !errors.Is(modelErr, pgx.ErrNoRows) {
			return modelbiz.BundleImportResult{}, modelErr
		}
		if versionErr != nil && !errors.Is(versionErr, pgx.ErrNoRows) {
			return modelbiz.BundleImportResult{}, versionErr
		}
		if taskErr != nil && !errors.Is(taskErr, pgx.ErrNoRows) {
			return modelbiz.BundleImportResult{}, taskErr
		}
		if modelErr == nil || versionErr == nil || taskErr == nil {
			return modelbiz.BundleImportResult{}, bundleConflict()
		}
		return modelbiz.BundleImportResult{}, errBundleAggregateMissing
	}
	if modelRow.RequestFingerprint != fingerprint || versionRow.RequestFingerprint != fingerprint || taskRow.RequestFingerprint != fingerprint {
		_ = tx.Rollback(ctx)
		return modelbiz.BundleImportResult{}, bundleConflict()
	}
	result := bundleResult(in, modelRow, versionRow, taskRow.ID, taskRow.TaskType, taskRow.Status)
	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return modelbiz.BundleImportResult{}, err
	}
	return result, nil
}

func bundleFingerprint(in modelbiz.BundleImport) (string, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("bundle fingerprint: %w", err)
	}
	return idempotency.Fingerprint(payload), nil
}

func mustJSON(value []string) []byte {
	b, _ := json.Marshal(value)
	return b
}

func bundleConflict() error {
	return &pgconn.PgError{Code: "23505", Message: "model bundle idempotency key payload conflict"}
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func bundleResult(in modelbiz.BundleImport, modelRow Model, versionRow ModelVersion, taskID pgtype.UUID, taskType, taskStatus string) modelbiz.BundleImportResult {
	version := versionFromRow(versionRow, in.TenantID)
	version.ExternalModelID = modelRow.ModelID
	return modelbiz.BundleImportResult{
		Model: record(modelRow), Version: version,
		TaskID: uuidString(taskID), TaskType: taskType, TaskStatus: taskStatus,
	}
}

func uuidString(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	return uuid.UUID(value.Bytes).String()
}

var _ modelbiz.BundleImporter = (*BundleImportStore)(nil)
