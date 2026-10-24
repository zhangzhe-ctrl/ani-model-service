package audit

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrInvalidEvent = errors.New("invalid audit event")

type Store interface {
	Insert(context.Context, Event) error
}

type ListOptions struct {
	Limit  int32
	Offset int64
}

type Reader interface {
	ListModelOperations(context.Context, string, string, ListOptions) ([]Event, int64, error)
}

type Event struct {
	TenantID, Actor, Workload, RequestID                string
	TaskID, Action, BeforeState, AfterState, ErrorClass string
	ID, ModelID, ModelVersionID                         string
	CreatedAt                                           time.Time
}

// Result describes the outcome at the time of this operation. An accepted
// import is not a successful import; task status is queried separately.
func (e Event) Result() string {
	if e.ErrorClass != "" || e.AfterState == "failed" || e.AfterState == "error" {
		return "failed"
	}
	if strings.HasPrefix(e.Action, "model_import") && (e.AfterState == "pending" || e.AfterState == "importing") {
		return "accepted"
	}
	return "succeeded"
}

func (e Event) Validate() error {
	if e.TenantID == "" || e.Actor == "" || e.Workload == "" || e.RequestID == "" || e.Action == "" {
		return ErrInvalidEvent
	}
	if e.BeforeState == "" && e.AfterState == "" && e.ErrorClass == "" {
		return ErrInvalidEvent
	}
	return nil
}
