package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func int32ptr(v int32) *int32 { return &v }

func paginationRecords(n int, tenant string) []model.Record {
	created := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	rs := make([]model.Record, n)
	for i := range rs {
		rs[i] = model.Record{TenantID: tenant, ID: fmt.Sprintf("11111111-1111-4111-8111-%012d", n-i), Name: fmt.Sprintf("model-%02d", i), CreatedAt: created.Add(-time.Duration(i) * time.Minute)}
	}
	return rs
}

type paginationCatalog struct {
	catalogFake
	records model.Record
	options []model.ListOptions
}

func (c *paginationCatalog) ListModels(_ context.Context, tenant string, options model.ListOptions) ([]model.Record, error) {
	c.options = append(c.options, options)
	all := paginationRecords(45, tenant)
	start := int(options.Offset)
	if start >= len(all) {
		return []model.Record{}, nil
	}
	end := start + int(options.Limit)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], nil
}
func (c *paginationCatalog) CountModels(context.Context, string, model.ListOptions) (int64, error) {
	return 45, nil
}

type paginationVersions struct {
	versionCatalogFake
	options []model.ListOptions
}

func (v *paginationVersions) ListVersions(_ context.Context, tenant, modelID string, options model.ListOptions) ([]model.Version, error) {
	v.options = append(v.options, options)
	created := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	all := make([]model.Version, 31)
	for i := range all {
		all[i] = model.Version{ID: fmt.Sprintf("22222222-2222-4222-8222-%012d", len(all)-i), TenantID: tenant, ModelID: modelID, Version: fmt.Sprintf("v%d", len(all)-i), CreatedAt: created.Add(-time.Duration(i) * time.Minute)}
	}
	start := int(options.Offset)
	if start >= len(all) {
		return []model.Version{}, nil
	}
	end := start + int(options.Limit)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], nil
}
func (v *paginationVersions) CountModelVersions(context.Context, string, string, model.ListOptions) (int64, error) {
	return 31, nil
}

func TestListModelsPageAndTotal(t *testing.T) {
	catalog := &paginationCatalog{}
	server := NewModelCatalogService(nil, catalog)
	first, err := server.ListModels(referenceContext("tenant-a"), &modelv1.ListModelsRequest{Page: int32ptr(1), PageSize: int32ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.GetItems()) != 2 || first.GetTotal() != 45 || first.Items[0].Name != "model-00" {
		t.Fatalf("page 1 = %v", first)
	}
	third, err := server.ListModels(referenceContext("tenant-a"), &modelv1.ListModelsRequest{Page: int32ptr(3), PageSize: int32ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.GetItems()) != 2 || third.GetTotal() != first.GetTotal() || third.Items[0].Name != "model-04" {
		t.Fatalf("page 3 = %v", third)
	}
	if got := catalog.options[len(catalog.options)-1]; got.Offset != 4 || got.Limit != 2 {
		t.Fatalf("page options = %+v", got)
	}
}

func TestListModelsDefaultsAndEmptyPage(t *testing.T) {
	catalog := &paginationCatalog{}
	server := NewModelCatalogService(nil, catalog)
	result, err := server.ListModels(referenceContext("tenant-a"), &modelv1.ListModelsRequest{})
	if err != nil || len(result.GetItems()) != 20 || result.GetTotal() != 45 {
		t.Fatalf("default page = %v err=%v", result, err)
	}
	empty, err := server.ListModels(referenceContext("tenant-a"), &modelv1.ListModelsRequest{Page: int32ptr(100), PageSize: int32ptr(100)})
	if err != nil || len(empty.GetItems()) != 0 || empty.GetTotal() != 45 {
		t.Fatalf("empty page = %v err=%v", empty, err)
	}
}

func TestListModelsRejectsInvalidPageValues(t *testing.T) {
	catalog := &paginationCatalog{}
	server := NewModelCatalogService(nil, catalog)
	for _, tc := range []struct {
		name string
		page *int32
		size *int32
	}{
		{name: "page zero", page: int32ptr(0)},
		{name: "page too large", page: int32ptr(101)},
		{name: "size zero", size: int32ptr(0)},
		{name: "size too large", size: int32ptr(101)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := server.ListModels(referenceContext("tenant-a"), &modelv1.ListModelsRequest{Page: tc.page, PageSize: tc.size})
			if kratoserrors.Code(err) != 400 {
				t.Fatalf("err=%v, want invalid argument", err)
			}
		})
	}
	if len(catalog.options) != 0 {
		t.Fatalf("store called for invalid page: %+v", catalog.options)
	}
}

func TestListModelVersionsPageAndTotal(t *testing.T) {
	versions := &paginationVersions{}
	server := NewModelServiceWithDependencies(versions, versions, &catalogFake{}, nil, nil)
	first, err := server.ListModelVersions(referenceContext("tenant-a"), &modelv1.ListModelVersionsRequest{ModelId: referenceModelUUID, Page: int32ptr(1), PageSize: int32ptr(2)})
	if err != nil || len(first.GetItems()) != 2 || first.GetTotal() != 31 || first.Items[0].GetVersion() != "v31" {
		t.Fatalf("page 1 = %v err=%v", first, err)
	}
	third, err := server.ListModelVersions(referenceContext("tenant-a"), &modelv1.ListModelVersionsRequest{ModelId: referenceModelUUID, Page: int32ptr(3), PageSize: int32ptr(2)})
	if err != nil || len(third.GetItems()) != 2 || third.GetTotal() != first.GetTotal() || third.Items[0].GetVersion() != "v27" {
		t.Fatalf("page 3 = %v err=%v", third, err)
	}
	if got := versions.options[len(versions.options)-1]; got.Offset != 4 || got.Limit != 2 {
		t.Fatalf("page options = %+v", got)
	}
}
