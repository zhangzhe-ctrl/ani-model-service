package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liangzai006/ani-model-service/internal/identity"
	"github.com/liangzai006/ani-model-service/internal/service"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The handler observes the principal accepted by the real TLS and transport
// middleware. Model business operations and persistence are outside this test.
type modelPrincipalObserver struct {
	modelv1.UnimplementedModelServiceServer
	principals chan identity.Principal
}

func (s *modelPrincipalObserver) GetModel(ctx context.Context, in *modelv1.GetModelRequest) (*modelv1.Model, error) {
	principal, err := identity.FromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	s.principals <- principal
	return &modelv1.Model{TenantId: principal.TenantID, Id: in.ModelId}, nil
}

func (s *modelPrincipalObserver) CreateModel(ctx context.Context, in *modelv1.CreateModelRequest) (*modelv1.Model, error) {
	return service.NewModelService(nil).CreateModel(ctx, in)
}

func TestModelServerGovernanceMTLSIdentity(t *testing.T) {
	caKey, caCert, caPEM := newTestCA(t)
	dir := t.TempDir()
	caPath, certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey := signTestCert(t, caKey, caCert, "model.ani.internal", nil, x509.ExtKeyUsageServerAuth)
	writeCertPair(t, certPath, keyPath, serverCert, serverKey)
	t.Setenv("ANI_MODEL_SERVER_CA_FILE", caPath)
	t.Setenv("ANI_MODEL_SERVER_CERT_FILE", certPath)
	t.Setenv("ANI_MODEL_SERVER_KEY_FILE", keyPath)
	server, err := configuredModelGRPCServer(&modelv1.Server_GRPC{
		Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second),
	}, RPCPolicyMiddleware(ModelServicePolicy()), ModelPrincipalMiddleware())
	if err != nil {
		t.Fatal(err)
	}
	observer := &modelPrincipalObserver{principals: make(chan identity.Principal, 1)}
	modelv1.RegisterModelServiceServer(server, observer)
	endpoint, err := server.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	runResult := make(chan error, 1)
	go func() { runResult <- server.Start(context.Background()) }()
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		if err := <-runResult; err != nil {
			t.Error(err)
		}
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	validMD := metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-actor", "governance:user:7", "x-ani-request-id", "request-1")
	spiffe, _ := url.Parse("spiffe://ani/ani-governance")
	tests := []struct {
		name   string
		dns    string
		option func(*x509.Certificate)
		md     metadata.MD
		code   codes.Code
	}{
		{name: "registered governance", dns: "ani-governance", md: validMD, code: codes.OK},
		{name: "legacy governance denied", dns: "ani-governance.ani.internal", md: validMD, code: codes.PermissionDenied},
		{name: "unregistered client denied", dns: "unregistered", md: validMD, code: codes.PermissionDenied},
		{name: "extra DNS denied", dns: "ani-governance", option: func(c *x509.Certificate) { c.DNSNames = append(c.DNSNames, "other") }, md: validMD, code: codes.Unavailable},
		{name: "URI SAN denied", dns: "ani-governance", option: func(c *x509.Certificate) { c.URIs = []*url.URL{spiffe} }, md: validMD, code: codes.Unavailable},
		{name: "IP SAN denied", dns: "ani-governance", option: func(c *x509.Certificate) { c.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)} }, md: validMD, code: codes.Unavailable},
		{name: "email SAN denied", dns: "ani-governance", option: func(c *x509.Certificate) { c.EmailAddresses = []string{"gov@ani.internal"} }, md: validMD, code: codes.Unavailable},
		{name: "wildcard denied", dns: "*.ani.internal", md: validMD, code: codes.Unavailable},
		{name: "CN fallback denied", dns: "ani-governance", option: func(c *x509.Certificate) { c.DNSNames = nil }, md: validMD, code: codes.Unavailable},
		{name: "missing tenant denied", dns: "ani-governance", md: metadata.Pairs("x-ani-actor", "governance:user:7", "x-ani-request-id", "request-1"), code: codes.Unauthenticated},
		{name: "conflicting tenant denied", dns: "ani-governance", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-tenant-id", "tenant-b", "x-ani-actor", "governance:user:7", "x-ani-request-id", "request-1"), code: codes.Unauthenticated},
		{name: "wrong actor denied", dns: "ani-governance", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-actor", "inference:service", "x-ani-request-id", "request-1"), code: codes.Unauthenticated},
		{name: "conflicting actor denied", dns: "ani-governance", md: metadata.Pairs("x-ani-tenant-id", "tenant-a", "x-ani-actor", "governance:user:7", "x-ani-actor", "governance:user:8", "x-ani-request-id", "request-1"), code: codes.Unauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var options []func(*x509.Certificate)
			if tt.option != nil {
				options = append(options, tt.option)
			}
			cert, key := signTestCert(t, caKey, caCert, tt.dns, nil, x509.ExtKeyUsageClientAuth, options...)
			conn, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "model.ani.internal",
				Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}},
			})))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), tt.md), 2*time.Second)
			defer cancel()
			_, err = modelv1.NewModelServiceClient(conn).GetModel(ctx, &modelv1.GetModelRequest{TenantId: "tenant-a", ModelId: "model-1"})
			if status.Code(err) != tt.code {
				t.Fatalf("GetModel status = %v, want %v; error = %v", status.Code(err), tt.code, err)
			}
			if tt.code == codes.OK {
				got := <-observer.principals
				want := identity.Principal{TenantID: "tenant-a", Actor: "governance:user:7", Workload: "ani-governance", RequestID: "request-1"}
				if got.TenantID != want.TenantID || got.Actor != want.Actor || got.Workload != want.Workload || got.RequestID != want.RequestID {
					t.Fatalf("accepted principal = %+v, want %+v", got, want)
				}
				_, err = modelv1.NewModelServiceClient(conn).CreateModel(ctx, &modelv1.CreateModelRequest{TenantId: "tenant-b", Name: "cross-tenant"})
				if status.Code(err) != codes.PermissionDenied {
					t.Fatalf("real CreateModel cross-tenant status = %v, want PermissionDenied; error = %v", status.Code(err), err)
				}
				err = conn.Invoke(ctx, "/model.v1.ModelService/Unknown", &modelv1.GetModelRequest{}, &modelv1.Model{})
				if status.Code(err) != codes.Unimplemented {
					t.Fatalf("unknown RPC status = %v, want Unimplemented; error = %v", status.Code(err), err)
				}
			} else {
				select {
				case p := <-observer.principals:
					t.Fatalf("rejected request reached handler with %+v", p)
				default:
				}
			}
		})
	}
}

func TestExtractClientIdentityRequiresVerifiedTLS(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{"ani-governance"}}
	verified := tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	for _, tt := range []struct {
		name  string
		state tls.ConnectionState
	}{
		{name: "TLS12", state: tls.ConnectionState{Version: tls.VersionTLS12, PeerCertificates: verified.PeerCertificates, VerifiedChains: verified.VerifiedChains}},
		{name: "unverified", state: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: verified.PeerCertificates}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tt.state}})
			if _, err := extractClientIdentity(ctx); err == nil {
				t.Fatal("unverified or outdated TLS identity accepted")
			}
		})
	}
}
