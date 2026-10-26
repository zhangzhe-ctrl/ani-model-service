package service

import (
	"context"

	errors2 "github.com/go-kratos/kratos/v3/errors"
	auditbiz "github.com/liangzai006/ani-model-service/internal/biz/audit"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func (s *ModelService) ListModelOperations(ctx context.Context, in *modelv1.ListModelOperationsRequest) (*modelv1.ListModelOperationsResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, mapIdentity(err)
	}
	options, err := resolvePage(in.Page, in.PageSize)
	if err != nil {
		return nil, err
	}
	m, err := s.resolveModel(ctx, p.TenantID, in.GetModelId())
	if err != nil {
		return nil, err
	}
	reader, ok := s.audit.(auditbiz.Reader)
	if !ok {
		return nil, errors2.New(503, "AUDIT_UNAVAILABLE", "operation history is not configured")
	}
	events, total, err := reader.ListModelOperations(ctx, p.TenantID, m.ID, auditbiz.ListOptions{Limit: options.Limit, Offset: options.Offset})
	if err != nil {
		return nil, mapStore(err)
	}
	items := make([]*modelv1.ModelOperation, len(events))
	for i, event := range events {
		items[i] = &modelv1.ModelOperation{Id: event.ID, ModelId: event.ModelID, ModelVersionId: event.ModelVersionID, TaskId: event.TaskID, Action: event.Action, Actor: event.Actor, Result: event.Result(), BeforeState: event.BeforeState, AfterState: event.AfterState, ErrorClass: event.ErrorClass, RequestId: event.RequestID, CreatedAt: protoTime(event.CreatedAt)}
	}
	return &modelv1.ListModelOperationsResponse{Items: items, Total: total}, nil
}
