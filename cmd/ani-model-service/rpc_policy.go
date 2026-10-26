package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"
	"github.com/liangzai006/ani-model-service/internal/identity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const governanceClientIdentity = "ani-governance"
const modeldevClientIdentity = "ani-modeldev-service"

// RPCPolicyMiddleware enforces method-level authorization based on client certificate identity.
// This implements deny-by-default: only explicitly allowed (client identity, RPC method) pairs are permitted.
func RPCPolicyMiddleware(policy RPCPolicy) middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (interface{}, error) {
			tr, ok := transport.FromServerContext(ctx)
			if !ok {
				return nil, status.Error(codes.Internal, "transport context unavailable")
			}
			method := tr.Operation()

			clientIdentity, err := extractClientIdentity(ctx)
			if err != nil {
				return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
			}
			// gRPC health is not a business method. The certificate is already
			// required by the server TLS config; kubelet uses HTTP probes instead.
			if method == "/grpc.health.v1.Health/Check" || method == "/grpc.health.v1.Health/Watch" {
				return next(ctx, req)
			}

			if !policy.IsAllowed(clientIdentity, method) {
				return nil, status.Errorf(codes.PermissionDenied,
					"client %q not authorized for method %q", clientIdentity, method)
			}

			return next(ctx, req)
		}
	}
}

// extractClientIdentity retrieves the DNS SAN from the client's mTLS certificate.
func extractClientIdentity(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("no peer info")
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", fmt.Errorf("not a TLS connection")
	}
	if tlsInfo.State.Version < tls.VersionTLS13 {
		return "", fmt.Errorf("TLS 1.3 is required")
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("unverified client certificate chain")
	}

	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", fmt.Errorf("no client certificate")
	}

	return clientDNSIdentity(tlsInfo.State.VerifiedChains[0][0])
}

// RPCPolicy defines which client identities can call which RPC methods.
type RPCPolicy interface {
	IsAllowed(clientIdentity, method string) bool
}

// StaticRPCPolicy is a simple map-based policy implementation.
type StaticRPCPolicy map[string][]string

func (p StaticRPCPolicy) IsAllowed(clientIdentity, method string) bool {
	allowedMethods, ok := p[clientIdentity]
	if !ok {
		return false
	}
	for _, m := range allowedMethods {
		if m == method {
			return true
		}
	}
	return false
}

// ModelServicePolicy defines the RPC authorization policy for Model Service.
// Inference receives read-only model queries; Governance receives the
// tenant-scoped management and import lifecycle methods.
func ModelServicePolicy() RPCPolicy {
	policy := StaticRPCPolicy{
		"ani-inference.ani.internal": {
			"/model.v1.ModelService/GetModel",
			"/model.v1.ModelService/GetModelVersion",
			"/model.v1.ModelService/GetModelDownloadURL",
		},
		modeldevClientIdentity: {
			"/model.v1.ModelService/ResolveFinetuningFacts",
			"/model.v1.ModelService/GetTrainedArtifactRegistration",
			"/model.v1.ModelService/GetTrainedArtifactUploadURL",
			"/model.v1.ModelService/PrepareTrainedArtifact",
			"/model.v1.ModelService/CompleteTrainedArtifact",
			"/model.v1.ModelService/AbortTrainedArtifact",
		},
		governanceClientIdentity: {
			"/model.v1.ModelService/CreateModel",
			"/model.v1.ModelService/GetModel",
			"/model.v1.ModelService/ListModels",
			"/model.v1.ModelService/DeleteModel",
			"/model.v1.ModelService/GetModelVersion",
			"/model.v1.ModelService/ListModelVersions",
			"/model.v1.ModelService/ListModelOperations",
			"/model.v1.ModelService/DeleteModelVersion",
			"/model.v1.ModelService/CreateModelVersion",
			"/model.v1.ModelService/GetUploadURL",
			"/model.v1.ModelService/ImportModel",
			"/model.v1.ModelService/ImportModelBundle",
			"/model.v1.ModelService/GetImportTask",
			"/model.v1.ModelService/GetImportTaskLogs",
			"/model.v1.ModelService/RetryImportTask",
			"/model.v1.ModelService/GetModelDownloadURL",
			"/model.v1.ModelService/SearchExternalModels",
			"/model.v1.ModelService/PrepareTrainedArtifact",
			"/model.v1.ModelService/CompleteTrainedArtifact",
			"/model.v1.ModelService/AbortTrainedArtifact",
		},
	}

	return &ValidatedPolicy{
		underlying: policy,
		expectedIdentities: map[string]string{
			"ani-inference.ani.internal": "Inference Service",
			governanceClientIdentity:     "Governance Service",
			modeldevClientIdentity:       "ModelDev Service",
		},
	}
}

// ModelPrincipalMiddleware binds the tenant and actor asserted by Governance
// after RPCPolicyMiddleware has authenticated the caller's mTLS certificate.
// Request bodies still carry tenant_id for API compatibility, but handlers
// compare them against this trusted context and never use a mismatched value.
func ModelPrincipalMiddleware() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (interface{}, error) {
			if tr, ok := transport.FromServerContext(ctx); ok {
				// Health is intentionally metadata-free. RPCPolicyMiddleware has
				// already verified the client certificate before reaching here.
				if method := tr.Operation(); method == "/grpc.health.v1.Health/Check" || method == "/grpc.health.v1.Health/Watch" {
					return next(ctx, req)
				}
			}
			clientIdentity, err := extractClientIdentity(ctx)
			if err != nil {
				return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
			}
			if clientIdentity != governanceClientIdentity {
				if clientIdentity == modeldevClientIdentity {
					principal, err := modeldevPrincipal(ctx)
					if err != nil {
						return nil, status.Errorf(codes.Unauthenticated, "modeldev identity unavailable: %v", err)
					}
					return next(identity.WithPrincipal(ctx, principal), req)
				}
				if clientIdentity == "ani-inference.ani.internal" {
					principal, err := inferencePrincipal(ctx)
					if err != nil {
						return nil, status.Errorf(codes.Unauthenticated, "inference identity unavailable: %v", err)
					}
					return next(identity.WithPrincipal(ctx, principal), req)
				}
				return next(ctx, req)
			}
			principal, err := governancePrincipal(ctx)
			if err != nil {
				return nil, status.Errorf(codes.Unauthenticated, "governance identity unavailable: %v", err)
			}
			return next(identity.WithPrincipal(ctx, principal), req)
		}
	}
}

func modeldevPrincipal(ctx context.Context) (identity.Principal, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return identity.Principal{}, fmt.Errorf("incoming metadata unavailable")
	}
	tenant, err := singleMetadataValue(md, "x-ani-tenant-id", true)
	if err != nil {
		return identity.Principal{}, err
	}
	actor, err := singleMetadataValue(md, "x-ani-actor", true)
	if err != nil {
		return identity.Principal{}, err
	}
	if !strings.HasPrefix(actor, "modeldev:") {
		return identity.Principal{}, fmt.Errorf("modeldev audit actor required")
	}
	request, err := singleMetadataValue(md, "x-ani-request-id", true)
	if err != nil {
		return identity.Principal{}, err
	}
	return identity.Principal{TenantID: tenant, Actor: actor, Workload: modeldevClientIdentity, RequestID: request}, nil
}

func inferencePrincipal(ctx context.Context) (identity.Principal, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return identity.Principal{}, fmt.Errorf("incoming metadata unavailable")
	}
	tenant, err := singleConsistentMetadataValue(md, "x-ani-tenant-id", "x-tenant-id")
	if err != nil {
		return identity.Principal{}, err
	}
	requestID, err := singleMetadataValue(md, "x-ani-request-id", true)
	if err != nil {
		return identity.Principal{}, err
	}
	return identity.Principal{
		TenantID:  tenant,
		Actor:     "inference:service",
		Workload:  "ani-inference.ani.internal",
		RequestID: requestID,
	}, nil
}

func governancePrincipal(ctx context.Context) (identity.Principal, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return identity.Principal{}, fmt.Errorf("incoming metadata unavailable")
	}
	tenant, err := singleMetadataValue(md, "x-ani-tenant-id", true)
	if err != nil {
		return identity.Principal{}, err
	}
	actor, err := singleMetadataValue(md, "x-ani-actor", true)
	if err != nil {
		return identity.Principal{}, err
	}
	if !strings.HasPrefix(actor, "governance:") {
		return identity.Principal{}, fmt.Errorf("metadata %q must identify a governance actor", "x-ani-actor")
	}
	requestID, err := singleMetadataValue(md, "x-ani-request-id", true)
	if err != nil {
		return identity.Principal{}, err
	}
	return identity.Principal{
		TenantID:  tenant,
		Actor:     actor,
		Workload:  governanceClientIdentity,
		RequestID: requestID,
	}, nil
}

func singleMetadataValue(md metadata.MD, key string, required bool) (string, error) {
	values := md.Get(key)
	if len(values) == 0 {
		if required {
			return "", fmt.Errorf("metadata %q is required", key)
		}
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("metadata %q must contain exactly one value", key)
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", fmt.Errorf("metadata %q must not be empty", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("metadata %q contains a newline", key)
	}
	return value, nil
}

func singleConsistentMetadataValue(md metadata.MD, keys ...string) (string, error) {
	var value string
	for _, key := range keys {
		candidate, err := singleMetadataValue(md, key, false)
		if err != nil {
			return "", err
		}
		if candidate == "" {
			continue
		}
		if value == "" {
			value = candidate
			continue
		}
		if value != candidate {
			return "", fmt.Errorf("metadata %q and %q conflict", keys[0], key)
		}
	}
	if value == "" {
		return "", fmt.Errorf("metadata %q is required", keys[0])
	}
	return value, nil
}

// ValidatedPolicy wraps a policy with explicit service identity validation.
type ValidatedPolicy struct {
	underlying         RPCPolicy
	expectedIdentities map[string]string
}

func (v *ValidatedPolicy) IsAllowed(clientIdentity, method string) bool {
	if _, expected := v.expectedIdentities[clientIdentity]; !expected {
		return false
	}
	return v.underlying.IsAllowed(clientIdentity, method)
}
