package main

import (
	"context"
	"testing"

	"github.com/liangzai006/ani-model-service/internal/identity"
	"google.golang.org/grpc/metadata"
)

func TestModelServicePolicyAllowsGovernanceModelLifecycle(t *testing.T) {
	policy := ModelServicePolicy()
	methods := []string{
		"/model.v1.ModelService/CreateModel",
		"/model.v1.ModelService/GetModel",
		"/model.v1.ModelService/ListModels",
		"/model.v1.ModelService/DeleteModel",
		"/model.v1.ModelService/CreateModelVersion",
		"/model.v1.ModelService/GetModelVersion",
		"/model.v1.ModelService/ListModelVersions",
		"/model.v1.ModelService/ListModelOperations",
		"/model.v1.ModelService/DeleteModelVersion",
		"/model.v1.ModelService/GetUploadURL",
		"/model.v1.ModelService/ImportModel",
		"/model.v1.ModelService/ImportModelBundle",
		"/model.v1.ModelService/GetImportTask",
		"/model.v1.ModelService/RetryImportTask",
		"/model.v1.ModelService/GetModelDownloadURL",
		"/model.v1.ModelService/SearchExternalModels",
		"/model.v1.ModelService/PrepareTrainedArtifact",
		"/model.v1.ModelService/CompleteTrainedArtifact",
		"/model.v1.ModelService/AbortTrainedArtifact",
	}
	for _, method := range methods {
		if !policy.IsAllowed("ani-governance", method) {
			t.Errorf("governance is not allowed to call %s", method)
		}
		if policy.IsAllowed("ani-governance.ani.internal", method) {
			t.Errorf("legacy governance identity is allowed to call %s", method)
		}
	}
	if policy.IsAllowed("ani-governance", "/model.v1.ModelService/Unknown") {
		t.Fatal("unknown RPC unexpectedly authorized")
	}
	if policy.IsAllowed("ani-inference.ani.internal", "/model.v1.ModelService/ImportModel") {
		t.Fatal("inference unexpectedly received model import permission")
	}
	if policy.IsAllowed("ani-inference.ani.internal", "/model.v1.ModelService/SearchExternalModels") {
		t.Fatal("inference unexpectedly received external model search permission")
	}
}

func TestModelDevHasOnlyFinetuningFactsAndOriginalRegistrationMethods(t *testing.T) {
	policy := ModelServicePolicy()
	for _, method := range []string{"ResolveFinetuningFacts", "GetTrainedArtifactRegistration", "GetTrainedArtifactUploadURL", "PrepareTrainedArtifact", "CompleteTrainedArtifact", "AbortTrainedArtifact"} {
		if !policy.IsAllowed(modeldevClientIdentity, "/model.v1.ModelService/"+method) {
			t.Fatalf("missing ModelDev method %s", method)
		}
	}
	for _, method := range []string{"CreateModel", "CreateModelVersion", "DeleteModel", "DeleteModelVersion", "ImportModel", "GetUploadURL", "GetModelDownloadURL", "ListModels", "Unknown"} {
		if policy.IsAllowed(modeldevClientIdentity, "/model.v1.ModelService/"+method) {
			t.Fatalf("unexpected ModelDev permission %s", method)
		}
	}
	for _, caller := range []string{"ani-modeldev", "ani-modeldev.ani.internal", "ani-governance.ani.internal", "*.ani.internal"} {
		if policy.IsAllowed(caller, "/model.v1.ModelService/ResolveFinetuningFacts") {
			t.Fatal("alias caller allowed")
		}
	}
}

func TestModelDevPrincipalRequiresItsOwnAuditMetadata(t *testing.T) {
	valid := metadata.Pairs("x-ani-tenant-id", "tenant", "x-ani-actor", "modeldev:execution-owner", "x-ani-request-id", "request")
	p, err := modeldevPrincipal(metadata.NewIncomingContext(context.Background(), valid))
	if err != nil || p.Workload != modeldevClientIdentity || p.TenantID != "tenant" {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	for _, md := range []metadata.MD{metadata.Pairs("x-ani-tenant-id", "tenant", "x-ani-actor", "governance:owner", "x-ani-request-id", "request"), metadata.Pairs("x-ani-tenant-id", "tenant", "x-ani-actor", "modeldev:owner"), metadata.Pairs("x-ani-tenant-id", "tenant", "x-ani-tenant-id", "other", "x-ani-actor", "modeldev:owner", "x-ani-request-id", "request")} {
		if _, err := modeldevPrincipal(metadata.NewIncomingContext(context.Background(), md)); err == nil {
			t.Fatal("invalid ModelDev metadata accepted")
		}
	}
}

func TestGovernancePrincipalUsesVerifiedMetadata(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-ani-tenant-id", "tenant-a",
		"x-ani-actor", "governance:user:7",
		"x-ani-request-id", "request-1",
	))

	principal, err := governancePrincipal(ctx)
	if err != nil {
		t.Fatalf("governancePrincipal() error = %v", err)
	}
	want := identity.Principal{
		TenantID:  "tenant-a",
		Actor:     "governance:user:7",
		Workload:  "ani-governance",
		RequestID: "request-1",
	}
	if principal.TenantID != want.TenantID || principal.Actor != want.Actor || principal.Workload != want.Workload || principal.RequestID != want.RequestID {
		t.Fatalf("principal = %+v, want %+v", principal, want)
	}
}

func TestGovernancePrincipalRejectsMissingOrDuplicateMetadata(t *testing.T) {
	tests := []struct {
		name string
		md   metadata.MD
	}{
		{name: "missing tenant", md: metadata.Pairs("x-ani-actor", "governance:user:7")},
		{name: "missing actor", md: metadata.Pairs("x-ani-tenant-id", "tenant-a")},
		{name: "missing request id", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-actor", "governance:user:7")},
		{name: "non-governance actor", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-actor", "user:7")},
		{name: "duplicate tenant", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-tenant-id", "tenant-b", "x-ani-actor", "governance:user:7")},
		{name: "duplicate actor", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-actor", "governance:user:7", "x-ani-actor", "governance:user:8")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := governancePrincipal(metadata.NewIncomingContext(context.Background(), tt.md)); err == nil {
				t.Fatal("governancePrincipal() unexpectedly accepted invalid metadata")
			}
		})
	}
}

func TestInferencePrincipalRequiresConsistentTenantMetadata(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-ani-tenant-id", "tenant-a",
		"x-tenant-id", "tenant-a",
		"x-ani-request-id", "request-1",
	))
	p, err := inferencePrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "tenant-a" || p.Actor != "inference:service" || p.Workload != "ani-inference.ani.internal" || p.RequestID != "request-1" {
		t.Fatalf("unexpected inference principal: %#v", p)
	}
	conflict := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-ani-tenant-id", "tenant-a", "x-tenant-id", "tenant-b", "x-ani-request-id", "request-1",
	))
	if _, err := inferencePrincipal(conflict); err == nil {
		t.Fatal("conflicting inference tenant metadata accepted")
	}
}
