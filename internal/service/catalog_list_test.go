package service

import (
	"context"
	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/biz/model"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/identity"
	"testing"
)

type listProbe struct {
	catalogFake
	calls  int
	tenant string
	limit  int32
	err    error
}

func (p *listProbe) ListModels(_ context.Context, tenant, state string, limit int32) ([]model.Record, error) {
	p.calls++
	p.tenant = tenant
	p.limit = limit
	return nil, p.err
}
func TestCatalogListContract(t *testing.T) {
	p := &listProbe{}
	s := NewModelServiceWithDependencies(nil, nil, p, nil, nil)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Actor: "governance:user:7", Workload: "ani-governance"})
	for _, req := range []*modelv1.ListModelsRequest{{Keyword: "x"}, {Source: "x"}, {Capability: "x"}, {Page: &modelv1.CursorPageRequest{Limit: 1, Cursor: "x"}}, {Page: &modelv1.CursorPageRequest{Limit: 0}}, {Page: &modelv1.CursorPageRequest{Limit: -1}}, {Page: &modelv1.CursorPageRequest{Limit: 1001}}, {Status: "unknown"}} {
		if _, err := s.ListModels(ctx, req); kratoserrors.Code(err) != 400 {
			t.Fatalf("bad input accepted: %v %v", req, err)
		}
	}
	if p.calls != 0 {
		t.Fatal("invalid query reached catalog")
	}
	if _, err := s.ListModels(ctx, &modelv1.ListModelsRequest{TenantId: "tenant-b"}); kratoserrors.Code(err) != 403 {
		t.Fatalf("tenant mismatch: %v", err)
	}
	for _, limit := range []int32{1, 1000} {
		out, err := s.ListModels(ctx, &modelv1.ListModelsRequest{Page: &modelv1.CursorPageRequest{Limit: limit}})
		if err != nil || out.Models == nil || p.limit != limit || p.tenant != "tenant-a" {
			t.Fatalf("valid query: %v %v", out, err)
		}
	}
	p.err = context.DeadlineExceeded
	if out, err := s.ListModels(ctx, &modelv1.ListModelsRequest{}); kratoserrors.Code(err) != 503 || out != nil {
		t.Fatalf("store error masked: %v %v", out, err)
	}
}
