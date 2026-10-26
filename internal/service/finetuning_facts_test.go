package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	kratoserrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

type fixedSourceStorage struct {
	storageFake
	inventory model.ArtifactInventory
	reads     int
}

func (s *fixedSourceStorage) InspectFinetuningArtifact(context.Context, string, string, int64, string) (model.ArtifactInventory, error) {
	s.reads++
	return s.inventory, nil
}

func TestResolveFinetuningFactsWithoutTargetReadsOnlyExistingSource(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	version := "22222222-2222-4222-8222-222222222222"
	owner := "33333333-3333-4333-8333-333333333333"
	config := []byte(`{"model_type":"qwen2"}`)
	h := sha256.Sum256(config)
	digest := hex.EncodeToString(h[:])
	st := &fixedSourceStorage{inventory: model.ArtifactInventory{Bucket: "models", Key: tenant + "/base.tar", VersionID: "existing-version", SHA256: strings.Repeat("a", 64), SizeBytes: 100, ConfigJSON: config, TokenizerConfigJSON: config}}
	for _, name := range []string{"config.json", "model.safetensors", "tokenizer.json", "tokenizer_config.json"} {
		st.inventory.Files = append(st.inventory.Files, model.ArtifactFile{Path: name, SizeBytes: int64(len(config)), SHA256: digest})
	}
	cat := &catalogFake{model: model.Record{TenantID: tenant, ID: owner}}
	reader := versionReaderFunc(func(_ context.Context, gotTenant, gotVersion string) (model.Version, error) {
		if gotTenant != tenant || gotVersion != version {
			t.Fatal("source selector was changed")
		}
		return model.Version{TenantID: tenant, ID: version, ModelID: owner, Version: "v1", Status: model.VersionReady, Format: "safetensors", ArtifactProvider: "s3", ArtifactReference: st.inventory.Key, ArtifactSHA256: st.inventory.SHA256, SizeBytes: 100}, nil
	})
	s := NewModelServiceWithDependencies(reader, nil, cat, st, nil)
	registrar := &trainedArtifactRegistrarFake{}
	notifier := &importNotifierFake{}
	s.SetTrainedArtifactRegistrar(registrar)
	s.SetImportNotifier(notifier)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{TenantID: tenant, Actor: "modeldev:execution", Workload: "ani-modeldev-service"})
	in := &modelv1.ResolveFinetuningFactsRequest{TenantId: tenant, BaseModelVersionId: version}
	out, err := s.ResolveFinetuningFacts(ctx, in)
	if err != nil || out == nil || out.Target != nil || out.ValidateFor(in) != nil || st.reads != 1 {
		t.Fatalf("source-only response=%+v err=%v reads=%d", out, err, st.reads)
	}
	if cat.created.ID != "" || registrar.prepareCalls != 0 || registrar.uploadedCalls != 0 || registrar.completeCalls != 0 || notifier.calls != 0 {
		t.Fatal("read-only source resolution wrote or started model registration/import")
	}
	in.Target = &modelv1.ResolveFinetuningFactsRequest_NewModelName{}
	if _, err := s.ResolveFinetuningFacts(ctx, in); kratoserrors.Code(err) != 400 || st.reads != 1 {
		t.Fatal("typed empty target was accepted")
	}
	in.Target = nil
	in.BaseModelVersionId = "not-a-uuid"
	if _, err := s.ResolveFinetuningFacts(ctx, in); kratoserrors.Code(err) != 400 || st.reads != 1 {
		t.Fatal("invalid base UUID reached source storage")
	}
	in.BaseModelVersionId = version
	in.TenantId = "44444444-4444-4444-8444-444444444444"
	if _, err := s.ResolveFinetuningFacts(ctx, in); err == nil || st.reads != 1 {
		t.Fatal("request tenant overrode current caller")
	}
}
