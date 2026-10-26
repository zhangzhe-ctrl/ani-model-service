package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/durationpb"

	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func TestBuildAppRunsProductionComposition(t *testing.T) {
	clientTLS := useTestServerMTLS(t)
	grpcAddress := reserveAddress(t)
	adminAddress := reserveAddress(t)
	for adminAddress == grpcAddress {
		adminAddress = reserveAddress(t)
	}
	bc := &conf.Bootstrap{Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: grpcAddress, Timeout: durationpb.New(time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: adminAddress, Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(3 * time.Second),
	}}
	var logs bytes.Buffer
	app, err := buildApp(bc, newRuntimeLogger(&logs))
	if err != nil {
		t.Fatalf("buildApp() error = %v", err)
	}

	runResult := make(chan error, 1)
	go func() { runResult <- app.Run() }()
	waitForHTTP(t, "http://"+adminAddress+"/readyz")
	assertProductionAdmin(t, adminAddress)
	assertProductionGRPCHealth(t, grpcAddress, clientTLS)

	if err := app.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("production composition did not stop within its timeout")
	}
	if !strings.Contains(logs.String(), `"service.name":"ani-model-service"`) {
		t.Fatalf("structured service identity missing from logs: %s", logs.String())
	}
	if !regexp.MustCompile(`"trace_id":"[0-9a-f]{32}"`).MatchString(logs.String()) {
		t.Fatalf("request trace correlation missing from logs: %s", logs.String())
	}
	if !regexp.MustCompile(`"span_id":"[0-9a-f]{16}"`).MatchString(logs.String()) {
		t.Fatalf("request span correlation missing from logs: %s", logs.String())
	}
}

func TestBuildAppRejectsInvalidConfig(t *testing.T) {
	_, err := buildApp(&conf.Bootstrap{}, newRuntimeLogger(io.Discard))
	if err == nil {
		t.Fatal("buildApp() unexpectedly accepted an incomplete config")
	}
}

func useTestServerMTLS(t *testing.T) *tls.Config {
	t.Helper()
	caKey, caCert, caPEM := newTestCA(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey := signTestCert(t, caKey, caCert, "model.ani.internal", net.IPv4(127, 0, 0, 1), x509.ExtKeyUsageServerAuth)
	writeCertPair(t, certPath, keyPath, serverCert, serverKey)
	clientCert, clientKey := signTestCert(t, caKey, caCert, "ani-inference.ani.internal", nil, x509.ExtKeyUsageClientAuth)
	t.Setenv("ANI_MODEL_SERVER_CA_FILE", caPath)
	t.Setenv("ANI_MODEL_SERVER_CERT_FILE", certPath)
	t.Setenv("ANI_MODEL_SERVER_KEY_FILE", keyPath)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA was not added")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{clientCert.Raw}, PrivateKey: clientKey, Leaf: clientCert}},
		RootCAs:      pool,
		ServerName:   "127.0.0.1",
	}
}

func newTestCA(t *testing.T) (*rsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func signTestCert(t *testing.T, caKey *rsa.PrivateKey, caCert *x509.Certificate, dns string, ip net.IP, usage x509.ExtKeyUsage, options ...func(*x509.Certificate)) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dns},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{dns},
	}
	if ip != nil {
		template.IPAddresses = []net.IP{ip}
	}
	for _, option := range options {
		option(&template)
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func writeCertPair(t *testing.T, certPath, keyPath string, cert *x509.Certificate, key *rsa.PrivateKey) {
	t.Helper()
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return address
}

func waitForHTTP(t *testing.T, endpoint string) {
	waitForHTTPStatus(t, endpoint, http.StatusOK)
}

func waitForHTTPStatus(t *testing.T, endpoint string, wantStatus int) {
	t.Helper()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == wantStatus {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("endpoint did not return status %d: %s", wantStatus, endpoint)
}

func assertProductionAdmin(t *testing.T, address string) {
	t.Helper()
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("GET /healthz = %d %q", response.StatusCode, body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("GET /healthz content type = %q", contentType)
	}
}

func assertProductionGRPCHealth(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatalf("dial production gRPC: %v", err)
	}
	defer connection.Close()
	response, err := grpc_health_v1.NewHealthClient(connection).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("production gRPC health: %v", err)
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("production gRPC health = %s", response.Status)
	}
}
