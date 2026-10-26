package storage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bizstorage "github.com/liangzai006/ani-model-service/internal/biz/storage"
	storagev1 "github.com/zhangzhe-ctrl/ani-model-service/api/storage/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type storageServerFake struct {
	storagev1.UnimplementedStorageServiceServer
}

type storageChecksumMismatchServer struct{ storageServerFake }

func (storageChecksumMismatchServer) VerifyChecksum(context.Context, *storagev1.VerifyChecksumRequest) (*storagev1.VerifyChecksumResponse, error) {
	return &storagev1.VerifyChecksumResponse{Matches: false, ActualSha256: "different"}, nil
}

type storageExpiredURLServer struct{ storageServerFake }

func (storageExpiredURLServer) CreateDownloadURL(context.Context, *storagev1.CreateDownloadURLRequest) (*storagev1.CreateDownloadURLResponse, error) {
	return &storagev1.CreateDownloadURLResponse{Url: "grpc://expired", ExpiresAt: timestamppb.New(time.Now().Add(-time.Minute))}, nil
}

type storageDownloadURLServer struct {
	storageServerFake
	url string
}

func (s storageDownloadURLServer) CreateDownloadURL(context.Context, *storagev1.CreateDownloadURLRequest) (*storagev1.CreateDownloadURLResponse, error) {
	return &storagev1.CreateDownloadURLResponse{Url: s.url, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}, nil
}

func (storageServerFake) CreateUploadURL(context.Context, *storagev1.CreateUploadURLRequest) (*storagev1.CreateUploadURLResponse, error) {
	return &storagev1.CreateUploadURLResponse{Url: "grpc://upload", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}, nil
}
func (storageServerFake) ObjectExists(context.Context, *storagev1.ObjectExistsRequest) (*storagev1.ObjectExistsResponse, error) {
	return &storagev1.ObjectExistsResponse{Exists: true}, nil
}
func (storageServerFake) VerifyChecksum(context.Context, *storagev1.VerifyChecksumRequest) (*storagev1.VerifyChecksumResponse, error) {
	return &storagev1.VerifyChecksumResponse{Matches: true}, nil
}
func (storageServerFake) CreateDownloadURL(context.Context, *storagev1.CreateDownloadURLRequest) (*storagev1.CreateDownloadURLResponse, error) {
	return &storagev1.CreateDownloadURLResponse{Url: "grpc://download", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}, nil
}

func TestGRPCAdapterMapsStoragePort(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	storagev1.RegisterStorageServiceServer(server, storageServerFake{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	adapter := NewGRPCAdapter(conn)
	upload, err := adapter.CreateUploadURL(context.Background(), bizstorage.UploadRequest{TenantID: "tenant", ObjectRef: "obj", SizeBytes: 1, ChecksumSHA256: "sum"})
	if err != nil || upload.URL != "grpc://upload" || !upload.ExpiresAt.After(time.Now()) {
		t.Fatalf("upload mapping: %+v err=%v", upload, err)
	}
	exists, err := adapter.ObjectExists(context.Background(), "tenant", "obj")
	if err != nil || !exists {
		t.Fatalf("exists mapping: %v err=%v", exists, err)
	}
	if err := adapter.VerifyChecksum(context.Background(), "tenant", "obj", "sum"); err != nil {
		t.Fatalf("checksum mapping: %v", err)
	}
	download, err := adapter.CreateDownloadURL(context.Background(), bizstorage.DownloadRequest{TenantID: "tenant", ObjectRef: "obj", Requester: "inference", TTL: time.Minute})
	if err != nil || download.URL != "grpc://download" {
		t.Fatalf("download mapping: %+v err=%v", download, err)
	}
}

func TestGRPCAdapterMapsChecksumMismatch(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	storagev1.RegisterStorageServiceServer(server, storageChecksumMismatchServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := NewGRPCAdapter(conn).VerifyChecksum(context.Background(), "tenant", "obj", "sum"); err != bizstorage.ErrChecksumMismatch {
		t.Fatalf("checksum error = %v, want ErrChecksumMismatch", err)
	}
}

func TestGRPCAdapterReturnsExpiryForCallerValidation(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	storagev1.RegisterStorageServiceServer(server, storageExpiredURLServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	u, err := NewGRPCAdapter(conn).CreateDownloadURL(context.Background(), bizstorage.DownloadRequest{TenantID: "tenant", ObjectRef: "obj", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := bizstorage.ValidateSignedURL(u, time.Now()); err != bizstorage.ErrURLExpired {
		t.Fatalf("expiry validation error = %v, want ErrURLExpired", err)
	}
}

func TestGRPCAdapterVerifiesDownloadedObjectSizeAndDigest(t *testing.T) {
	body := []byte("model")
	hash := sha256.Sum256(body)
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer download.Close()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	storagev1.RegisterStorageServiceServer(server, storageDownloadURLServer{url: download.URL})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	adapter := NewGRPCAdapter(conn)
	verification, err := adapter.VerifyObject(context.Background(), "tenant", "obj", int64(len(body)), fmt.Sprintf("%x", hash[:]))
	if err != nil || !verification.Matches || verification.SizeBytes != int64(len(body)) {
		t.Fatalf("verification=%+v err=%v", verification, err)
	}
	verification, err = adapter.VerifyObject(context.Background(), "tenant", "obj", int64(len(body)+1), fmt.Sprintf("%x", hash[:]))
	if err != nil || verification.Matches {
		t.Fatalf("size mismatch verification=%+v err=%v", verification, err)
	}
}
