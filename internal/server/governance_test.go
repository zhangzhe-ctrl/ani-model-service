package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type catalogIdentityProbe struct {
	modelv1.UnimplementedModelServiceServer
}

func (catalogIdentityProbe) ListModels(ctx context.Context, req *modelv1.ListModelsRequest) (*modelv1.ListModelsResponse, error) {
	p, err := identity.RequireTenant(ctx, req.TenantId)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	return &modelv1.ListModelsResponse{Models: []*modelv1.Model{{Id: p.TenantID, Name: p.Actor}}}, nil
}

func TestGovernanceMTLSBoundary(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(ca)
	serial := int64(1)
	issue := func(name string, usage x509.ExtKeyUsage) (tls.Certificate, string, string) {
		t.Helper()
		serial++
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		d, e := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		kd, e := x509.MarshalECPrivateKey(k)
		if e != nil {
			t.Fatal(e)
		}
		cp, kp := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		if e = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}), 0600); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0600); e != nil {
			t.Fatal(e)
		}
		c, e := tls.LoadX509KeyPair(cp, kp)
		if e != nil {
			t.Fatal(e)
		}
		return c, cp, kp
	}
	_, cp, kp := issue("ani-model-service", x509.ExtKeyUsageServerAuth)
	good, _, _ := issue(GovernanceSAN, x509.ExtKeyUsageClientAuth)
	wrong, _, _ := issue("wrong-service", x509.ExtKeyUsageClientAuth)
	wrongUsage, _, _ := issue(GovernanceSAN, x509.ExtKeyUsageServerAuth)
	serverTLS, err := GovernanceTLS(caPath, cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.UnaryInterceptor(GovernanceUnary()))
	modelv1.RegisterModelServiceServer(srv, catalogIdentityProbe{})
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Log(err)
		}
	}()
	defer srv.Stop()
	tenant := "11111111-1111-4111-8111-111111111111"
	base := metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:7", "x-ani-request-id", "22222222-2222-4222-8222-222222222222")
	for _, tc := range []struct {
		name   string
		certs  []tls.Certificate
		mutate func(metadata.MD)
		tenant string
		method string
		want   codes.Code
	}{
		{"valid", []tls.Certificate{good}, nil, tenant, "", codes.OK},
		{"no-cert", nil, nil, tenant, "", codes.Unavailable},
		{"wrong-san", []tls.Certificate{wrong}, nil, tenant, "", codes.Unavailable},
		{"wrong-usage", []tls.Certificate{wrongUsage}, nil, tenant, "", codes.Unavailable},
		{"duplicate", []tls.Certificate{good}, func(m metadata.MD) { m.Append("x-ani-tenant-id", tenant) }, tenant, "", codes.Unauthenticated},
		{"missing", []tls.Certificate{good}, func(m metadata.MD) { m.Delete("x-ani-actor") }, tenant, "", codes.Unauthenticated},
		{"actor-format", []tls.Certificate{good}, func(m metadata.MD) { m.Set("x-ani-actor", "governance:user:07") }, tenant, "", codes.Unauthenticated},
		{"tenant-mismatch", []tls.Certificate{good}, nil, "33333333-3333-4333-8333-333333333333", "", codes.PermissionDenied},
		{"wrong-method", []tls.Certificate{good}, nil, tenant, "/model.v1.ModelService/DeleteModel", codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, e := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "ani-model-service", Certificates: tc.certs})))
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			md := base.Copy()
			if tc.mutate != nil {
				tc.mutate(md)
			}
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 2*time.Second)
			defer cancel()
			method := tc.method
			if method == "" {
				method = ListModelsMethod
			}
			out := new(modelv1.ListModelsResponse)
			e = conn.Invoke(ctx, method, &modelv1.ListModelsRequest{TenantId: tc.tenant}, out)
			if status.Code(e) != tc.want {
				t.Fatalf("code=%s want=%s err=%v", status.Code(e), tc.want, e)
			}
			if e == nil && (len(out.Models) != 1 || out.Models[0].Id != tenant || out.Models[0].Name != "governance:user:7") {
				t.Fatalf("principal lost: %v", out)
			}
		})
	}
	if _, e := GovernanceTLS("", cp, kp); e == nil {
		t.Fatal("missing CA accepted")
	}
}

func TestCatalogReadinessLiveProbe(t *testing.T) {
	healthy := true
	r := NewCatalogReadiness(func(context.Context) error {
		if healthy {
			return nil
		}
		return context.DeadlineExceeded
	})
	if r.Ready() {
		t.Fatal("ready before start")
	}
	r.Set(true)
	if !r.Ready() {
		t.Fatal("catalog requires unused storage/worker")
	}
	healthy = false
	if r.Ready() {
		t.Fatal("database failure stayed ready")
	}
	healthy = true
	if !r.Ready() {
		t.Fatal("recovery did not restore readiness")
	}
	r.Set(false)
	if r.Ready() {
		t.Fatal("ready after stop")
	}
	missing := NewCatalogReadiness(nil)
	missing.Set(true)
	if missing.Ready() {
		t.Fatal("nil probe accepted")
	}
}
