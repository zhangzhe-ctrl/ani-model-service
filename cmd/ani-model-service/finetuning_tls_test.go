package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// This observer replaces only the handler behind the real TLS/principal seam.
// The separate PostgreSQL/S3 test exercises actual facts business behavior.
type finetuningPrincipalObserver struct {
	modelv1.UnimplementedModelServiceServer
	principals chan identity.Principal
}

func (s *finetuningPrincipalObserver) ResolveFinetuningFacts(ctx context.Context, in *modelv1.ResolveFinetuningFactsRequest) (*modelv1.ResolveFinetuningFactsResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	s.principals <- p
	return &modelv1.ResolveFinetuningFactsResponse{TenantId: p.TenantID}, nil
}

func (s *finetuningPrincipalObserver) GetTrainedArtifactRegistration(ctx context.Context, in *modelv1.GetTrainedArtifactRegistrationRequest) (*modelv1.GetTrainedArtifactRegistrationResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	s.principals <- p
	return &modelv1.GetTrainedArtifactRegistrationResponse{}, nil
}
func (s *finetuningPrincipalObserver) GetTrainedArtifactUploadURL(ctx context.Context, in *modelv1.GetTrainedArtifactUploadURLRequest) (*modelv1.GetTrainedArtifactUploadURLResponse, error) {
	p, err := identity.RequireTenant(ctx, in.GetTenantId())
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	s.principals <- p
	return &modelv1.GetTrainedArtifactUploadURLResponse{}, nil
}

func TestModelServerFinetuningUsesDedicatedMTLSIdentity(t *testing.T) {
	key, ca, pem := newTestCA(t)
	dir := t.TempDir()
	caFile, certFile, keyFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(caFile, pem, 0600); err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey := signTestCert(t, key, ca, "model.ani.internal", nil, x509.ExtKeyUsageServerAuth)
	writeCertPair(t, certFile, keyFile, serverCert, serverKey)
	t.Setenv("ANI_MODEL_SERVER_CA_FILE", caFile)
	t.Setenv("ANI_MODEL_SERVER_CERT_FILE", certFile)
	t.Setenv("ANI_MODEL_SERVER_KEY_FILE", keyFile)
	server, err := configuredModelGRPCServer(&modelv1.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second)}, RPCPolicyMiddleware(ModelServicePolicy()), ModelPrincipalMiddleware())
	if err != nil {
		t.Fatal(err)
	}
	observer := &finetuningPrincipalObserver{principals: make(chan identity.Principal, 1)}
	modelv1.RegisterModelServiceServer(server, observer)
	endpoint, err := server.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Start(context.Background()) }()
	defer func() {
		_ = server.Stop(context.Background())
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	valid := metadata.Pairs("x-ani-tenant-id", "tenant", "x-ani-actor", "modeldev:execution-owner", "x-ani-request-id", "request")
	for _, test := range []struct {
		name, dns, tenant string
		md                metadata.MD
		code              codes.Code
		forbiddenWrite    bool
		method            string
	}{
		{name: "dedicated identity", dns: modeldevClientIdentity, tenant: "tenant", md: valid, code: codes.OK},
		{name: "original key readonly", dns: modeldevClientIdentity, tenant: "tenant", md: valid, code: codes.OK, method: "lookup"},
		{name: "original path ticket", dns: modeldevClientIdentity, tenant: "tenant", md: valid, code: codes.OK, method: "ticket"},
		{name: "borrowed governance lookup denied", dns: governanceClientIdentity, tenant: "tenant", md: valid, code: codes.PermissionDenied, method: "lookup"},
		{name: "cross tenant", dns: modeldevClientIdentity, tenant: "other", md: valid, code: codes.PermissionDenied},
		{name: "legacy alias", dns: "ani-modeldev", tenant: "tenant", md: valid, code: codes.PermissionDenied},
		{name: "unpublished proposed alias denied", dns: "ani-modeldev.ani.internal", tenant: "tenant", md: valid, code: codes.PermissionDenied},
		{name: "borrowed governance", dns: governanceClientIdentity, tenant: "tenant", md: valid, code: codes.PermissionDenied},
		{name: "wrong actor", dns: modeldevClientIdentity, tenant: "tenant", md: metadata.Pairs("x-ani-tenant-id", "tenant", "x-ani-actor", "governance:user", "x-ani-request-id", "request"), code: codes.Unauthenticated},
		{name: "missing metadata", dns: modeldevClientIdentity, tenant: "tenant", code: codes.Unauthenticated},
		{name: "generic create denied", dns: modeldevClientIdentity, tenant: "tenant", md: valid, code: codes.PermissionDenied, forbiddenWrite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cert, private := signTestCert(t, key, ca, test.dns, nil, x509.ExtKeyUsageClientAuth)
			connection, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "model.ani.internal", Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: private}}})))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), test.md), 2*time.Second)
			defer cancel()
			client := modelv1.NewModelServiceClient(connection)
			if test.forbiddenWrite {
				_, err = client.CreateModel(ctx, &modelv1.CreateModelRequest{TenantId: test.tenant, Name: "must-not-create"})
			} else if test.method == "lookup" {
				_, err = client.GetTrainedArtifactRegistration(ctx, &modelv1.GetTrainedArtifactRegistrationRequest{TenantId: test.tenant})
			} else if test.method == "ticket" {
				_, err = client.GetTrainedArtifactUploadURL(ctx, &modelv1.GetTrainedArtifactUploadURLRequest{TenantId: test.tenant})
			} else {
				_, err = client.ResolveFinetuningFacts(ctx, &modelv1.ResolveFinetuningFactsRequest{TenantId: test.tenant})
			}
			if status.Code(err) != test.code {
				t.Fatalf("code=%v want=%v err=%v", status.Code(err), test.code, err)
			}
			if test.code == codes.OK {
				p := <-observer.principals
				if p.Workload != modeldevClientIdentity || p.Actor != "modeldev:execution-owner" {
					t.Fatalf("principal=%+v", p)
				}
			}
		})
	}
}
