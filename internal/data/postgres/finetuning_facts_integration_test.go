package postgres_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/liangzai006/ani-model-service/internal/data/postgres"
	storagedata "github.com/liangzai006/ani-model-service/internal/data/storage"
	"github.com/liangzai006/ani-model-service/internal/identity"
	"github.com/liangzai006/ani-model-service/internal/service"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func TestFinetuningFactsPostgresReadsOriginalTenantAndVersionWithoutWrites(t *testing.T) {
	dsn := os.Getenv("MODEL_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_DATABASE_URL not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	tenant, other, base, target, foreign, version := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range []string{"config.json", "model.safetensors", "tokenizer.json", "tokenizer_config.json"} {
		body := []byte(`{"model_type":"qwen2"}`)
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := archive.Bytes()
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	key := tenant + "/imports/base/model.tar"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ tenant, id, name string }{{tenant, base, "base"}, {tenant, target, "existing-target"}, {other, foreign, "foreign"}} {
		exec(`INSERT INTO models(tenant_id,id,model_id,name,source,status) VALUES($1,$2,$3,$3,'upload','ready')`, row.tenant, row.id, row.name)
	}
	exec(`INSERT INTO model_versions(tenant_id,id,model_id,version,format,status,size_bytes,checksum_sha256) VALUES($1,$2,$3,'original','safetensors','ready',$4,$5)`, tenant, version, base, len(raw), digest)
	exec(`INSERT INTO model_artifacts(tenant_id,id,model_version_id,provider,reference,format,size_bytes,sha256) VALUES($1,$2,$3,'s3',$4,'safetensors',$5,$6)`, tenant, uuid.NewString(), version, key, len(raw), digest)
	gets := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
			return
		}
		if r.URL.Path != "/models/"+key {
			t.Errorf("foreign storage read %s", r.URL)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		w.Header().Set("x-amz-version-id", "original-object-version")
		w.Header().Set("ETag", `"not-the-sha"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		if r.Method == http.MethodGet {
			gets++
			if r.URL.Query().Get("versionId") != "original-object-version" {
				t.Errorf("latest GET %s", r.URL)
			}
			_, _ = w.Write(raw)
		} else if r.Method != http.MethodHead {
			t.Errorf("unexpected storage write %s", r.Method)
			w.WriteHeader(405)
		}
	}))
	defer endpoint.Close()
	storage, err := storagedata.NewMinIOAdapter(strings.TrimPrefix(endpoint.URL, "http://"), "access", "secret", "models", false)
	if err != nil {
		t.Fatal(err)
	}
	cat := postgres.NewCatalogStore(postgres.NewModelStore(tx))
	versions := postgres.NewVersionStore(tx)
	app := service.NewModelServiceWithDependencies(versions, versions, cat, storage, nil)
	principal := identity.WithPrincipal(ctx, identity.Principal{TenantID: tenant, Actor: "modeldev:integration", Workload: "ani-modeldev-service"})
	request := &modelv1.ResolveFinetuningFactsRequest{TenantId: tenant, BaseModelVersionId: version, Target: &modelv1.ResolveFinetuningFactsRequest_TargetModelId{TargetModelId: target}}
	result, err := app.ResolveFinetuningFacts(principal, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	if gets != 1 || result.ModelId != base || result.ObjectVersionId != "original-object-version" || result.Sha256 != digest {
		t.Fatalf("facts=%+v GET=%d", result, gets)
	}
	// Source/target denial occurs before any additional object read.
	for _, test := range []struct {
		name    string
		request *modelv1.ResolveFinetuningFactsRequest
	}{
		{"foreign tenant", &modelv1.ResolveFinetuningFactsRequest{TenantId: other, BaseModelVersionId: version, Target: request.Target}},
		{"foreign target", &modelv1.ResolveFinetuningFactsRequest{TenantId: tenant, BaseModelVersionId: version, Target: &modelv1.ResolveFinetuningFactsRequest_TargetModelId{TargetModelId: foreign}}},
		{"name conflict", &modelv1.ResolveFinetuningFactsRequest{TenantId: tenant, BaseModelVersionId: version, Target: &modelv1.ResolveFinetuningFactsRequest_NewModelName{NewModelName: "existing-target"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := app.ResolveFinetuningFacts(principal, test.request); err == nil {
				t.Fatal("unauthorized facts accepted")
			}
			if gets != 1 {
				t.Fatal("read source before denial")
			}
		})
	}
	var models, versionRows, registrations int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM models WHERE tenant_id=$1),(SELECT count(*) FROM model_versions WHERE tenant_id=$1),(SELECT count(*) FROM trained_artifact_registrations WHERE tenant_id=$1)`, tenant).Scan(&models, &versionRows, &registrations); err != nil {
		t.Fatal(err)
	}
	if models != 2 || versionRows != 1 || registrations != 0 {
		t.Fatalf("read-only facts changed business rows %d/%d/%d", models, versionRows, registrations)
	}
}
