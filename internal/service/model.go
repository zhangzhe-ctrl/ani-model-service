package service

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	errors2 "github.com/go-kratos/kratos/v3/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	auditbiz "github.com/zhangzhe-ctrl/ani-model-service/internal/biz/audit"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/biz/model"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/biz/storage"
	workbiz "github.com/zhangzhe-ctrl/ani-model-service/internal/biz/work"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/identity"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

type ModelService struct {
	modelv1.UnimplementedModelServiceServer
	reader     model.VersionReader
	versions   model.VersionCatalog
	catalog    model.Catalog
	storage    storage.Port
	artifacts  model.ArtifactStore
	work       workbiz.Creator
	audit      auditbiz.Store
	references InferenceReferenceChecker
	notifier   ImportNotifier
}

// ImportNotifier wakes the durable import worker after a task is committed.
// PostgreSQL remains the source of truth when a notification is lost.
type ImportNotifier interface {
	Notify()
}

// InferenceReferenceChecker is a versioned external contract. Model never
// reads or writes Inference tables directly.
type InferenceReferenceChecker interface {
	HasActiveReferences(context.Context, string, string) (bool, error)
}

type externalModelCreator interface {
	CreateModelWithExternalID(context.Context, string, string, string, string, string, string, string, []byte, string) (model.Record, error)
}

func (s *ModelService) SetAuditStore(store auditbiz.Store)                       { s.audit = store }
func (s *ModelService) SetInferenceReferenceChecker(c InferenceReferenceChecker) { s.references = c }
func (s *ModelService) SetArtifactStore(store model.ArtifactStore)               { s.artifacts = store }
func (s *ModelService) SetImportNotifier(n ImportNotifier)                       { s.notifier = n }

func NewModelService(r model.VersionReader) *ModelService { return &ModelService{reader: r} }
func NewModelServiceWithDependencies(r model.VersionReader, v model.VersionCatalog, c model.Catalog, st storage.Port, w workbiz.Creator) *ModelService {
	return &ModelService{reader: r, versions: v, catalog: c, storage: st, work: w}
}
func NewModelCatalogService(r model.VersionReader, c model.Catalog) *ModelService {
	return &ModelService{reader: r, catalog: c}
}
func NewModelVersionService(v model.VersionCatalog) *ModelService {
	return &ModelService{reader: v, versions: v}
}
func NewModelDownloadService(v model.VersionReader, st storage.Port) *ModelService {
	return &ModelService{reader: v, storage: st}
}
func NewModelImportService(w workbiz.Creator) *ModelService { return &ModelService{work: w} }

func (s *ModelService) GetModelVersion(ctx context.Context, in *modelv1.GetModelVersionRequest) (*modelv1.GetModelVersionResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.reader == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	var v model.Version
	switch {
	case in.GetModelVersionId() != "" && (in.GetModelId() != "" || in.GetVersion() != ""):
		return nil, errors2.New(400, "INVALID_ARGUMENT", "model_version_id cannot be combined with model_id/version")
	case in.GetModelVersionId() != "":
		v, err = s.reader.GetVersion(ctx, p.TenantID, in.GetModelVersionId())
	case in.GetModelId() != "" || in.GetVersion() != "":
		if in.GetModelId() == "" || in.GetVersion() == "" {
			return nil, errors2.New(400, "INVALID_ARGUMENT", "model_id and version are required together")
		}
		r, ok := s.reader.(model.VersionReferenceReader)
		if !ok {
			return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "external model reference lookup is not configured")
		}
		v, err = r.GetVersionByExternalRef(ctx, p.TenantID, in.GetModelId(), in.GetVersion())
	default:
		return nil, errors2.New(400, "INVALID_ARGUMENT", "a model version selector is required")
	}
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) || stderrors.Is(err, model.ErrModelNotReady) {
			return nil, mapDomain(err)
		}
		slog.ErrorContext(ctx, "model version query failed", "error", err, "request_id", p.RequestID)
		return nil, errors2.New(503, "MODEL_STORE_ERROR", "model version store unavailable")
	}
	if err := v.DeploymentInput(); err != nil {
		return nil, mapDomain(err)
	}
	version := toProtoVersion(v)
	modelID := v.ExternalModelID
	if modelID == "" {
		modelID = v.ModelID
	}
	return &modelv1.GetModelVersionResponse{Model: &modelv1.Model{TenantId: p.TenantID, Id: v.ModelID, ModelId: modelID}, Version: version}, nil
}
func (s *ModelService) CreateModel(ctx context.Context, in *modelv1.CreateModelRequest) (*modelv1.Model, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if in.GetName() == "" || model.ValidateModelName(in.GetName()) != nil {
		return nil, errors2.New(400, "INVALID_ARGUMENT", "invalid model name")
	}
	if s.catalog == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	id := uuid.NewString()
	caps, _ := json.Marshal(in.GetCapabilities())
	var r model.Record
	externalID := in.GetModelId()
	if externalID == "" {
		externalID = in.GetName()
	}
	if len(externalID) > 255 || strings.TrimSpace(externalID) != externalID || strings.ContainsAny(externalID, "\r\n") {
		return nil, errors2.New(400, "INVALID_ARGUMENT", "invalid model_id")
	}
	if creator, ok := s.catalog.(externalModelCreator); ok {
		r, err = creator.CreateModelWithExternalID(ctx, p.TenantID, id, externalID, in.GetName(), in.GetDisplayName(), in.GetDescription(), "upload", caps, in.GetIdempotencyKey())
	} else {
		r, err = s.catalog.CreateModel(ctx, p.TenantID, id, in.GetName(), in.GetDisplayName(), in.GetDescription(), "upload", caps, in.GetIdempotencyKey())
	}
	if err != nil {
		return nil, mapStore(err)
	}
	if err := s.recordAudit(ctx, p, "model.create", "", r.Status, ""); err != nil {
		return nil, err
	}
	return toProtoModel(r), nil
}
func (s *ModelService) GetModel(ctx context.Context, in *modelv1.GetModelRequest) (*modelv1.Model, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.catalog == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	r, err := s.resolveModel(ctx, p.TenantID, in.GetModelId())
	if err != nil {
		return nil, err
	}
	return toProtoModel(r), nil
}
func (s *ModelService) ListModels(ctx context.Context, in *modelv1.ListModelsRequest) (*modelv1.ListModelsResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if in == nil || in.GetKeyword() != "" || in.GetSource() != "" || in.GetCapability() != "" || in.GetPage().GetCursor() != "" {
		return nil, errors2.BadRequest("INVALID_LIST_PARAMETERS", "cursor and extended filters are not supported")
	}
	switch in.GetStatus() {
	case "", "pending", "downloading", "ready", "error":
	default:
		return nil, errors2.BadRequest("INVALID_STATUS", "unsupported model status")
	}
	if in.GetPage() != nil && (in.GetPage().GetLimit() < 1 || in.GetPage().GetLimit() > 1000) {
		return nil, errors2.BadRequest("INVALID_LIMIT", "limit must be between 1 and 1000")
	}
	if s.catalog == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	limit := int32(100)
	if in.GetPage() != nil && in.GetPage().GetLimit() > 0 {
		limit = in.GetPage().GetLimit()
	}
	rs, err := s.catalog.ListModels(ctx, p.TenantID, in.GetStatus(), limit)
	if err != nil {
		slog.ErrorContext(ctx, "model catalog query failed", "error", err, "request_id", p.RequestID)
		return nil, errors2.New(503, "MODEL_STORE_ERROR", "model catalog unavailable")
	}
	out := &modelv1.ListModelsResponse{Models: make([]*modelv1.Model, len(rs))}
	for i, r := range rs {
		out.Models[i] = toProtoModel(r)
	}
	return out, nil
}
func (s *ModelService) DeleteModel(ctx context.Context, in *modelv1.DeleteModelRequest) (*emptypb.Empty, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.catalog == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	if s.references == nil {
		return nil, errors2.New(503, "INFERENCE_REFERENCE_CHECK_UNAVAILABLE", "inference reference checker is not configured")
	}
	m, err := s.resolveModel(ctx, p.TenantID, in.GetModelId())
	if err != nil {
		return nil, err
	}
	// Inference snapshots can contain either the legacy UUID or the external ID.
	ids := []string{m.ID}
	if m.ExternalModelID != "" && m.ExternalModelID != m.ID {
		ids = append(ids, m.ExternalModelID)
	}
	for _, id := range ids {
		referenced, err := s.references.HasActiveReferences(ctx, p.TenantID, id)
		if err != nil {
			return nil, errors2.New(503, "INFERENCE_REFERENCE_CHECK_UNAVAILABLE", "inference reference check failed")
		}
		if referenced {
			return nil, errors2.New(409, "MODEL_IN_USE", "model has active inference references")
		}
	}
	if err := s.catalog.SoftDeleteModel(ctx, p.TenantID, m.ID); err != nil {
		return nil, mapStore(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *ModelService) CreateModelVersion(ctx context.Context, in *modelv1.CreateModelVersionRequest) (*modelv1.ModelVersion, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.versions == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	m, err := s.resolveModel(ctx, p.TenantID, in.GetModelId())
	if err != nil {
		return nil, err
	}
	v := model.Version{TenantID: p.TenantID, ID: uuid.NewString(), ModelID: m.ID, ExternalModelID: m.ExternalModelID, Version: in.GetVersion(), Format: in.GetFormat(), Status: "pending", ArtifactSHA256: in.GetChecksumSha256(), EngineType: in.GetEngineType(), StartupCommand: in.GetStartupCommand(), StartupArgs: in.GetStartupArgs(), IdempotencyKey: in.GetIdempotencyKey()}
	if err := v.Validate(); err != nil || model.ValidateEngine(v.EngineType, v.StartupCommand, v.StartupArgs) != nil {
		return nil, errors2.New(400, "INVALID_ARGUMENT", "invalid model version")
	}
	r, err := s.versions.CreateVersion(ctx, v)
	if err != nil {
		return nil, mapStore(err)
	}
	r.ExternalModelID = m.ExternalModelID
	if in.GetStoragePath() != "" {
		// An idempotent replay of an already finalized upload must return the
		// same ready version without attempting to insert the artifact again.
		if r.Status == "ready" {
			r.ArtifactProvider = "upload"
			r.ArtifactReference = in.GetStoragePath()
			return toProtoVersion(r), nil
		}
		if s.storage == nil || s.artifacts == nil {
			return nil, errors2.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "storage and artifact stores are required to finalize an upload")
		}
		exists, err := s.storage.ObjectExists(ctx, p.TenantID, in.GetStoragePath())
		if err != nil {
			return nil, errors2.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
		}
		if !exists {
			return nil, errors2.NotFound("STORAGE_OBJECT_NOT_FOUND", "uploaded object was not found")
		}
		if err := s.storage.VerifyChecksum(ctx, p.TenantID, in.GetStoragePath(), in.GetChecksumSha256()); err != nil {
			if stderrors.Is(err, storage.ErrChecksumMismatch) {
				return nil, errors2.New(400, "CHECKSUM_MISMATCH", "uploaded object checksum does not match")
			}
			return nil, errors2.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
		}
		if _, err := s.artifacts.CreateArtifact(ctx, model.Artifact{
			TenantID: p.TenantID, ID: uuid.NewString(), ModelVersionID: r.ID,
			Provider: "upload", Reference: in.GetStoragePath(), Format: in.GetFormat(),
			SizeBytes: in.GetSizeBytes(), SHA256: in.GetChecksumSha256(),
			IsEncrypted: in.GetIsEncrypted(), EncryptAlgo: in.GetEncryptAlgo(),
		}); err != nil {
			return nil, mapStore(err)
		}
		stateStore, ok := s.versions.(model.VersionStateStore)
		if !ok {
			return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "version state store is not configured")
		}
		if err := stateStore.MarkReady(ctx, p.TenantID, r.ID); err != nil {
			return nil, mapStore(err)
		}
		r.Status = "ready"
		r.ArtifactProvider = "upload"
		r.ArtifactReference = in.GetStoragePath()
	}
	if err := s.recordAudit(ctx, p, "model_version.create", "", r.Status, ""); err != nil {
		return nil, err
	}
	return toProtoVersion(r), nil
}
func (s *ModelService) ListModelVersions(ctx context.Context, in *modelv1.ListModelVersionsRequest) (*modelv1.ListModelVersionsResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.versions == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	limit := int32(100)
	if in.GetPage() != nil && in.GetPage().GetLimit() > 0 {
		limit = in.GetPage().GetLimit()
	}
	m, err := s.resolveModel(ctx, p.TenantID, in.GetModelId())
	if err != nil {
		return nil, err
	}
	rs, err := s.versions.ListVersions(ctx, p.TenantID, m.ID, limit)
	if err != nil {
		return nil, mapStore(err)
	}
	out := &modelv1.ListModelVersionsResponse{Versions: make([]*modelv1.ModelVersion, len(rs))}
	for i, v := range rs {
		v.ExternalModelID = m.ExternalModelID
		out.Versions[i] = toProtoVersion(v)
	}
	return out, nil
}

func (s *ModelService) resolveModel(ctx context.Context, tenant, selector string) (model.Record, error) {
	if selector == "" || len(selector) > 255 || strings.TrimSpace(selector) != selector || strings.ContainsAny(selector, "\r\n") {
		return model.Record{}, errors2.New(400, "INVALID_ARGUMENT", "invalid model_id")
	}
	if s.catalog == nil {
		return model.Record{}, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model catalog is not configured")
	}
	var r model.Record
	var err error
	if id, parseErr := uuid.Parse(selector); parseErr == nil {
		r, err = s.catalog.GetModel(ctx, tenant, id.String())
	} else if catalog, ok := s.catalog.(model.ExternalModelCatalog); ok {
		r, err = catalog.GetModelByExternalID(ctx, tenant, selector)
	} else {
		return model.Record{}, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "external model lookup is not configured")
	}
	if err != nil {
		return model.Record{}, mapStore(err)
	}
	return r, nil
}
func toProtoVersion(v model.Version) *modelv1.ModelVersion {
	modelID := v.ExternalModelID
	if modelID == "" {
		modelID = v.ModelID
	}
	return &modelv1.ModelVersion{Id: v.ID, ModelId: modelID, Version: v.Version, Format: v.Format, Status: v.Status, ChecksumSha256: v.ArtifactSHA256, StoragePath: v.ArtifactReference, EngineType: v.EngineType, StartupCommand: v.StartupCommand, StartupArgs: v.StartupArgs}
}

func (s *ModelService) GetModelDownloadURL(ctx context.Context, in *modelv1.GetModelDownloadURLRequest) (*modelv1.GetModelDownloadURLResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.reader == nil || s.storage == nil {
		return nil, errors2.New(503, "MODEL_DEPENDENCY_UNAVAILABLE", "model or storage provider is not configured")
	}
	v, err := s.reader.GetVersion(ctx, p.TenantID, in.GetModelVersionId())
	if err != nil {
		return nil, mapDomain(err)
	}
	if err := v.DeploymentInput(); err != nil {
		return nil, mapDomain(err)
	}
	requester := in.GetRequester()
	if requester == "" {
		requester = p.Actor
	}
	u, err := s.storage.CreateDownloadURL(ctx, storage.DownloadRequest{TenantID: p.TenantID, ObjectRef: v.ArtifactReference, Requester: requester, TTL: 5 * time.Minute})
	if err != nil {
		return nil, errors2.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
	}
	if err := storage.ValidateSignedURL(u, time.Now()); err != nil {
		return nil, errors2.New(502, "STORAGE_INVALID_URL", err.Error())
	}
	return &modelv1.GetModelDownloadURLResponse{DownloadUrl: u.URL, StoragePath: v.ArtifactReference, ExpiresAt: timestamppb.New(u.ExpiresAt)}, nil
}

func (s *ModelService) GetUploadURL(ctx context.Context, in *modelv1.GetUploadURLRequest) (*modelv1.GetUploadURLResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.storage == nil {
		return nil, errors2.New(503, "STORAGE_UNAVAILABLE", "storage provider is not configured")
	}
	if in.GetModelId() == "" || in.GetVersion() == "" || in.GetFileName() == "" || strings.ContainsAny(in.GetFileName(), "/\\\r\n") || in.GetSizeBytes() <= 0 || !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(in.GetChecksumSha256()) {
		return nil, errors2.New(400, "INVALID_ARGUMENT", "invalid upload request")
	}
	ref := fmt.Sprintf("%s/%s/%s/%s", p.TenantID, in.GetModelId(), in.GetVersion(), in.GetFileName())
	u, err := s.storage.CreateUploadURL(ctx, storage.UploadRequest{TenantID: p.TenantID, ObjectRef: ref, SizeBytes: in.GetSizeBytes(), ChecksumSHA256: in.GetChecksumSha256()})
	if err != nil {
		return nil, errors2.New(502, "STORAGE_PROVIDER_ERROR", err.Error())
	}
	if err := storage.ValidateSignedURL(u, time.Now()); err != nil {
		return nil, errors2.New(502, "STORAGE_INVALID_URL", err.Error())
	}
	return &modelv1.GetUploadURLResponse{UploadUrl: u.URL, StoragePath: ref, DocId: uuid.NewString(), ExpiresAt: timestamppb.New(u.ExpiresAt)}, nil
}

func (s *ModelService) ImportModel(ctx context.Context, in *modelv1.ImportModelRequest) (*modelv1.ImportTask, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.work == nil {
		return nil, errors2.New(503, "WORKER_UNAVAILABLE", "import worker is not configured")
	}
	if in.GetSource() != "huggingface" && in.GetSource() != "modelscope" {
		return nil, errors2.New(400, "INVALID_ARGUMENT", "unsupported import source")
	}
	if in.GetRepoId() == "" || in.GetIdempotencyKey() == "" {
		return nil, errors2.New(400, "INVALID_ARGUMENT", "repo_id and idempotency_key are required")
	}
	modelID := in.GetModelId()
	if modelID != "" {
		if parsed, parseErr := uuid.Parse(modelID); parseErr == nil {
			modelID = parsed.String()
		} else {
			m, resolveErr := s.resolveModel(ctx, p.TenantID, modelID)
			if resolveErr != nil {
				return nil, resolveErr
			}
			modelID = m.ID
		}
	}
	if in.GetModelVersionId() != "" {
		if _, err := uuid.Parse(in.GetModelVersionId()); err != nil {
			return nil, errors2.New(400, "INVALID_ARGUMENT", "invalid model_version_id")
		}
	}
	t := workbiz.Task{
		TenantID:       p.TenantID,
		ID:             uuid.NewString(),
		ModelID:        modelID,
		VersionID:      in.GetModelVersionId(),
		TaskType:       in.GetSource(),
		Source:         in.GetSource(),
		RepoID:         in.GetRepoId(),
		Revision:       in.GetRevision(),
		IdempotencyKey: in.GetIdempotencyKey(),
		Status:         workbiz.Pending,
	}
	created, err := s.work.Create(ctx, t)
	if err != nil {
		return nil, errors2.New(409, "IMPORT_CONFLICT", err.Error())
	}
	if err := s.recordAudit(ctx, p, "model_import.create", "", created.Status, created.ID); err != nil {
		return nil, err
	}
	if s.notifier != nil {
		s.notifier.Notify()
	}
	return &modelv1.ImportTask{TaskId: created.ID, TaskType: in.GetSource(), Status: created.Status, ModelId: created.ModelID, ModelVersionId: created.VersionID, AttemptCount: int32(created.AttemptCount)}, nil
}

func (s *ModelService) recordAudit(ctx context.Context, p identity.Principal, operation, before, after, taskID string) error {
	if s.audit == nil {
		return nil
	}
	if err := s.audit.Insert(ctx, auditbiz.Event{TenantID: p.TenantID, Actor: p.Actor, Workload: p.Workload, RequestID: p.RequestID, TaskID: taskID, Action: operation, BeforeState: before, AfterState: after}); err != nil {
		return errors2.New(500, "AUDIT_UNAVAILABLE", "audit event could not be persisted")
	}
	return nil
}
func toProtoModel(r model.Record) *modelv1.Model {
	var caps []string
	_ = json.Unmarshal(r.Capabilities, &caps)
	modelID := r.ExternalModelID
	if modelID == "" {
		modelID = r.Name
	}
	return &modelv1.Model{TenantId: r.TenantID, Id: r.ID, ModelId: modelID, Name: r.Name, DisplayName: r.DisplayName, Description: r.Description, Source: r.Source, Capabilities: caps, Status: r.Status, TotalSizeBytes: r.TotalSizeBytes}
}
func mapIdentity(err error) error {
	if stderrors.Is(err, identity.ErrMissingPrincipal) {
		return errors2.New(401, "UNAUTHENTICATED", err.Error())
	}
	return errors2.New(403, "TENANT_MISMATCH", err.Error())
}
func mapDomain(err error) error {
	if stderrors.Is(err, model.ErrModelNotReady) {
		return errors2.New(412, "MODEL_NOT_READY", err.Error())
	}
	return errors2.NotFound("MODEL_NOT_FOUND", err.Error())
}
func mapStore(err error) error {
	if stderrors.Is(err, pgx.ErrNoRows) {
		return errors2.NotFound("MODEL_NOT_FOUND", "resource not found")
	}
	var pe *pgconn.PgError
	if stderrors.As(err, &pe) && pe.Code == "23505" {
		return errors2.New(409, "CONFLICT", "resource already exists")
	}
	return errors2.New(500, "MODEL_STORE_ERROR", err.Error())
}
