package service

import (
	"context"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	auditbiz "github.com/liangzai006/ani-model-service/internal/biz/audit"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

type operationAuditFake struct {
	bundleAuditFake
	options         auditbiz.ListOptions
	tenant, modelID string
	calls           int
}

func (f *operationAuditFake) ListModelOperations(_ context.Context, tenant, modelID string, options auditbiz.ListOptions) ([]auditbiz.Event, int64, error) {
	f.calls++
	f.tenant, f.modelID, f.options = tenant, modelID, options
	return []auditbiz.Event{{ID: "event", ModelID: modelID, Actor: "operator", Action: "model_import.create", AfterState: "pending", CreatedAt: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}}, 21, nil
}

func TestListModelOperationsScopesAndMapsHistory(t *testing.T) {
	store := &operationAuditFake{}
	s := NewModelCatalogService(nil, &referenceCatalog{})
	s.SetAuditStore(store)
	got, err := s.ListModelOperations(referenceContext("tenant-a"), &modelv1.ListModelOperationsRequest{ModelId: "Qwen3-32B", Page: int32ptr(2), PageSize: int32ptr(10)})
	if err != nil {
		t.Fatal(err)
	}
	if store.tenant != "tenant-a" || store.modelID != referenceModelUUID || store.options.Offset != 10 || store.options.Limit != 10 {
		t.Fatalf("scope = %+v", store)
	}
	if got.Total != 21 || len(got.Items) != 1 || got.Items[0].Result != "accepted" || got.Items[0].Actor != "operator" || got.Items[0].CreatedAt == nil {
		t.Fatalf("history = %v", got)
	}
	_, err = s.ListModelOperations(referenceContext("tenant-a"), &modelv1.ListModelOperationsRequest{TenantId: "tenant-b", ModelId: referenceModelUUID})
	if err == nil || store.calls != 1 {
		t.Fatalf("cross tenant err=%v calls=%d", err, store.calls)
	}
	_, err = s.ListModelOperations(referenceContext("tenant-a"), &modelv1.ListModelOperationsRequest{ModelId: referenceModelUUID, Page: int32ptr(0)})
	if kratoserrors.Code(err) != 400 || store.calls != 1 {
		t.Fatalf("invalid pagination err=%v calls=%d", err, store.calls)
	}
}

func TestListModelVersionsStatusValidationAndAliases(t *testing.T) {
	for input, want := range map[string]string{"ready": "ready", "downloading": "importing", "failed": "error", "pending": "pending", "": ""} {
		t.Run(input, func(t *testing.T) {
			versions := &paginationVersions{}
			s := NewModelServiceWithDependencies(versions, versions, &referenceCatalog{}, nil, nil)
			_, err := s.ListModelVersions(referenceContext("tenant-a"), &modelv1.ListModelVersionsRequest{ModelId: referenceModelUUID, Status: input})
			if err != nil {
				t.Fatal(err)
			}
			if len(versions.options) != 1 || versions.options[0].Status != want {
				t.Fatalf("filter = %+v", versions.options)
			}
		})
	}
	versions := &paginationVersions{}
	s := NewModelServiceWithDependencies(versions, versions, &referenceCatalog{}, nil, nil)
	_, err := s.ListModelVersions(referenceContext("tenant-a"), &modelv1.ListModelVersionsRequest{ModelId: referenceModelUUID, Status: "deleted"})
	if kratoserrors.Code(err) != 400 || len(versions.options) != 0 {
		t.Fatalf("invalid status err=%v queries=%v", err, versions.options)
	}
}
