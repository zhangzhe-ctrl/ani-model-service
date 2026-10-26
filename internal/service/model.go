package service

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"regexp"
	"strings"

	errors2 "github.com/go-kratos/kratos/v3/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	auditbiz "github.com/liangzai006/ani-model-service/internal/biz/audit"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/biz/storage"
	workbiz "github.com/liangzai006/ani-model-service/internal/biz/work"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

type ModelService struct {
	modelv1.UnimplementedModelServiceServer
	reader         model.VersionReader
	versions       model.VersionCatalog
	catalog        model.Catalog
	storage        storage.Port
	artifacts      model.ArtifactStore
	work           workbiz.Creator
	taskReader     workbiz.Reader
	taskLogs       ImportTaskLogReader
	logNamespace   string
	taskRetry      workbiz.Retrier
	audit          auditbiz.Store
	references     InferenceReferenceChecker
	notifier       ImportNotifier
	searcher       model.Searcher
	bundleImporter model.BundleImporter
	trained        model.TrainedArtifactRegistrar
}

// ImportNotifier wakes the durable import worker after a task is committed.
// PostgreSQL remains the source of truth when a notification is lost.
type ImportNotifier interface {
	Notify()
}

type ImportTaskLogReader interface {
	Logs(context.Context, string, string) (string, error)
}

// InferenceReferenceChecker is a versioned external contract. Model never
// reads or writes Inference tables directly.
type InferenceReferenceChecker = model.ReferenceChecker

type externalModelCreator interface {
	CreateModelWithExternalID(context.Context, string, string, string, string, string, string, string, []byte, string) (model.Record, error)
}

func (s *ModelService) SetAuditStore(store auditbiz.Store)                       { s.audit = store }
func (s *ModelService) SetInferenceReferenceChecker(c InferenceReferenceChecker) { s.references = c }
func (s *ModelService) SetArtifactStore(store model.ArtifactStore)               { s.artifacts = store }
func (s *ModelService) SetImportNotifier(n ImportNotifier)                       { s.notifier = n }
func (s *ModelService) SetImportTaskReader(r workbiz.Reader)                     { s.taskReader = r }
func (s *ModelService) SetImportTaskLogReader(r ImportTaskLogReader, namespace string) {
	s.taskLogs, s.logNamespace = r, namespace
}
func (s *ModelService) SetImportTaskRetrier(r workbiz.Retrier)           { s.taskRetry = r }
func (s *ModelService) SetExternalModelSearcher(searcher model.Searcher) { s.searcher = searcher }
func (s *ModelService) SetBundleImporter(importer model.BundleImporter)  { s.bundleImporter = importer }
func (s *ModelService) SetTrainedArtifactRegistrar(registrar model.TrainedArtifactRegistrar) {
	s.trained = registrar
}

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
func NewModelBundleImportService(i model.BundleImporter) *ModelService {
	return &ModelService{bundleImporter: i}
}

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
		return nil, mapDomain(err)
	}
	if err := v.DeploymentInput(); err != nil {
		return nil, mapDomain(err)
	}
	version := toProtoVersion(v)
	modelID := v.ExternalModelID
	if modelID == "" {
		modelID = v.ModelID
	}
	owner := &modelv1.Model{TenantId: p.TenantID, Id: v.ModelID, ModelId: modelID}
	if s.catalog != nil {
		r, err := s.catalog.GetModel(ctx, p.TenantID, v.ModelID)
		if err != nil {
			return nil, mapStore(err)
		}
		owner = toProtoModel(r)
	}
	return &modelv1.GetModelVersionResponse{Model: owner, Version: version}, nil
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
	if err := s.recordAudit(ctx, p, "model.create", "", r.Status, "", r.ID, ""); err != nil {
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
	if s.catalog == nil {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is not configured")
	}
	options, err := resolvePage(in.Page, in.PageSize)
	if err != nil {
		return nil, err
	}
	options.Status, options.Keyword, options.Source, options.Capability = in.GetStatus(), in.GetKeyword(), in.GetSource(), in.GetCapability()
	var rs []model.Record
	var total int64
	if paged, ok := s.catalog.(model.PagedCatalog); ok {
		rs, total, err = paged.ListModelsPage(ctx, p.TenantID, options)
	} else {
		rs, err = s.catalog.ListModels(ctx, p.TenantID, options)
		if err == nil {
			total, err = s.catalog.CountModels(ctx, p.TenantID, options)
		}
	}
	if err != nil {
		return nil, errors2.New(500, "MODEL_STORE_ERROR", err.Error())
	}
	if rs == nil {
		rs = []model.Record{}
	}
	out := &modelv1.ListModelsResponse{Items: make([]*modelv1.Model, len(rs)), Total: total}
	for i, r := range rs {
		out.Items[i] = toProtoModel(r)
	}
	return out, nil
}

func (s *ModelService) SearchExternalModels(ctx context.Context, in *modelv1.SearchExternalModelsRequest) (*modelv1.SearchExternalModelsResponse, error) {
	_, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.searcher == nil {
		return nil, errors2.New(503, "MODEL_PROVIDER_UNAVAILABLE", "external model search is not configured")
	}
	options, err := resolvePage(in.Page, in.PageSize)
	if err != nil {
		return nil, err
	}
	page := int(options.Offset/int64(options.Limit)) + 1
	pageSize := int(options.Limit)
	request := model.SearchRequest{Source: in.GetSource(), Keyword: in.GetKeyword(), Owner: in.GetOwner(), Task: in.GetTask(), Library: in.GetLibrary(), Page: page, PageSize: pageSize, Limit: pageSize}
	if err := request.Validate(); err != nil {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid external model search request")
	}
	result, err := s.searcher.Search(ctx, request)
	if err != nil {
		if stderrors.Is(err, model.ErrInvalidSearchRequest) {
			return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid external model search request")
		}
		return nil, errors2.New(502, "MODEL_PROVIDER_UNAVAILABLE", "external model provider is unavailable")
	}
	items := make([]*modelv1.ExternalModelCandidate, len(result.Items))
	for i, item := range result.Items {
		if item.Source != request.Source || strings.TrimSpace(item.RepoID) == "" || strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.Revision) == "" {
			return nil, errors2.New(502, "MODEL_PROVIDER_INVALID_RESPONSE", "external model provider returned an invalid candidate")
		}
		items[i] = &modelv1.ExternalModelCandidate{Source: item.Source, RepoId: item.RepoID, Name: item.Name, DisplayName: item.DisplayName, Description: item.Description, Revision: item.Revision, SizeBytes: item.SizeBytes, Tags: append([]string(nil), item.Tags...), UpdatedAt: protoTime(item.UpdatedAt)}
	}
	return &modelv1.SearchExternalModelsResponse{Items: items, HasMore: result.HasMore}, nil
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
	selector := in.GetModelId()
	if selector == "" || len(selector) > 255 || strings.TrimSpace(selector) != selector || strings.ContainsAny(selector, "\r\n") {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid model_id")
	}
	store, ok := s.catalog.(model.DeletionCatalog)
	if !ok {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "protected deletion is not configured")
	}
	if err := store.DeleteModel(ctx, p.TenantID, selector, s.references); err != nil {
		return nil, mapDeletion(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *ModelService) DeleteModelVersion(ctx context.Context, in *modelv1.DeleteModelVersionRequest) (*modelv1.DeleteModelVersionResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	id, err := uuid.Parse(in.GetModelVersionId())
	if err != nil || id == uuid.Nil {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid model_version_id")
	}
	if s.references == nil {
		return nil, mapDeletion(model.ErrReferenceCheckUnavailable)
	}
	store, ok := s.catalog.(model.DeletionCatalog)
	if !ok {
		return nil, errors2.New(503, "MODEL_STORE_UNAVAILABLE", "protected deletion is not configured")
	}
	if err = store.DeleteVersion(ctx, p.TenantID, id.String(), s.references); err != nil {
		return nil, mapDeletion(err)
	}
	return &modelv1.DeleteModelVersionResponse{}, nil
}

func mapDeletion(err error) error {
	if stderrors.Is(err, model.ErrModelInUse) {
		return errors2.New(409, "MODEL_IN_USE", model.ErrModelInUse.Error())
	}
	if stderrors.Is(err, model.ErrReferenceCheckUnavailable) {
		return errors2.New(503, "INFERENCE_REFERENCE_CHECK_UNAVAILABLE", model.ErrReferenceCheckUnavailable.Error())
	}
	return mapStore(err)
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
	v := model.Version{TenantID: p.TenantID, ID: uuid.NewString(), ModelID: m.ID, ExternalModelID: m.ExternalModelID, Version: in.GetVersion(), Format: in.GetFormat(), Status: "pending", SizeBytes: in.GetSizeBytes(), ArtifactSHA256: in.GetChecksumSha256(), EngineType: in.GetEngineType(), EngineImage: in.GetEngineImage(), StartupCommand: in.GetStartupCommand(), IdempotencyKey: in.GetIdempotencyKey()}
	if err := v.Validate(); err != nil || model.ValidateEngineDefaults(v.EngineType, v.EngineImage, v.StartupCommand) != nil {
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
	if err := s.recordAudit(ctx, p, "model_version.create", "", r.Status, "", r.ModelID, r.ID); err != nil {
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
	m, err := s.resolveModel(ctx, p.TenantID, in.GetModelId())
	if err != nil {
		return nil, err
	}
	options, err := resolvePage(in.Page, in.PageSize)
	if err != nil {
		return nil, err
	}
	options.Status, err = model.NormalizeVersionStatus(in.GetStatus())
	if err != nil {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", err.Error())
	}
	var rs []model.Version
	var total int64
	if paged, ok := s.versions.(model.PagedVersionCatalog); ok {
		rs, total, err = paged.ListVersionsPage(ctx, p.TenantID, m.ID, options)
	} else {
		rs, err = s.versions.ListVersions(ctx, p.TenantID, m.ID, options)
		if err == nil {
			total, err = s.versions.CountModelVersions(ctx, p.TenantID, m.ID, options)
		}
	}
	if err != nil {
		return nil, mapStore(err)
	}
	if rs == nil {
		rs = []model.Version{}
	}
	out := &modelv1.ListModelVersionsResponse{Items: make([]*modelv1.ModelVersion, len(rs)), Total: total}
	for i, v := range rs {
		v.ExternalModelID = m.ExternalModelID
		out.Items[i] = toProtoVersion(v)
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
	return &modelv1.ModelVersion{Id: v.ID, ModelId: modelID, Version: v.Version, Format: v.Format, Status: v.Status, ChecksumSha256: v.ArtifactSHA256, StoragePath: v.ArtifactReference, EngineType: v.EngineType, EngineImage: v.EngineImage, StartupCommand: v.StartupCommand, SizeBytes: v.SizeBytes, IsEncrypted: v.IsEncrypted, EncryptAlgo: v.EncryptAlgo, EncryptHint: v.EncryptHint, CreatedAt: protoTime(v.CreatedAt)}
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
	if err := s.recordAudit(ctx, p, "model_import.create", "", created.Status, created.ID, created.ModelID, created.VersionID); err != nil {
		return nil, err
	}
	if s.notifier != nil {
		s.notifier.Notify()
	}
	return &modelv1.ImportTask{TaskId: created.ID, TaskType: in.GetSource(), Status: created.Status, ModelId: created.ModelID, ModelVersionId: created.VersionID, AttemptCount: int32(created.AttemptCount)}, nil
}

func (s *ModelService) ImportModelBundle(ctx context.Context, in *modelv1.ImportModelBundleRequest) (*modelv1.ImportModelBundleResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if s.bundleImporter == nil {
		return nil, errors2.New(503, "MODEL_BUNDLE_IMPORT_UNAVAILABLE", "model bundle importer is not configured")
	}
	modelData, versionData, sourceData := in.GetModel(), in.GetVersion(), in.GetSource()
	bundle := model.BundleImport{TenantID: p.TenantID, IdempotencyKey: in.GetIdempotencyKey()}
	if modelData != nil {
		bundle.Name, bundle.DisplayName, bundle.Description, bundle.ExternalModelID = modelData.GetName(), modelData.GetDisplayName(), modelData.GetDescription(), modelData.GetExternalModelId()
		bundle.Capabilities = append([]string(nil), modelData.GetCapabilities()...)
	}
	if versionData != nil {
		bundle.Version, bundle.Format, bundle.EngineType, bundle.EngineImage, bundle.StartupCommand = versionData.GetVersion(), versionData.GetFormat(), versionData.GetEngineType(), versionData.GetEngineImage(), versionData.GetStartupCommand()
	}
	if sourceData != nil {
		bundle.Provider, bundle.RepoID, bundle.Revision = sourceData.GetProvider(), sourceData.GetRepoId(), sourceData.GetRevision()
	}
	if err := bundle.Validate(); err != nil {
		return nil, errors2.New(400, "INVALID_ARGUMENT", err.Error())
	}
	result, err := s.bundleImporter.ImportBundle(ctx, bundle)
	if err != nil {
		return nil, mapStore(err)
	}
	if err := s.recordAudit(ctx, p, "model_import_bundle.create", "", result.TaskStatus, result.TaskID, result.Model.ID, result.Version.ID); err != nil {
		return nil, err
	}
	if s.notifier != nil {
		s.notifier.Notify()
	}
	return &modelv1.ImportModelBundleResponse{
		Model:   toProtoModel(result.Model),
		Version: toProtoVersion(result.Version),
		Task:    &modelv1.ImportTask{TaskId: result.TaskID, TaskType: result.TaskType, Status: result.TaskStatus, ModelId: result.Model.ID, ModelVersionId: result.Version.ID},
	}, nil
}

func (s *ModelService) GetImportTask(ctx context.Context, in *modelv1.GetImportTaskRequest) (*modelv1.GetImportTaskResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if _, err := uuid.Parse(in.GetTaskId()); err != nil {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid task_id")
	}
	reader := s.taskReader
	if reader == nil {
		if candidate, ok := s.work.(workbiz.Reader); ok {
			reader = candidate
		}
	}
	if reader == nil {
		return nil, errors2.New(503, "WORKER_UNAVAILABLE", "import task reader is not configured")
	}
	task, err := reader.Get(ctx, p.TenantID, in.GetTaskId())
	if err != nil {
		return nil, mapStore(err)
	}
	if task.TenantID != "" && task.TenantID != p.TenantID {
		return nil, mapIdentity(identity.ErrTenantMismatch)
	}
	return &modelv1.GetImportTaskResponse{Task: toProtoImportTask(task)}, nil
}

func (s *ModelService) GetImportTaskLogs(ctx context.Context, in *modelv1.GetImportTaskLogsRequest) (*modelv1.GetImportTaskLogsResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if _, err := uuid.Parse(in.GetTaskId()); err != nil {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid task_id")
	}
	reader := s.taskReader
	if reader == nil {
		if candidate, ok := s.work.(workbiz.Reader); ok {
			reader = candidate
		}
	}
	if reader == nil {
		return nil, errors2.New(503, "WORKER_UNAVAILABLE", "import task reader is not configured")
	}
	if s.taskLogs == nil || strings.TrimSpace(s.logNamespace) == "" {
		return nil, errors2.New(503, "IMPORT_LOGS_UNAVAILABLE", "import task log reader is not configured")
	}
	task, err := reader.Get(ctx, p.TenantID, in.GetTaskId())
	if err != nil {
		return nil, mapStore(err)
	}
	if task.TenantID != "" && task.TenantID != p.TenantID {
		return nil, mapIdentity(identity.ErrTenantMismatch)
	}
	logs, err := s.taskLogs.Logs(ctx, s.logNamespace, workbiz.ImportJobName(task.ID, task.AttemptCount))
	if err != nil {
		return nil, errors2.New(409, "IMPORT_LOGS_UNAVAILABLE", "import task logs are no longer available")
	}
	return &modelv1.GetImportTaskLogsResponse{TaskId: task.ID, Status: task.Status, Logs: logs, Available: true}, nil
}

func (s *ModelService) RetryImportTask(ctx context.Context, in *modelv1.RetryImportTaskRequest) (*modelv1.RetryImportTaskResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	if _, err := uuid.Parse(in.GetTaskId()); err != nil {
		return nil, errors2.BadRequest("INVALID_ARGUMENT", "invalid task_id")
	}
	retrier := s.taskRetry
	if retrier == nil {
		if candidate, ok := s.work.(workbiz.Retrier); ok {
			retrier = candidate
		}
	}
	if retrier == nil {
		return nil, errors2.New(503, "WORKER_UNAVAILABLE", "import task retry is not configured")
	}
	task, err := retrier.RetryFailed(ctx, p.TenantID, in.GetTaskId())
	if err != nil {
		if stderrors.Is(err, workbiz.ErrInvalidTransition) {
			return nil, errors2.New(409, "IMPORT_NOT_RETRYABLE", err.Error())
		}
		return nil, mapStore(err)
	}
	if task.TenantID != "" && task.TenantID != p.TenantID {
		return nil, mapIdentity(identity.ErrTenantMismatch)
	}
	if err := s.recordAudit(ctx, p, "model_import.retry", "failed", task.Status, task.ID, task.ModelID, task.VersionID); err != nil {
		return nil, err
	}
	if s.notifier != nil {
		s.notifier.Notify()
	}
	return &modelv1.RetryImportTaskResponse{Task: toProtoImportTask(task)}, nil
}

func (s *ModelService) recordAudit(ctx context.Context, p identity.Principal, operation, before, after, taskID, modelID, versionID string) error {
	if s.audit == nil {
		return nil
	}
	if err := s.audit.Insert(ctx, auditbiz.Event{TenantID: p.TenantID, Actor: p.Actor, Workload: p.Workload, RequestID: p.RequestID, TaskID: taskID, ModelID: modelID, ModelVersionID: versionID, Action: operation, BeforeState: before, AfterState: after}); err != nil {
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
	out := &modelv1.Model{TenantId: r.TenantID, Id: r.ID, ModelId: modelID, Name: r.Name, DisplayName: r.DisplayName, Description: r.Description, Source: r.Source, Capabilities: caps, Status: r.Status, TotalSizeBytes: r.TotalSizeBytes, SourceRepoId: r.SourceRepoID, ErrorMessage: r.ErrorMessage, CreatedAt: protoTime(r.CreatedAt), UpdatedAt: protoTime(r.UpdatedAt)}
	if r.LatestVersion != nil {
		out.Versions = []*modelv1.ModelVersion{toProtoVersion(*r.LatestVersion)}
	}
	return out
}

func toProtoImportTask(t workbiz.Task) *modelv1.ImportTask {
	errorMessage := t.ErrorMessage
	if t.Status == workbiz.Completed {
		errorMessage = ""
	}
	return &modelv1.ImportTask{TaskId: t.ID, TaskType: t.TaskType, Status: t.Status, ModelId: t.ModelID, ModelVersionId: t.VersionID, AttemptCount: int32(t.AttemptCount), ProgressPct: int32(t.ProgressPct), ErrorMessage: errorMessage, CreatedAt: protoTime(t.CreatedAt), CompletedAt: protoTime(t.CompletedAt)}
}

func protoTime(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
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
	if storeUnavailable(err) {
		return errors2.New(503, "MODEL_STORE_UNAVAILABLE", "model store is unavailable")
	}
	var pe *pgconn.PgError
	if stderrors.As(err, &pe) && pe.Code == "23505" {
		return errors2.New(409, "CONFLICT", "resource already exists")
	}
	return errors2.New(500, "MODEL_STORE_ERROR", err.Error())
}

func storeUnavailable(err error) bool {
	if stderrors.Is(err, pgx.ErrTxClosed) {
		return true
	}
	var pe *pgconn.PgError
	if stderrors.As(err, &pe) {
		return strings.HasPrefix(pe.Code, "08") || pe.Code == "57P01" || pe.Code == "57P02" || pe.Code == "57P03" || pe.Code == "53300" || pe.Code == "55006"
	}
	var ce *pgconn.ConnectError
	return stderrors.As(err, &ce)
}
