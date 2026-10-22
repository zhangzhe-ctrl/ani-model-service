package modelv1

import (
	"crypto/sha256"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
	"testing"
)

func completeFinetuningFacts() (*ResolveFinetuningFactsRequest, *ResolveFinetuningFactsResponse) {
	tenant := "11111111-1111-4111-8111-111111111111"
	version := "22222222-2222-4222-8222-222222222222"
	in := &ResolveFinetuningFactsRequest{TenantId: tenant, BaseModelVersionId: version, Target: &ResolveFinetuningFactsRequest_NewModelName{NewModelName: "new-model"}}
	config := []byte(`{"model_type":"qwen2","sliding_window":null}`)
	h := sha256.Sum256(config)
	digest := hex.EncodeToString(h[:])
	out := &ResolveFinetuningFactsResponse{TenantId: tenant, ModelId: "33333333-3333-4333-8333-333333333333", ModelVersionId: version, Format: "safetensors", Bucket: "models", ObjectKey: tenant + "/model.tar", ObjectVersionId: "original-version", SizeBytes: 10000, Sha256: digest, ConfigJson: config, TokenizerConfigJson: config, Target: &ResolveFinetuningFactsResponse_NewModelName{NewModelName: "new-model"}, VerificationSource: FinetuningVerificationSource}
	for _, name := range []string{"config.json", "model.safetensors", "tokenizer.json", "tokenizer_config.json"} {
		out.Files = append(out.Files, &FinetuningArtifactFile{Path: name, SizeBytes: int64(len(config)), Sha256: digest})
	}
	return in, out
}

func TestFinetuningFactsValidateCompleteObservation(t *testing.T) {
	in, out := completeFinetuningFacts()
	if err := out.ValidateFor(in); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ResolveFinetuningFactsResponse)
	}{
		{"cross tenant", func(r *ResolveFinetuningFactsResponse) { r.TenantId = r.ModelId }},
		{"null version", func(r *ResolveFinetuningFactsResponse) { r.ObjectVersionId = "null" }},
		{"changed target", func(r *ResolveFinetuningFactsResponse) {
			r.Target = &ResolveFinetuningFactsResponse_NewModelName{NewModelName: "renamed"}
		}},
		{"foreign key", func(r *ResolveFinetuningFactsResponse) { r.ObjectKey = "other/model.tar" }},
		{"unsorted or duplicate inventory", func(r *ResolveFinetuningFactsResponse) { r.Files[1].Path = r.Files[0].Path }},
		{"changed config bytes", func(r *ResolveFinetuningFactsResponse) { r.ConfigJson = []byte(`{"model_type":"other"}`) }},
		{"unknown fields", func(r *ResolveFinetuningFactsResponse) { r.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := proto.Clone(out).(*ResolveFinetuningFactsResponse)
			test.mutate(next)
			if err := next.ValidateFor(in); err == nil {
				t.Fatal("changed provider facts accepted")
			}
		})
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"nested":{"x":1,"x":2}}`, `[]`, `null`, `{"a":1} {"b":2}`} {
		if factsJSONObject([]byte(raw)) {
			t.Fatalf("ambiguous config accepted: %s", raw)
		}
	}
}

func TestFinetuningFactsWithoutRegistrationTargetStillValidatesSource(t *testing.T) {
	in, out := completeFinetuningFacts()
	in.Target, out.Target = nil, nil
	if err := out.ValidateFor(in); err != nil {
		t.Fatalf("verified source without a registration target: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*ResolveFinetuningFactsRequest, *ResolveFinetuningFactsResponse)
	}{
		{"unexpected response target", func(_ *ResolveFinetuningFactsRequest, r *ResolveFinetuningFactsResponse) {
			r.Target = &ResolveFinetuningFactsResponse_NewModelName{NewModelName: "unrequested"}
		}},
		{"typed empty target", func(q *ResolveFinetuningFactsRequest, _ *ResolveFinetuningFactsResponse) {
			q.Target = &ResolveFinetuningFactsRequest_NewModelName{}
		}},
		{"foreign tenant", func(_ *ResolveFinetuningFactsRequest, r *ResolveFinetuningFactsResponse) { r.TenantId = r.ModelId }},
		{"missing weights", func(_ *ResolveFinetuningFactsRequest, r *ResolveFinetuningFactsResponse) { r.Files = r.Files[:1] }},
		{"unfixed source version", func(_ *ResolveFinetuningFactsRequest, r *ResolveFinetuningFactsResponse) { r.ObjectVersionId = "null" }},
		{"unknown request field", func(q *ResolveFinetuningFactsRequest, _ *ResolveFinetuningFactsResponse) {
			q.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := proto.Clone(in).(*ResolveFinetuningFactsRequest)
			r := proto.Clone(out).(*ResolveFinetuningFactsResponse)
			test.change(q, r)
			if r.ValidateFor(q) == nil {
				t.Fatal("registration-free request bypassed source or target validation")
			}
		})
	}
}
