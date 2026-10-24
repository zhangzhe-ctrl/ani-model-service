package service

import (
	"context"
	"errors"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	auditbiz "github.com/liangzai006/ani-model-service/internal/biz/audit"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/biz/storage"
	workbiz "github.com/liangzai006/ani-model-service/internal/biz/work"
	"github.com/liangzai006/ani-model-service/internal/data/importer"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

type catalogFake struct {
	created model.Record
	model   model.Record
}

type getModelCatalogFake struct {
	catalogFake
	err       error
	gotTenant string
	gotID     string
}

func (c *getModelCatalogFake) GetModel(ctx context.Context, tenant, id string) (model.Record, error) {
	c.gotTenant = tenant
	c.gotID = id
	if c.err != nil {
		return model.Record{}, c.err
	}
	return c.catalogFake.GetModel(ctx, tenant, id)
}

func (c *catalogFake) CreateModel(_ context.Context, tenant, id, name, display, desc, source string, caps []byte, idempotencyKey string) (model.Record, error) {
	c.created = model.Record{TenantID: tenant, ID: id, Name: name}
	return c.created, nil
}
func (c *catalogFake) GetModel(_ context.Context, tenant, id string) (model.Record, error) {
	if c.model.ID != "" {
		return c.model, nil
	}
	return model.Record{TenantID: tenant, ID: id}, nil
}
func (c *catalogFake) ListModels(context.Context, string, model.ListOptions) ([]model.Record, error) {
	return nil, nil
}
func (c *catalogFake) CountModels(context.Context, string, model.ListOptions) (int64, error) {
	return 0, nil
}
func (c *catalogFake) SoftDeleteModel(context.Context, string, string) error { return nil }

type referenceCheckerFake struct {
	referenced bool
	err        error
}

func (c referenceCheckerFake) HasActiveVersionReferences(context.Context, string, []string) (bool, error) {
	return c.referenced, c.err
}

func (c *catalogFake) DeleteModel(ctx context.Context, tenant, selector string, refs model.ReferenceChecker) error {
	active, err := refs.HasActiveVersionReferences(ctx, tenant, []string{"22222222-2222-4222-8222-222222222222"})
	if err != nil {
		return model.ErrReferenceCheckUnavailable
	}
	if active {
		return model.ErrModelInUse
	}
	return nil
}
func (c *catalogFake) DeleteVersion(ctx context.Context, tenant, version string, refs model.ReferenceChecker) error {
	active, err := refs.HasActiveVersionReferences(ctx, tenant, []string{version})
	if err != nil {
		return model.ErrReferenceCheckUnavailable
	}
	if active {
		return model.ErrModelInUse
	}
	return nil
}

type storageFake struct{}

func (storageFake) CreateUploadURL(context.Context, storage.UploadRequest) (storage.SignedURL, error) {
	return storage.SignedURL{URL: "https://upload", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (storageFake) ObjectExists(context.Context, string, string) (bool, error)   { return true, nil }
func (storageFake) VerifyChecksum(context.Context, string, string, string) error { return nil }
func (storageFake) CreateDownloadURL(context.Context, storage.DownloadRequest) (storage.SignedURL, error) {
	return storage.SignedURL{}, nil
}

type versionReaderFunc func(context.Context, string, string) (model.Version, error)

type externalVersionReaderFake struct{ version model.Version }

type importWorkFake struct{ got workbiz.Task }

func (f *importWorkFake) Create(_ context.Context, task workbiz.Task) (workbiz.Task, error) {
	f.got = task
	return task, nil
}

type importNotifierFake struct{ calls int }

func (f *importNotifierFake) Notify() { f.calls++ }

type bundleAuditFake struct {
	event auditbiz.Event
	err   error
}

func (f *bundleAuditFake) Insert(_ context.Context, event auditbiz.Event) error {
	f.event = event
	return f.err
}

type bundleImporterFake struct {
	input  model.BundleImport
	result model.BundleImportResult
	err    error
}

func (f *bundleImporterFake) ImportBundle(_ context.Context, input model.BundleImport) (model.BundleImportResult, error) {
	f.input = input
	return f.result, f.err
}

func bundleImportRequest(tenant string) *modelv1.ImportModelBundleRequest {
	return &modelv1.ImportModelBundleRequest{
		TenantId:       tenant,
		IdempotencyKey: "bundle-1",
		Model:          &modelv1.ImportModelBundleModelData{Name: "qwen3.5-0.8b", Capabilities: []string{"text-generation"}, ExternalModelId: "Qwen/Qwen3.5-0.8B"},
		Version:        &modelv1.ImportModelBundleVersionData{Version: "master", Format: "safetensors", EngineType: "vllm", EngineImage: "registry/vllm:tag", StartupCommand: "vllm serve /models"},
		Source:         &modelv1.ImportModelBundleSourceData{Provider: "modelscope", RepoId: "Qwen/Qwen3.5-0.8B", Revision: "master"},
	}
}

func TestImportModelBundleRequestCarriesCompleteRuntimeInput(t *testing.T) {
	in := &modelv1.ImportModelBundleRequest{
		TenantId:       "tenant-from-auth",
		IdempotencyKey: "bundle-1",
		Model: &modelv1.ImportModelBundleModelData{
			Name:            "qwen3.5-0.8b",
			Capabilities:    []string{"text-generation"},
			ExternalModelId: "Qwen/Qwen3.5-0.8B",
		},
		Version: &modelv1.ImportModelBundleVersionData{
			Version:        "master",
			Format:         "safetensors",
			EngineType:     "vllm",
			EngineImage:    "registry/vllm:tag",
			StartupCommand: "vllm serve /models --served-model-name qwen3.5-0.8b",
		},
		Source: &modelv1.ImportModelBundleSourceData{
			Provider: "modelscope",
			RepoId:   "Qwen/Qwen3.5-0.8B",
			Revision: "master",
		},
	}
	if in.GetVersion().GetStartupCommand() != "vllm serve /models --served-model-name qwen3.5-0.8b" {
		t.Fatal("full startup command was not retained")
	}
	if in.GetSource().GetRepoId() != "Qwen/Qwen3.5-0.8B" || in.GetSource().GetRevision() != "master" || in.GetVersion().GetEngineType() != "vllm" || in.GetVersion().GetEngineImage() != "registry/vllm:tag" {
		t.Fatalf("request runtime input was not retained: %+v", in)
	}
}

func TestImportModelBundleUsesAuthenticatedTenantAndReturnsAggregate(t *testing.T) {
	importer := &bundleImporterFake{result: model.BundleImportResult{
		Model:   model.Record{TenantID: "tenant-auth", ID: "model-1", Name: "qwen3.5-0.8b", ExternalModelID: "Qwen/Qwen3.5-0.8B", Capabilities: []byte(`["text-generation"]`)},
		Version: model.Version{TenantID: "tenant-auth", ID: "version-1", ModelID: "model-1", Version: "master", Format: "safetensors", EngineType: "vllm", EngineImage: "registry/vllm:tag", StartupCommand: "vllm serve /models"},
		TaskID:  "task-1", TaskType: "model-import", TaskStatus: "pending",
	}}
	s := NewModelService(nil)
	s.SetBundleImporter(importer)
	audit := &bundleAuditFake{}
	s.SetAuditStore(audit)
	notifier := &importNotifierFake{}
	s.SetImportNotifier(notifier)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-auth", Actor: "actor", Workload: "internal"})
	got, err := s.ImportModelBundle(ctx, bundleImportRequest("tenant-auth"))
	if err != nil {
		t.Fatalf("ImportModelBundle() error = %v", err)
	}
	if importer.input.TenantID != "tenant-auth" {
		t.Fatalf("tenant passed to importer = %q, want authenticated tenant", importer.input.TenantID)
	}
	if importer.input.StartupCommand != "vllm serve /models" || importer.input.RepoID != "Qwen/Qwen3.5-0.8B" {
		t.Fatalf("runtime input = %+v", importer.input)
	}
	if audit.event.Action != "model_import_bundle.create" || audit.event.TaskID != "task-1" || notifier.calls != 1 {
		t.Fatalf("audit/notifier = %+v/%d", audit.event, notifier.calls)
	}
	if got.GetModel().GetId() == "" || got.GetVersion().GetId() == "" || got.GetTask().GetTaskId() == "" {
		t.Fatalf("aggregate response has empty members: %+v", got)
	}
}

func TestImportModelBundleTenantMismatchFailsClosed(t *testing.T) {
	importer := &bundleImporterFake{result: model.BundleImportResult{TaskID: "task-1"}}
	s := NewModelBundleImportService(importer)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-auth", Actor: "actor", Workload: "internal"})
	_, err := s.ImportModelBundle(ctx, bundleImportRequest("tenant-request"))
	if kratoserrors.Code(err) != 403 {
		t.Fatalf("code=%d err=%v, want 403", kratoserrors.Code(err), err)
	}
	if importer.input.TenantID != "" {
		t.Fatalf("importer called after tenant mismatch: %+v", importer.input)
	}
}

func TestImportModelBundleEmptyTenantUsesAuthenticatedTenant(t *testing.T) {
	importer := &bundleImporterFake{result: model.BundleImportResult{
		Model: model.Record{ID: "model-1", Name: "qwen3.5-0.8b"}, Version: model.Version{ID: "version-1", ModelID: "model-1"}, TaskID: "task-1", TaskType: "model-import", TaskStatus: "pending",
	}}
	s := NewModelBundleImportService(importer)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-auth", Actor: "actor", Workload: "internal"})
	if _, err := s.ImportModelBundle(ctx, bundleImportRequest("")); err != nil {
		t.Fatalf("ImportModelBundle() error = %v", err)
	}
	if importer.input.TenantID != "tenant-auth" {
		t.Fatalf("tenant passed to importer = %q, want authenticated tenant", importer.input.TenantID)
	}
}

func TestImportModelBundleReturns503WhenImporterIsUnavailable(t *testing.T) {
	s := NewModelService(nil)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-auth", Actor: "actor", Workload: "internal"})
	_, err := s.ImportModelBundle(ctx, bundleImportRequest("tenant-auth"))
	if kratoserrors.Code(err) != 503 {
		t.Fatalf("code=%d err=%v, want 503", kratoserrors.Code(err), err)
	}
}

func TestImportModelBundleDoesNotNotifyWhenAuditFails(t *testing.T) {
	importer := &bundleImporterFake{result: model.BundleImportResult{
		Model: model.Record{ID: "model-1", Name: "qwen3.5-0.8b"}, Version: model.Version{ID: "version-1", ModelID: "model-1"}, TaskID: "task-1", TaskType: "model-import", TaskStatus: "pending",
	}}
	s := NewModelBundleImportService(importer)
	s.SetAuditStore(&bundleAuditFake{err: errors.New("audit unavailable")})
	notifier := &importNotifierFake{}
	s.SetImportNotifier(notifier)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-auth", Actor: "actor", Workload: "internal"})
	_, err := s.ImportModelBundle(ctx, bundleImportRequest("tenant-auth"))
	if kratoserrors.Code(err) != 500 || notifier.calls != 0 {
		t.Fatalf("err=%v code=%d notifier=%d, want audit failure and no notification", err, kratoserrors.Code(err), notifier.calls)
	}
}

type externalModelSearcherFake struct {
	request model.SearchRequest
	page    model.SearchPage
	err     error
}

func (f *externalModelSearcherFake) Search(_ context.Context, request model.SearchRequest) (model.SearchPage, error) {
	f.request = request
	return f.page, f.err
}

func (f versionReaderFunc) GetVersion(ctx context.Context, tenant, id string) (model.Version, error) {
	return f(ctx, tenant, id)
}
func (f externalVersionReaderFake) GetVersion(ctx context.Context, tenant, id string) (model.Version, error) {
	return f.version, nil
}
func (f externalVersionReaderFake) GetVersionByExternalRef(context.Context, string, string, string) (model.Version, error) {
	return f.version, nil
}

type versionCatalogFake struct {
	created model.Version
	ready   bool
}

type listDefaultsVersionCatalog struct{ versionCatalogFake }

func (f *listDefaultsVersionCatalog) ListVersions(context.Context, string, string, model.ListOptions) ([]model.Version, error) {
	return []model.Version{
		{TenantID: "tenant-a", ID: "v1", ModelID: referenceModelUUID, Version: "v1", Status: "ready", EngineType: "vllm", EngineImage: "registry/vllm:latest", StartupCommand: "exec vllm serve /models"},
		{TenantID: "tenant-a", ID: "v2", ModelID: referenceModelUUID, Version: "v2", Status: "ready", EngineType: "sglang", EngineImage: "registry/sglang:latest", StartupCommand: "exec python -m sglang.launch_server --model /models"},
	}, nil
}

func (f *versionCatalogFake) GetVersion(_ context.Context, tenant, id string) (model.Version, error) {
	if f.created.ID == id && f.created.TenantID == tenant {
		return f.created, nil
	}
	return model.Version{}, nil
}
func (f *versionCatalogFake) GetVersionByExternalRef(context.Context, string, string, string) (model.Version, error) {
	return f.created, nil
}
func (f *versionCatalogFake) CreateVersion(_ context.Context, v model.Version) (model.Version, error) {
	f.created = v
	return v, nil
}
func (f *versionCatalogFake) ListVersions(context.Context, string, string, model.ListOptions) ([]model.Version, error) {
	return nil, nil
}
func (f *versionCatalogFake) CountModelVersions(context.Context, string, string, model.ListOptions) (int64, error) {
	return 0, nil
}
func (f *versionCatalogFake) MarkReady(context.Context, string, string) error {
	f.ready = true
	return nil
}
func (f *versionCatalogFake) MarkError(context.Context, string, string, string) error { return nil }

type artifactFake struct{ created model.Artifact }

func (f *artifactFake) CreateArtifact(_ context.Context, a model.Artifact) (model.Artifact, error) {
	f.created = a
	return a, nil
}
func (f *artifactFake) GetArtifact(context.Context, string, string) (model.Artifact, error) {
	return f.created, nil
}

func TestGetModelVersionUsesRequestTenantWithoutPrincipal(t *testing.T) {
	// SECURITY: Without a principal, the request must fail authentication.
	s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		return model.Version{TenantID: "t", ID: "v", Status: "pending"}, nil
	}))
	_, err := s.GetModelVersion(context.Background(), &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v"})
	if kratoserrors.Code(err) != 401 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestSearchExternalModelsForwardsFiltersAndMapsCandidates(t *testing.T) {
	searcher := &externalModelSearcherFake{page: model.SearchPage{
		Items: []model.SearchResult{{
			Source: "huggingface", RepoID: "Qwen/Qwen3-8B", Name: "Qwen3-8B", DisplayName: "Qwen 3 8B",
			Description: "text model", Revision: "main", SizeBytes: 123, Tags: []string{"text-generation"},
			UpdatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		}}, NextCursor: "opaque", HasMore: true}}
	s := NewModelService(nil)
	s.SetExternalModelSearcher(searcher)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "gateway"})
	got, err := s.SearchExternalModels(ctx, &modelv1.SearchExternalModelsRequest{
		TenantId: "tenant-a", Source: "huggingface", Keyword: "Qwen", Owner: "Qwen", Task: "text-generation", Library: "transformers",
		Page: int32ptr(1), PageSize: int32ptr(7),
	})
	if err != nil {
		t.Fatalf("SearchExternalModels() error = %v", err)
	}
	want := model.SearchRequest{Source: "huggingface", Keyword: "Qwen", Owner: "Qwen", Task: "text-generation", Library: "transformers", Page: 1, PageSize: 7, Limit: 7}
	if searcher.request != want {
		t.Fatalf("search request = %+v, want %+v", searcher.request, want)
	}
	if len(got.GetItems()) != 1 || got.GetItems()[0].GetRepoId() != "Qwen/Qwen3-8B" || got.GetItems()[0].GetRevision() != "main" {
		t.Fatalf("candidates = %+v", got.GetItems())
	}
	if got.GetItems()[0].GetUpdatedAt().AsTime() != searcher.page.Items[0].UpdatedAt || !got.GetHasMore() {
		t.Fatalf("response has_more = %+v", got.GetHasMore())
	}
}

func TestSearchExternalModelsRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		in   *modelv1.SearchExternalModelsRequest
	}{
		{name: "blank keyword", in: &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", PageSize: int32ptr(1)}},
		{name: "unknown source", in: &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "custom", Keyword: "q", PageSize: int32ptr(1)}},
		{name: "invalid page", in: &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", Keyword: "q", Page: int32ptr(0)}},
		{name: "invalid page size", in: &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", Keyword: "q", PageSize: int32ptr(0)}},
		{name: "invalid limit", in: &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", Keyword: "q", PageSize: int32ptr(101)}},
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "gateway"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewModelService(nil)
			searcher := &externalModelSearcherFake{}
			s.SetExternalModelSearcher(searcher)
			_, err := s.SearchExternalModels(ctx, tt.in)
			if kratoserrors.Code(err) != 400 {
				t.Fatalf("code=%v err=%v, want 400", kratoserrors.Code(err), err)
			}
		})
	}
}

func TestSearchExternalModelsRejectsCrossTenant(t *testing.T) {
	s := NewModelService(nil)
	s.SetExternalModelSearcher(&externalModelSearcherFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "gateway"})
	_, err := s.SearchExternalModels(ctx, &modelv1.SearchExternalModelsRequest{TenantId: "tenant-b", Source: "huggingface", Keyword: "q"})
	if kratoserrors.Code(err) != 403 {
		t.Fatalf("code=%v err=%v, want 403", kratoserrors.Code(err), err)
	}
}

func TestSearchExternalModelsMapsProviderFailure(t *testing.T) {
	s := NewModelService(nil)
	s.SetExternalModelSearcher(&externalModelSearcherFake{err: importer.ErrProvider})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "gateway"})
	_, err := s.SearchExternalModels(ctx, &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", Keyword: "q"})
	if kratoserrors.Code(err) != 502 {
		t.Fatalf("code=%v err=%v, want 502", kratoserrors.Code(err), err)
	}
}

func TestSearchExternalModelsRejectsMalformedProviderResult(t *testing.T) {
	s := NewModelService(nil)
	s.SetExternalModelSearcher(&externalModelSearcherFake{page: model.SearchPage{Items: []model.SearchResult{{Source: "huggingface", Name: "missing-repository"}}}})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "gateway"})
	_, err := s.SearchExternalModels(ctx, &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", Keyword: "q"})
	if kratoserrors.Code(err) != 502 {
		t.Fatalf("code=%v err=%v, want 502", kratoserrors.Code(err), err)
	}
}

func TestGetModelVersionChecksReadyArtifact(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "a", Workload: "w"})
	s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		return model.Version{TenantID: "t", ID: "v", ModelID: "m", Version: "1", Format: "gguf", Status: "pending"}, nil
	}))
	_, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v"})
	if kratoserrors.Code(err) != 412 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestGetModelVersionByExternalReference(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "a", Workload: "w"})
	checksum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s := NewModelService(externalVersionReaderFake{version: model.Version{TenantID: "t", ID: "version-uuid", ModelID: "model-uuid", ExternalModelID: "Qwen3-32B", Version: "v2", Format: "gguf", Status: "ready", ArtifactProvider: "minio", ArtifactReference: "t/Qwen3-32B/v2/model.gguf", ArtifactSHA256: checksum}})
	got, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelId: "Qwen3-32B", Version: "v2"})
	if err != nil {
		t.Fatalf("GetModelVersion() error = %v", err)
	}
	if got.GetModel().GetModelId() != "Qwen3-32B" || got.GetVersion().GetModelId() != "Qwen3-32B" || got.GetVersion().GetId() != "version-uuid" {
		t.Fatalf("response = %+v", got)
	}
}

func TestGetModelVersionReturnsModelMetadata(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	catalog := &catalogFake{}
	catalog.model = model.Record{
		TenantID: "tenant-a", ID: "model-uuid", ExternalModelID: "Qwen/Qwen3-8B",
		Name: "qwen3-8b", DisplayName: "Qwen 3 8B", Capabilities: []byte(`["chat","text-generation"]`),
		Status: "ready", Source: "huggingface", SourceRepoID: "Qwen/Qwen3-8B",
	}
	s := NewModelCatalogService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		return model.Version{TenantID: "tenant-a", ID: "version-uuid", ModelID: "model-uuid", Version: "v1", Format: "gguf", Status: "ready", ArtifactProvider: "minio", ArtifactReference: "tenant-a/qwen3-8b/v1/model.gguf", ArtifactSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, nil
	}), catalog)

	got, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "tenant-a", ModelVersionId: "version-uuid"})
	if err != nil {
		t.Fatalf("GetModelVersion() error = %v", err)
	}
	if got.GetModel().GetName() != "qwen3-8b" || got.GetModel().GetDisplayName() != "Qwen 3 8B" {
		t.Fatalf("model names = %q/%q", got.GetModel().GetName(), got.GetModel().GetDisplayName())
	}
	if got.GetModel().GetStatus() != "ready" || got.GetModel().GetSource() != "huggingface" || got.GetModel().GetSourceRepoId() != "Qwen/Qwen3-8B" {
		t.Fatalf("model metadata = %+v", got.GetModel())
	}
	if want := []string{"chat", "text-generation"}; len(got.GetModel().GetCapabilities()) != len(want) || got.GetModel().GetCapabilities()[0] != want[0] || got.GetModel().GetCapabilities()[1] != want[1] {
		t.Fatalf("capabilities = %v, want %v", got.GetModel().GetCapabilities(), want)
	}
}

func TestGetModelVersionPassesPrincipalTenantAndOwningModelIDToCatalog(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	catalog := &getModelCatalogFake{catalogFake: catalogFake{model: model.Record{TenantID: "tenant-a", ID: "model-uuid", Name: "qwen3-8b"}}}
	s := NewModelCatalogService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		return model.Version{TenantID: "tenant-a", ID: "version-uuid", ModelID: "model-uuid", Version: "v1", Format: "gguf", Status: "ready", ArtifactProvider: "minio", ArtifactReference: "tenant-a/model/v1/model.gguf", ArtifactSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, nil
	}), catalog)

	if _, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "tenant-a", ModelVersionId: "version-uuid"}); err != nil {
		t.Fatalf("GetModelVersion() error = %v", err)
	}
	if catalog.gotTenant != "tenant-a" || catalog.gotID != "model-uuid" {
		t.Fatalf("catalog GetModel arguments = tenant %q, id %q; want tenant %q, id %q", catalog.gotTenant, catalog.gotID, "tenant-a", "model-uuid")
	}
}

func TestGetModelVersionMapsCatalogErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code int
		kind string
	}{
		{name: "missing", err: pgx.ErrNoRows, code: 404, kind: "MODEL_NOT_FOUND"},
		{name: "store failure", err: errors.New("catalog unavailable"), code: 500, kind: "MODEL_STORE_ERROR"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
			catalog := &getModelCatalogFake{catalogFake: catalogFake{model: model.Record{TenantID: "tenant-a", ID: "model-uuid"}}, err: tt.err}
			s := NewModelCatalogService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
				return model.Version{TenantID: "tenant-a", ID: "version-uuid", ModelID: "model-uuid", Version: "v1", Format: "gguf", Status: "ready", ArtifactProvider: "minio", ArtifactReference: "tenant-a/model/v1/model.gguf", ArtifactSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, nil
			}), catalog)
			_, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "tenant-a", ModelVersionId: "version-uuid"})
			if kratoserrors.Code(err) != tt.code || kratoserrors.Reason(err) != tt.kind {
				t.Fatalf("error = %v (code %d, reason %q), want code %d reason %q", err, kratoserrors.Code(err), kratoserrors.Reason(err), tt.code, tt.kind)
			}
		})
	}
}

func TestMapStoreMapsDatabaseUnavailableTo503(t *testing.T) {
	for name, err := range map[string]error{
		"connection exception": &pgconn.PgError{Code: "08006", Message: "connection failure"},
		"closed transaction":   pgx.ErrTxClosed,
	} {
		t.Run(name, func(t *testing.T) {
			mapped := mapStore(err)
			if kratoserrors.Code(mapped) != 503 || kratoserrors.Reason(mapped) != "MODEL_STORE_UNAVAILABLE" {
				t.Fatalf("mapStore(%v) = %v (code %d reason %q), want 503 MODEL_STORE_UNAVAILABLE", err, mapped, kratoserrors.Code(mapped), kratoserrors.Reason(mapped))
			}
		})
	}
}

func TestGetModelVersionRejectsMixedSelectors(t *testing.T) {
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t", Actor: "a", Workload: "w"})
	s := NewModelService(versionReaderFunc(func(context.Context, string, string) (model.Version, error) {
		t.Fatal("reader called")
		return model.Version{}, nil
	}))
	_, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "t", ModelVersionId: "v", ModelId: "Qwen3-32B", Version: "v2"})
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestCreateModelRejectsCrossTenant(t *testing.T) {
	s := NewModelCatalogService(nil, &catalogFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.CreateModel(ctx, &modelv1.CreateModelRequest{TenantId: "tenant-b", Name: "demo"})
	if kratoserrors.Code(err) != 403 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestDeleteModelRejectsUnknownReferenceState(t *testing.T) {
	s := NewModelCatalogService(nil, &catalogFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.DeleteModel(ctx, &modelv1.DeleteModelRequest{TenantId: "tenant-a", ModelId: referenceModelUUID})
	if kratoserrors.Code(err) != 503 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestDeleteModelRejectsActiveInferenceReference(t *testing.T) {
	s := NewModelCatalogService(nil, &catalogFake{})
	s.SetInferenceReferenceChecker(referenceCheckerFake{referenced: true})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.DeleteModel(ctx, &modelv1.DeleteModelRequest{TenantId: "tenant-a", ModelId: referenceModelUUID})
	if kratoserrors.Code(err) != 409 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestGetUploadURLRejectsInvalidChecksum(t *testing.T) {
	s := NewModelDownloadService(nil, storageFake{})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "w"})
	_, err := s.GetUploadURL(ctx, &modelv1.GetUploadURLRequest{TenantId: "tenant-a", ModelId: "m", Version: "1", FileName: "model.bin", SizeBytes: 1, ChecksumSha256: "bad"})
	if kratoserrors.Code(err) != 400 {
		t.Fatalf("code=%v err=%v", kratoserrors.Code(err), err)
	}
}

func TestImportModelPersistsProviderPayload(t *testing.T) {
	work := &importWorkFake{}
	s := NewModelImportService(work)
	notifier := &importNotifierFake{}
	s.SetImportNotifier(notifier)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	got, err := s.ImportModel(ctx, &modelv1.ImportModelRequest{
		TenantId:       "tenant-a",
		Source:         "huggingface",
		RepoId:         "org/model",
		Revision:       "main",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("ImportModel() error = %v", err)
	}
	if got.GetStatus() != workbiz.Pending || got.GetTaskType() != "huggingface" {
		t.Fatalf("response = %+v", got)
	}
	if work.got.Source != "huggingface" || work.got.RepoID != "org/model" || work.got.Revision != "main" || work.got.IdempotencyKey != "idem-1" {
		t.Fatalf("persisted task lost provider payload: %+v", work.got)
	}
	if notifier.calls != 1 {
		t.Fatalf("notifier calls=%d, want 1", notifier.calls)
	}
}

func TestImportModelPersistsOptionalVersionBinding(t *testing.T) {
	work := &importWorkFake{}
	s := NewModelImportService(work)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	modelID := "11111111-1111-1111-1111-111111111111"
	versionID := "22222222-2222-2222-2222-222222222222"
	got, err := s.ImportModel(ctx, &modelv1.ImportModelRequest{TenantId: "tenant-a", Source: "huggingface", RepoId: "org/model#model.gguf", IdempotencyKey: "idem-bind", ModelId: modelID, ModelVersionId: versionID})
	if err != nil {
		t.Fatalf("ImportModel() error = %v", err)
	}
	if work.got.ModelID != modelID || work.got.VersionID != versionID || got.GetModelId() != modelID || got.GetModelVersionId() != versionID {
		t.Fatalf("binding lost: %+v", work.got)
	}
}

func TestCreateModelVersionFinalizesUploadedArtifact(t *testing.T) {
	versions := &versionCatalogFake{}
	artifacts := &artifactFake{}
	s := NewModelVersionService(versions)
	s.catalog = &referenceCatalog{}
	s.storage = storageFake{}
	s.SetArtifactStore(artifacts)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "actor", Workload: "gateway"})
	checksum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := s.CreateModelVersion(ctx, &modelv1.CreateModelVersionRequest{
		TenantId: "tenant-a", ModelId: referenceModelUUID, Version: "v1", Format: "gguf",
		EngineType: "vllm", EngineImage: "image", StartupCommand: "exec vllm serve /models",
		StoragePath: "tenant-a/model-a/v1/model.gguf", SizeBytes: 42, ChecksumSha256: checksum,
	})
	if err != nil {
		t.Fatalf("CreateModelVersion() error = %v", err)
	}
	if !versions.ready || got.GetStatus() != "ready" || got.GetSizeBytes() != 42 || versions.created.SizeBytes != 42 || artifacts.created.Reference != "tenant-a/model-a/v1/model.gguf" || artifacts.created.SHA256 != checksum {
		t.Fatalf("version=%+v ready=%v artifact=%+v", got, versions.ready, artifacts.created)
	}
}

func TestCreateAndGetModelVersionPreservesEngineDefaults(t *testing.T) {
	versions := &versionCatalogFake{}
	catalog := &referenceCatalog{}
	s := NewModelServiceWithDependencies(versions, versions, catalog, nil, nil)
	command := "exec  vllm serve '/models'   --served-model-name \"qwen2.5\""
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "test", Workload: "test"})
	created, err := s.CreateModelVersion(ctx, &modelv1.CreateModelVersionRequest{TenantId: "tenant-a", ModelId: "Qwen3-32B", Version: "v1", Format: "gguf", EngineType: "vllm", EngineImage: "registry/vllm:latest", StartupCommand: command})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetId() == "" {
		t.Fatalf("created version id empty: %+v", created)
	}
	if versions.created.EngineType != "vllm" || versions.created.EngineImage != "registry/vllm:latest" || versions.created.StartupCommand != command {
		t.Fatalf("captured defaults: %+v", versions.created)
	}
	versions.created.Status = "ready"
	versions.created.ArtifactProvider, versions.created.ArtifactReference, versions.created.ArtifactSHA256 = "s3", "obj", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := s.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: "tenant-a", ModelVersionId: versions.created.ID})
	if err != nil {
		t.Fatalf("GetModelVersion() error = %v", err)
	}
	if got.GetVersion().GetEngineType() != "vllm" || got.GetVersion().GetEngineImage() != "registry/vllm:latest" || got.GetVersion().GetStartupCommand() != command {
		t.Fatalf("returned defaults: type=%q image=%q command=%q", got.GetVersion().GetEngineType(), got.GetVersion().GetEngineImage(), got.GetVersion().GetStartupCommand())
	}
}

func TestListModelVersionsPreservesPerVersionEngineDefaults(t *testing.T) {
	versions := &listDefaultsVersionCatalog{}
	s := NewModelServiceWithDependencies(versions, versions, &referenceCatalog{}, nil, nil)
	got, err := s.ListModelVersions(referenceContext("tenant-a"), &modelv1.ListModelVersionsRequest{ModelId: referenceModelUUID})
	if err != nil {
		t.Fatalf("ListModelVersions() error = %v", err)
	}
	if len(got.GetItems()) != 2 {
		t.Fatalf("versions = %d, want 2", len(got.GetItems()))
	}
	want := [][3]string{{"vllm", "registry/vllm:latest", "exec vllm serve /models"}, {"sglang", "registry/sglang:latest", "exec python -m sglang.launch_server --model /models"}}
	for i, version := range got.GetItems() {
		if version.GetEngineType() != want[i][0] || version.GetEngineImage() != want[i][1] || version.GetStartupCommand() != want[i][2] {
			t.Fatalf("version %d defaults = (%q, %q, %q)", i, version.GetEngineType(), version.GetEngineImage(), version.GetStartupCommand())
		}
	}
}

func TestSearchExternalModelsMapsHasMoreWithoutTotal(t *testing.T) {
	searcher := &externalModelSearcherFake{page: model.SearchPage{HasMore: true, Items: []model.SearchResult{{Source: "huggingface", RepoID: "org/model", Name: "model", Revision: "main"}}}}
	s := NewModelService(nil)
	s.SetExternalModelSearcher(searcher)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "a", Workload: "gateway"})
	got, err := s.SearchExternalModels(ctx, &modelv1.SearchExternalModelsRequest{TenantId: "tenant-a", Source: "huggingface", Keyword: "model"})
	if err != nil || len(got.GetItems()) != 1 || !got.GetHasMore() {
		t.Fatalf("response=%v err=%v", got, err)
	}
}
