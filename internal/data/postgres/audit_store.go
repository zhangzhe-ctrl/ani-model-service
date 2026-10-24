package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	auditbiz "github.com/liangzai006/ani-model-service/internal/biz/audit"
)

type AuditStore struct{ q *Queries }

func NewAuditStore(db DBTX) *AuditStore { return &AuditStore{q: New(db)} }
func (s *AuditStore) Insert(ctx context.Context, e auditbiz.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	tenant, err := uuid.Parse(e.TenantID)
	if err != nil {
		return fmt.Errorf("tenant_id: %w", err)
	}
	task := uuid.Nil
	if e.TaskID != "" {
		task, err = uuid.Parse(e.TaskID)
		if err != nil {
			return fmt.Errorf("task_id: %w", err)
		}
	}
	modelID, err := optionalAuditID(e.ModelID)
	if err != nil {
		return fmt.Errorf("model_id: %w", err)
	}
	versionID, err := optionalAuditID(e.ModelVersionID)
	if err != nil {
		return fmt.Errorf("model_version_id: %w", err)
	}
	return s.q.InsertAuditEvent(ctx, InsertAuditEventParams{TenantID: uuidType(tenant), ID: uuidType(uuid.New()), Actor: e.Actor, Workload: e.Workload, RequestID: e.RequestID, Column6: task.String(), Action: e.Action, BeforeState: e.BeforeState, AfterState: e.AfterState, ErrorClass: e.ErrorClass, ModelID: modelID, ModelVersionID: versionID})
}

func optionalAuditID(value string) (pgtype.UUID, error) {
	if value == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return uuidType(id), nil
}

func (s *AuditStore) ListModelOperations(ctx context.Context, tenant, modelID string, options auditbiz.ListOptions) ([]auditbiz.Event, int64, error) {
	t, m, err := parseModelIDs(tenant, modelID)
	if err != nil {
		return nil, 0, err
	}
	read := func(q *Queries) ([]auditbiz.Event, int64, error) {
		total, err := q.CountModelOperations(ctx, CountModelOperationsParams{TenantID: uuidType(t), ModelID: uuidType(m)})
		if err != nil {
			return nil, 0, err
		}
		rows, err := q.ListModelOperations(ctx, ListModelOperationsParams{TenantID: uuidType(t), ModelID: uuidType(m), PageLimit: options.Limit, PageOffset: int32(options.Offset)})
		if err != nil {
			return nil, 0, err
		}
		items := make([]auditbiz.Event, len(rows))
		for i, row := range rows {
			items[i] = auditbiz.Event{ID: uuidString(row.ID), TenantID: tenant, ModelID: uuidString(row.ResolvedModelID), ModelVersionID: uuidString(row.ResolvedModelVersionID), Actor: row.Actor, Workload: row.Workload, RequestID: row.RequestID, TaskID: uuidString(row.TaskID), Action: row.Action, BeforeState: row.BeforeState, AfterState: row.AfterState, ErrorClass: row.ErrorClass, CreatedAt: row.CreatedAt.Time}
		}
		return items, total, nil
	}
	if db, ok := s.q.db.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	}); ok {
		tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, 0, err
		}
		defer tx.Rollback(context.Background())
		items, total, err := read(New(tx))
		if err == nil {
			err = tx.Commit(ctx)
		}
		return items, total, err
	}
	return read(s.q)
}
