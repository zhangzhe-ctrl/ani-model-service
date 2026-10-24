package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/liangzai006/ani-model-service/internal/biz/idempotency"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
)

type VersionStore struct{ pool DBTX }

func NewVersionStore(db DBTX) *VersionStore { return &VersionStore{pool: db} }

// resolvedVersionSize uses the materialized artifact size for deployment
// snapshots when it is available. Older rows can have a zero model-version
// size because the importer records the final byte count on the artifact.
func resolvedVersionSize(versionSize, artifactSize int64) int64 {
	if artifactSize > 0 {
		return artifactSize
	}
	return versionSize
}

func (s *VersionStore) GetVersion(ctx context.Context, tenant, id string) (modelbiz.Version, error) {
	t, vid, err := parseModelIDs(tenant, id)
	if err != nil {
		return modelbiz.Version{}, err
	}
	r, err := New(s.pool).GetReadyModelVersion(ctx, GetReadyModelVersionParams{TenantID: uuidType(t), ID: uuidType(vid)})
	if err != nil {
		return modelbiz.Version{}, err
	}
	var args []string
	_ = json.Unmarshal(r.StartupArgs, &args)
	return modelbiz.Version{TenantID: tenant, ID: id, ModelID: uuid.UUID(r.ModelID.Bytes).String(), ExternalModelID: r.ExternalModelID, Version: r.Version, Format: r.Format, Status: r.Status, ArtifactProvider: r.ArtifactProvider, ArtifactReference: r.ArtifactReference, ArtifactSHA256: r.ArtifactSha256, EngineType: r.EngineType, EngineImage: r.EngineImage, StartupCommand: r.StartupCommand, StartupArgs: args, SizeBytes: resolvedVersionSize(r.SizeBytes, r.ArtifactSizeBytes), IsEncrypted: r.IsEncrypted, EncryptAlgo: r.EncryptAlgo, EncryptHint: r.EncryptHint, CreatedAt: r.CreatedAt.Time}, nil
}

func (s *VersionStore) GetVersionByExternalRef(ctx context.Context, tenant, externalModelID, version string) (modelbiz.Version, error) {
	t, err := uuid.Parse(tenant)
	if err != nil {
		return modelbiz.Version{}, fmt.Errorf("tenant_id: %w", err)
	}
	r, err := New(s.pool).GetReadyModelVersionByExternalRef(ctx, GetReadyModelVersionByExternalRefParams{TenantID: uuidType(t), ModelID: externalModelID, Version: version})
	if err != nil {
		return modelbiz.Version{}, err
	}
	var args []string
	if len(r.StartupArgs) > 0 {
		if err := json.Unmarshal(r.StartupArgs, &args); err != nil {
			return modelbiz.Version{}, fmt.Errorf("unmarshal startup_args: %w", err)
		}
	}
	return modelbiz.Version{TenantID: tenant, ID: uuid.UUID(r.ID.Bytes).String(), ModelID: uuid.UUID(r.ModelID.Bytes).String(), ExternalModelID: r.ExternalModelID, Version: r.Version, Format: r.Format, Status: r.Status, ArtifactProvider: r.ArtifactProvider, ArtifactReference: r.ArtifactReference, ArtifactSHA256: r.ArtifactSha256, EngineType: r.EngineType, EngineImage: r.EngineImage, StartupCommand: r.StartupCommand, StartupArgs: args, SizeBytes: resolvedVersionSize(r.SizeBytes, r.ArtifactSizeBytes), IsEncrypted: r.IsEncrypted, EncryptAlgo: r.EncryptAlgo, EncryptHint: r.EncryptHint, CreatedAt: r.CreatedAt.Time}, nil
}
func (s *VersionStore) CreateVersion(ctx context.Context, v modelbiz.Version) (modelbiz.Version, error) {
	t, id, err := parseModelIDs(v.TenantID, v.ID)
	if err != nil {
		return modelbiz.Version{}, err
	}
	mid, err := uuid.Parse(v.ModelID)
	if err != nil {
		return modelbiz.Version{}, fmt.Errorf("model_id: %w", err)
	}
	args, _ := json.Marshal(v.StartupArgs)
	fingerprint := versionFingerprint(v)
	q := New(s.pool)
	if v.IdempotencyKey != "" {
		if existing, lookupErr := q.GetModelVersionByIdempotency(ctx, GetModelVersionByIdempotencyParams{TenantID: uuidType(t), IdempotencyKey: v.IdempotencyKey}); lookupErr == nil {
			if existing.RequestFingerprint != fingerprint {
				return modelbiz.Version{}, &pgconn.PgError{Code: "23505", Message: "idempotency key payload conflict"}
			}
			return versionFromRow(existing, v.TenantID), nil
		} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return modelbiz.Version{}, lookupErr
		}
	}
	r, err := q.CreateModelVersion(ctx, CreateModelVersionParams{TenantID: uuidType(t), ID: uuidType(id), ModelID: uuidType(mid), Version: v.Version, Format: v.Format, SizeBytes: v.SizeBytes, ChecksumSha256: v.ArtifactSHA256, EngineType: v.EngineType, EngineImage: v.EngineImage, StartupCommand: v.StartupCommand, StartupArgs: args, IdempotencyKey: v.IdempotencyKey, RequestFingerprint: fingerprint})
	if err != nil {
		return modelbiz.Version{}, err
	}
	return versionFromRow(r, v.TenantID), nil
}

func versionFingerprint(v modelbiz.Version) string {
	fingerprintBytes, _ := json.Marshal(struct {
		ModelID, Version, Format, ArtifactReference, ArtifactSHA256, EngineType, EngineImage, StartupCommand string
		StartupArgs                                                                                          []string
	}{v.ModelID, v.Version, v.Format, v.ArtifactReference, v.ArtifactSHA256, v.EngineType, v.EngineImage, v.StartupCommand, v.StartupArgs})
	return idempotency.Fingerprint(fingerprintBytes)
}

func (s *VersionStore) ListVersions(ctx context.Context, tenant, model string, options modelbiz.ListOptions) ([]modelbiz.Version, error) {
	t, mid, err := parseModelIDs(tenant, model)
	if err != nil {
		return nil, err
	}
	beforeTime, beforeID, err := listBoundary(options)
	if err != nil {
		return nil, err
	}
	rs, err := New(s.pool).ListModelVersions(ctx, ListModelVersionsParams{Status: options.Status, TenantID: uuidType(t), ModelID: uuidType(mid), PageOffset: int32(options.Offset), PageLimit: options.Limit, BeforeCreatedAt: beforeTime, BeforeID: beforeID})
	if err != nil {
		return nil, err
	}
	out := make([]modelbiz.Version, len(rs))
	for i, r := range rs {
		out[i] = listedVersion(r)
	}
	if out == nil {
		out = []modelbiz.Version{}
	}
	return out, nil
}

func (s *VersionStore) CountModelVersions(ctx context.Context, tenant, model string, options modelbiz.ListOptions) (int64, error) {
	t, mid, err := parseModelIDs(tenant, model)
	if err != nil {
		return 0, err
	}
	return New(s.pool).CountModelVersions(ctx, CountModelVersionsParams{Status: options.Status, TenantID: uuidType(t), ModelID: uuidType(mid)})
}

func (s *VersionStore) ListVersionsPage(ctx context.Context, tenant, model string, options modelbiz.ListOptions) ([]modelbiz.Version, int64, error) {
	t, mid, err := parseModelIDs(tenant, model)
	if err != nil {
		return []modelbiz.Version{}, 0, err
	}
	read := func(q *Queries) ([]modelbiz.Version, int64, error) {
		count, err := q.CountModelVersions(ctx, CountModelVersionsParams{Status: options.Status, TenantID: uuidType(t), ModelID: uuidType(mid)})
		if err != nil {
			return nil, 0, err
		}
		beforeTime, beforeID, err := listBoundary(options)
		if err != nil {
			return nil, 0, err
		}
		rs, err := q.ListModelVersions(ctx, ListModelVersionsParams{Status: options.Status, TenantID: uuidType(t), ModelID: uuidType(mid), PageOffset: int32(options.Offset), PageLimit: options.Limit, BeforeCreatedAt: beforeTime, BeforeID: beforeID})
		if err != nil {
			return nil, 0, err
		}
		out := make([]modelbiz.Version, len(rs))
		for i, r := range rs {
			out[i] = listedVersion(r)
		}
		return out, count, nil
	}
	if db, ok := s.pool.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	}); ok {
		tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, 0, err
		}
		out, count, err := read(New(tx))
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, 0, err
		}
		return out, count, nil
	}
	return read(New(s.pool))
}

func listedVersion(r ListModelVersionsRow) modelbiz.Version {
	var args []string
	if len(r.StartupArgs) > 0 {
		// Log but don't fail the list operation for invalid startup args
		_ = json.Unmarshal(r.StartupArgs, &args)
	}
	return modelbiz.Version{TenantID: uuid.UUID(r.TenantID.Bytes).String(), ID: uuid.UUID(r.ID.Bytes).String(), ModelID: uuid.UUID(r.ModelID.Bytes).String(), ExternalModelID: r.ExternalModelID, Version: r.Version, Format: r.Format, Status: r.Status, ArtifactProvider: r.ArtifactProvider, ArtifactReference: r.ArtifactReference, ArtifactSHA256: r.ChecksumSha256, EngineType: r.EngineType, EngineImage: r.EngineImage, StartupCommand: r.StartupCommand, StartupArgs: args, SizeBytes: r.SizeBytes, IsEncrypted: r.IsEncrypted, EncryptAlgo: r.EncryptAlgo, EncryptHint: r.EncryptHint, CreatedAt: r.CreatedAt.Time}
}

func (s *VersionStore) MarkReady(ctx context.Context, tenant, version string) error {
	t, id, err := parseModelIDs(tenant, version)
	if err != nil {
		return err
	}
	n, err := New(s.pool).MarkModelVersionReady(ctx, MarkModelVersionReadyParams{TenantID: uuidType(t), ID: uuidType(id)})
	if err != nil {
		return err
	}
	if n != 1 {
		// A replay may reach finalization after a previous worker already
		// marked this version ready. Treat that state as idempotent success;
		// all other zero-row outcomes remain not-found/conflict to the caller.
		if _, getErr := s.GetVersion(ctx, tenant, version); getErr == nil {
			return nil
		}
		return pgx.ErrNoRows
	}
	return nil
}

func (s *VersionStore) MarkError(ctx context.Context, tenant, version, message string) error {
	t, id, err := parseModelIDs(tenant, version)
	if err != nil {
		return err
	}
	n, err := New(s.pool).MarkModelVersionError(ctx, MarkModelVersionErrorParams{TenantID: uuidType(t), ID: uuidType(id), ErrorMessage: message})
	if err != nil {
		return err
	}
	if n != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// MarkErrorAndMaybeModel records a terminal import failure on the bound
// version and, when no other ready version remains for the model, marks the
// model itself as errored. The aggregate SQL keeps tenant and version scope
// together so a stale or cross-tenant worker cannot update another record.
func (s *VersionStore) MarkErrorAndMaybeModel(ctx context.Context, tenant, taskID, version, owner string, epoch int64, message string) error {
	t, id, err := parseModelIDs(tenant, version)
	if err != nil {
		return err
	}
	task, err := uuid.Parse(taskID)
	if err != nil {
		return fmt.Errorf("task_id: %w", err)
	}
	params := MarkModelVersionErrorForTaskParams{TenantID: uuidType(t), ID: uuidType(task), ModelVersionID: uuidType(id), LeaseOwner: owner, LeaseEpoch: epoch, ErrorMessage: message}
	apply := func(q *Queries) error {
		r, queryErr := q.MarkModelVersionErrorForTask(ctx, params)
		if queryErr != nil {
			return queryErr
		}
		// Locking the model is a separate statement so the ready check gets a
		// fresh READ COMMITTED snapshot after any concurrent MarkReady commits.
		if _, queryErr = q.LockModelForVersionError(ctx, LockModelForVersionErrorParams{TenantID: uuidType(t), ID: r.ModelID}); queryErr != nil {
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return nil // the version update is still valid if its parent was removed
			}
			return queryErr
		}
		_, queryErr = q.MarkModelErrorIfNoReady(ctx, MarkModelErrorIfNoReadyParams{TenantID: uuidType(t), ID: r.ModelID, ErrorMessage: message})
		return queryErr
	}

	if db, ok := s.pool.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	}); ok {
		tx, beginErr := db.BeginTx(ctx, pgx.TxOptions{})
		if beginErr != nil {
			return beginErr
		}
		if queryErr := apply(New(tx)); queryErr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return pgx.ErrNoRows
			}
			return queryErr
		}
		return tx.Commit(ctx)
	}
	// DBTX implementations without transactions are test-only adapters; keep
	// the lease-checked path usable for them.
	return apply(New(s.pool))
}

func (s *VersionStore) SetChecksum(ctx context.Context, tenant, version, checksum string, size int64) error {
	t, id, err := parseModelIDs(tenant, version)
	if err != nil {
		return err
	}
	n, err := New(s.pool).SetModelVersionChecksum(ctx, SetModelVersionChecksumParams{TenantID: uuidType(t), ID: uuidType(id), ChecksumSha256: checksum, Column4: size})
	if err != nil {
		return err
	}
	if n != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
func versionFromRow(r ModelVersion, tenant string) modelbiz.Version {
	var args []string
	if len(r.StartupArgs) > 0 {
		// Log but don't fail for invalid startup args in version creation response
		_ = json.Unmarshal(r.StartupArgs, &args)
	}
	return modelbiz.Version{TenantID: tenant, ID: uuid.UUID(r.ID.Bytes).String(), ModelID: uuid.UUID(r.ModelID.Bytes).String(), Version: r.Version, Format: r.Format, Status: r.Status, ArtifactSHA256: r.ChecksumSha256, EngineType: r.EngineType, EngineImage: r.EngineImage, StartupCommand: r.StartupCommand, StartupArgs: args, SizeBytes: r.SizeBytes, IsEncrypted: r.IsEncrypted, EncryptAlgo: r.EncryptAlgo, EncryptHint: r.EncryptHint, CreatedAt: r.CreatedAt.Time}
}
