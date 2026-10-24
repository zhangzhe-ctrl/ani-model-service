package client

import (
	"testing"

	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func TestRuntimeModelMappingLeavesEmptyCommandArgvEmpty(t *testing.T) {
	v, err := runtimeModelFromVersion(&modelv1.ModelVersion{Id: "v", ModelId: "m", Status: "ready", StoragePath: "tenant/m/v/model.gguf", ChecksumSha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EngineType: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if v.ArtifactRef != "tenant/m/v/model.gguf" {
		t.Fatalf("runtime model = %#v", v)
	}
}

func TestRuntimeModelMappingIgnoresStoredStartupFields(t *testing.T) {
	v, err := runtimeModelFromVersion(&modelv1.ModelVersion{Id: "v", ModelId: "m", Status: "ready", StoragePath: "object://model", ChecksumSha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", StartupCommand: "python"})
	if err != nil {
		t.Fatal(err)
	}
	if v.ArtifactRef != "object://model" || v.ModelID != "m" {
		t.Fatalf("runtime model = %#v", v)
	}
}
