package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	inferencev1 "github.com/liangzai006/ani-inference-service/api/inference/v1"
	modelbiz "github.com/liangzai006/ani-model-service/internal/biz/model"
	inferencedata "github.com/liangzai006/ani-model-service/internal/data/inference"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// InferenceTLSConfig holds mTLS configuration for connecting to Inference Service.
type InferenceTLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

// IsConfigured returns true if all required TLS fields are set.
func (c InferenceTLSConfig) IsConfigured() bool {
	return c.CAFile != "" && c.CertFile != "" && c.KeyFile != "" && c.ServerName != ""
}

// Validate checks that the client certificate has exactly one DNS SAN and no wildcards.
func (c InferenceTLSConfig) Validate(cert *x509.Certificate) error {
	if len(cert.DNSNames) != 1 {
		return fmt.Errorf("client certificate must have exactly one DNS SAN, found %d", len(cert.DNSNames))
	}
	clientDNS := cert.DNSNames[0]
	if strings.ContainsAny(clientDNS, "*:/\\ \t\n") {
		return fmt.Errorf("client DNS SAN contains wildcard or special characters: %q", clientDNS)
	}
	return nil
}

func configuredInferenceReferences() (modelbiz.ReferenceChecker, func(), error) {
	close := func() {}
	address := strings.TrimSpace(os.Getenv("ANI_INFERENCE_GRPC_ADDR"))
	if address == "" {
		return nil, close, nil
	}

	tlsConfig := InferenceTLSConfig{
		CAFile:     strings.TrimSpace(os.Getenv("ANI_INFERENCE_CA_FILE")),
		CertFile:   strings.TrimSpace(os.Getenv("ANI_INFERENCE_CERT_FILE")),
		KeyFile:    strings.TrimSpace(os.Getenv("ANI_INFERENCE_KEY_FILE")),
		ServerName: strings.TrimSpace(os.Getenv("ANI_INFERENCE_SERVER_NAME")),
	}

	if !tlsConfig.IsConfigured() {
		return nil, close, fmt.Errorf("mTLS required when ANI_INFERENCE_GRPC_ADDR is set: missing ANI_INFERENCE_CA_FILE, ANI_INFERENCE_CERT_FILE, ANI_INFERENCE_KEY_FILE, or ANI_INFERENCE_SERVER_NAME")
	}

	cert, err := tls.LoadX509KeyPair(tlsConfig.CertFile, tlsConfig.KeyFile)
	if err != nil {
		return nil, close, fmt.Errorf("load client certificate: %w", err)
	}

	caPool := x509.NewCertPool()
	caPEM, err := os.ReadFile(tlsConfig.CAFile)
	if err != nil {
		return nil, close, fmt.Errorf("read CA file: %w", err)
	}
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, close, fmt.Errorf("failed to append CA certificates")
	}

	// Parse client certificate to validate DNS SAN
	if len(cert.Certificate) == 0 {
		return nil, close, fmt.Errorf("client certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, close, fmt.Errorf("parse client certificate: %w", err)
	}
	if err := tlsConfig.Validate(leaf); err != nil {
		return nil, close, err
	}

	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		ServerName:   tlsConfig.ServerName,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("server provided no certificates")
			}
			serverCert := state.PeerCertificates[0]
			for _, name := range serverCert.DNSNames {
				if name == tlsConfig.ServerName {
					return nil
				}
			}
			return fmt.Errorf("server certificate DNS SANs %v do not match expected ServerName %q", serverCert.DNSNames, tlsConfig.ServerName)
		},
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(config))}
	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, close, fmt.Errorf("create Inference client: %w", err)
	}
	return inferencedata.NewClient(inferencev1.NewModelReferenceServiceClient(conn), ""), func() { _ = conn.Close() }, nil
}
