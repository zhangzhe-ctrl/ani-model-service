package service

import (
	"context"
	"errors"
	"testing"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/jackc/pgx/v5"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

const referenceModelUUID = "11111111-1111-4111-8111-111111111111"

type referenceCatalog struct {
	catalogFake
	err       error
	deletedID string
}

func (c *referenceCatalog) GetModel(_ context.Context, tenant, id string) (model.Record, error) {
	if c.err != nil {
		return model.Record{}, c.err
	}
	if tenant != "tenant-a" || id != referenceModelUUID {
		return model.Record{}, pgx.ErrNoRows
	}
	return model.Record{TenantID: tenant, ID: id, ExternalModelID: "Qwen3-32B", Name: "qwen3-32b"}, nil
}
func (c *referenceCatalog) GetModelByExternalID(ctx context.Context, tenant, id string) (model.Record, error) {
	if id != "Qwen3-32B" {
		return model.Record{}, pgx.ErrNoRows
	}
	return c.GetModel(ctx, tenant, referenceModelUUID)
}
func (c *referenceCatalog) DeleteModel(ctx context.Context, tenant, id string, refs model.ReferenceChecker) error {
	if tenant != "tenant-a" || (id != referenceModelUUID && id != "Qwen3-32B") {
		return pgx.ErrNoRows
	}
	if err := c.catalogFake.DeleteModel(ctx, tenant, id, refs); err != nil {
		return err
	}
	c.deletedID = referenceModelUUID
	return nil
}

type referenceVersionCatalog struct {
	versionCatalogFake
	listedID string
}

func (c *referenceVersionCatalog) ListVersions(_ context.Context, tenant, id string, _ model.ListOptions) ([]model.Version, error) {
	c.listedID = id
	return []model.Version{{TenantID: tenant, ID: "version-uuid", ModelID: id, Version: "v2", Format: "gguf", Status: "pending"}}, nil
}

type referenceCheckFunc func(context.Context, string, []string) (bool, error)

func (f referenceCheckFunc) HasActiveVersionReferences(ctx context.Context, tenant string, ids []string) (bool, error) {
	return f(ctx, tenant, ids)
}

func referenceContext(tenant string) context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: tenant, Actor: "actor", Workload: "gateway"})
}

func TestModelCRUDResolvesExternalID(t *testing.T) {
	for _, selector := range []string{"Qwen3-32B", referenceModelUUID} {
		t.Run(selector, func(t *testing.T) {
			catalog, versions := &referenceCatalog{}, &referenceVersionCatalog{}
			s := NewModelServiceWithDependencies(versions, versions, catalog, nil, nil)
			ctx := referenceContext("tenant-a")
			got, err := s.GetModel(ctx, &modelv1.GetModelRequest{ModelId: selector})
			if err != nil || got.GetId() != referenceModelUUID || got.GetModelId() != "Qwen3-32B" {
				t.Fatalf("get=%v error=%v", got, err)
			}
			created, err := s.CreateModelVersion(ctx, &modelv1.CreateModelVersionRequest{ModelId: selector, Version: "v2", Format: "gguf", EngineType: "vllm", EngineImage: "image", StartupCommand: "exec vllm serve /models"})
			if err != nil || versions.created.ModelID != referenceModelUUID || created.GetModelId() != "Qwen3-32B" {
				t.Fatalf("create=%v stored=%+v error=%v", created, versions.created, err)
			}
			listed, err := s.ListModelVersions(ctx, &modelv1.ListModelVersionsRequest{ModelId: selector})
			if err != nil || versions.listedID != referenceModelUUID || len(listed.GetItems()) != 1 || listed.GetItems()[0].GetModelId() != "Qwen3-32B" {
				t.Fatalf("list=%v internal_id=%s error=%v", listed, versions.listedID, err)
			}
		})
	}
}

func TestModelReferenceResolutionRejectsTenantMismatchAndMissingModel(t *testing.T) {
	for _, tc := range []struct {
		name, principal, requested, selector string
		code                                 int
	}{
		{"mismatch", "tenant-a", "tenant-b", "Qwen3-32B", 403},
		{"other tenant", "tenant-b", "tenant-b", "Qwen3-32B", 404},
		{"case sensitive", "tenant-a", "tenant-a", "qwen3-32b", 404},
		{"empty selector", "tenant-a", "tenant-a", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			versions := &referenceVersionCatalog{}
			s := NewModelServiceWithDependencies(versions, versions, &referenceCatalog{}, nil, nil)
			ctx := referenceContext(tc.principal)
			_, err := s.GetModel(ctx, &modelv1.GetModelRequest{TenantId: tc.requested, ModelId: tc.selector})
			if kratoserrors.Code(err) != tc.code {
				t.Fatalf("get code=%d error=%v", kratoserrors.Code(err), err)
			}
			_, err = s.CreateModelVersion(ctx, &modelv1.CreateModelVersionRequest{TenantId: tc.requested, ModelId: tc.selector, Version: "v2", Format: "gguf"})
			if kratoserrors.Code(err) != tc.code || versions.created.ID != "" {
				t.Fatalf("create code=%d error=%v", kratoserrors.Code(err), err)
			}
			_, err = s.ListModelVersions(ctx, &modelv1.ListModelVersionsRequest{TenantId: tc.requested, ModelId: tc.selector})
			if kratoserrors.Code(err) != tc.code || versions.listedID != "" {
				t.Fatalf("list code=%d error=%v", kratoserrors.Code(err), err)
			}
		})
	}
}

func TestDeleteModelChecksVersionIdentifiers(t *testing.T) {
	for _, selector := range []string{"Qwen3-32B", referenceModelUUID} {
		for _, active := range []bool{true, false} {
			t.Run(selector, func(t *testing.T) {
				catalog := &referenceCatalog{}
				s := NewModelCatalogService(nil, catalog)
				checked := map[string]bool{}
				s.SetInferenceReferenceChecker(referenceCheckFunc(func(_ context.Context, tenant string, ids []string) (bool, error) {
					if tenant != "tenant-a" {
						t.Fatalf("wrong tenant %s", tenant)
					}
					for _, id := range ids {
						checked[id] = true
					}
					return active, nil
				}))
				_, err := s.DeleteModel(referenceContext("tenant-a"), &modelv1.DeleteModelRequest{ModelId: selector})
				if active {
					if kratoserrors.Code(err) != 409 || catalog.deletedID != "" {
						t.Fatalf("delete error=%v deleted=%s", err, catalog.deletedID)
					}
				} else if err != nil || catalog.deletedID != referenceModelUUID || len(checked) != 1 || !checked["22222222-2222-4222-8222-222222222222"] {
					t.Fatalf("delete error=%v deleted=%s checked=%v", err, catalog.deletedID, checked)
				}
			})
		}
	}
}

func TestDeleteModelVersionProtection(t *testing.T) {
	id := "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name       string
		checker    model.ReferenceChecker
		tenant, id string
		code       int
	}{
		{name: "unconfigured", tenant: "tenant-a", id: id, code: 503},
		{name: "in use", checker: referenceCheckerFake{referenced: true}, tenant: "tenant-a", id: id, code: 409},
		{name: "provider failure", checker: referenceCheckerFake{err: errors.New("offline")}, tenant: "tenant-a", id: id, code: 503},
		{name: "not referenced", checker: referenceCheckerFake{}, tenant: "tenant-a", id: id, code: 200},
		{name: "tenant mismatch", checker: referenceCheckerFake{}, tenant: "tenant-b", id: id, code: 403},
		{name: "bad id", checker: referenceCheckerFake{}, tenant: "tenant-a", id: "operation-id", code: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewModelCatalogService(nil, &catalogFake{})
			s.SetInferenceReferenceChecker(tc.checker)
			_, err := s.DeleteModelVersion(referenceContext("tenant-a"), &modelv1.DeleteModelVersionRequest{TenantId: tc.tenant, ModelVersionId: tc.id})
			if tc.code == 200 {
				if err != nil {
					t.Fatal(err)
				}
			} else if kratoserrors.Code(err) != tc.code {
				t.Fatalf("error=%v code=%d", err, kratoserrors.Code(err))
			}
		})
	}
}

func TestGetModelProviderErrorIsNotNotFound(t *testing.T) {
	s := NewModelCatalogService(nil, &referenceCatalog{err: errors.New("database unavailable")})
	_, err := s.GetModel(referenceContext("tenant-a"), &modelv1.GetModelRequest{ModelId: "Qwen3-32B"})
	if kratoserrors.Code(err) != 500 {
		t.Fatalf("code=%d error=%v", kratoserrors.Code(err), err)
	}
}

func TestImportModelResolvesExternalModelID(t *testing.T) {
	work := &importWorkFake{}
	catalog := &referenceCatalog{}
	s := NewModelServiceWithDependencies(nil, nil, catalog, nil, work)
	_, err := s.ImportModel(referenceContext("tenant-a"), &modelv1.ImportModelRequest{Source: "huggingface", RepoId: "Qwen/Qwen3-32B#model.gguf", Revision: "v2", IdempotencyKey: "import-external", ModelId: "Qwen3-32B"})
	if err != nil || work.got.ModelID != referenceModelUUID {
		t.Fatalf("task=%+v error=%v", work.got, err)
	}
}
