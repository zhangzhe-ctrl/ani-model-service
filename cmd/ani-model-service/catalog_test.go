package main

import (
	"context"
	"github.com/jackc/pgx/v5"
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/data/postgres"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/identity"
	"google.golang.org/protobuf/types/known/durationpb"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCatalogStartupRequiresIdentityConfiguration(t *testing.T) {
	t.Setenv("ANI_MODEL_CLIENT_CA", "")
	t.Setenv("ANI_MODEL_TLS_CERT", "")
	t.Setenv("ANI_MODEL_TLS_KEY", "")
	bc := &conf.Bootstrap{Server: &conf.Server{Grpc: &conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:19090", Timeout: durationpb.New(time.Second)}, Admin: &conf.Server_Admin{Network: "tcp", Addr: "127.0.0.1:19091", Timeout: durationpb.New(time.Second)}, ShutdownTimeout: durationpb.New(time.Second)}}
	err := runCatalogRead(bc, newRuntimeLogger(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("missing identity configuration accepted: %v", err)
	}
}

type catalogDBProbe struct {
	postgres.DBTX
	called bool
}
type missingVersionRow struct{}

func (missingVersionRow) Scan(...any) error { return pgx.ErrNoRows }
func (d *catalogDBProbe) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	d.called = strings.Contains(query, "GetReadyModelVersion") && len(args) == 2
	return missingVersionRow{}
}
func TestCatalogWiresVersionReader(t *testing.T) {
	db := &catalogDBProbe{}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "11111111-1111-4111-8111-111111111111", Actor: "service:ani-inference-service", Workload: "ani-inference-service"})
	_, err := newCatalogService(db, nil).GetModelVersion(ctx, &conf.GetModelVersionRequest{TenantId: "11111111-1111-4111-8111-111111111111", ModelVersionId: "22222222-2222-4222-8222-222222222222"})
	if !db.called || err == nil {
		t.Fatalf("version SQL not reached: called=%v err=%v", db.called, err)
	}
}
