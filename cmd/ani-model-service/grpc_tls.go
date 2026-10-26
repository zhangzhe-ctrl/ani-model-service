package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-kratos/kratos/v3/middleware"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"

	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

// ModelServerTLSConfig is the mandatory mTLS material for the Model gRPC server.
// Client certificates are always required; there is no plaintext or server-only mode.
type ModelServerTLSConfig struct {
	CAFile, CertFile, KeyFile string
}

func (c ModelServerTLSConfig) Validate() error {
	if strings.TrimSpace(c.CertFile) == "" || strings.TrimSpace(c.KeyFile) == "" || strings.TrimSpace(c.CAFile) == "" {
		return errors.New("mTLS requires ANI_MODEL_SERVER_CA_FILE, ANI_MODEL_SERVER_CERT_FILE, and ANI_MODEL_SERVER_KEY_FILE")
	}
	return nil
}

// configuredModelGRPCServer creates a gRPC server that always requires a client
// certificate signed by the configured CA. Callers cannot disable that check.
func configuredModelGRPCServer(c *conf.Server_GRPC, middlewares ...middleware.Middleware) (*kratosgrpc.Server, error) {
	tlsConfig := ModelServerTLSConfig{
		CAFile:   strings.TrimSpace(os.Getenv("ANI_MODEL_SERVER_CA_FILE")),
		CertFile: strings.TrimSpace(os.Getenv("ANI_MODEL_SERVER_CERT_FILE")),
		KeyFile:  strings.TrimSpace(os.Getenv("ANI_MODEL_SERVER_KEY_FILE")),
	}
	if err := tlsConfig.Validate(); err != nil {
		return nil, err
	}

	cert, err := tls.LoadX509KeyPair(tlsConfig.CertFile, tlsConfig.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Model server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(tlsConfig.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read Model server CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Model server CA bundle has no certificates")
	}

	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
				return errors.New("unverified client certificate chain")
			}
			_, err := clientDNSIdentity(verifiedChains[0][0])
			return err
		},
	}

	return kratosgrpc.NewServer(
		kratosgrpc.Network(c.Network),
		kratosgrpc.Address(c.Addr),
		kratosgrpc.Timeout(c.Timeout.AsDuration()),
		kratosgrpc.TLSConfig(config),
		kratosgrpc.Middleware(middlewares...),
		kratosgrpc.DisableReflection(),
	), nil
}

func clientDNSIdentity(cert *x509.Certificate) (string, error) {
	if cert == nil || len(cert.DNSNames) != 1 || len(cert.URIs) != 0 || len(cert.IPAddresses) != 0 || len(cert.EmailAddresses) != 0 {
		return "", errors.New("client certificate must contain exactly one DNS SAN and no URI, IP, or email SANs")
	}
	clientDNS := cert.DNSNames[0]
	if strings.TrimSpace(clientDNS) == "" || strings.ContainsAny(clientDNS, "*:/\\ \t\r\n") {
		return "", fmt.Errorf("invalid client DNS SAN: %q (wildcards not allowed)", clientDNS)
	}
	return clientDNS, nil
}
