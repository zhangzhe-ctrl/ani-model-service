package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	bizstorage "github.com/liangzai006/ani-model-service/internal/biz/storage"
	storagev1 "github.com/zhangzhe-ctrl/ani-model-service/api/storage/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GRPCAdapter implements the Model storage port against the versioned
// external StorageService contract. It owns no Storage infrastructure.
type GRPCAdapter struct {
	client storagev1.StorageServiceClient
	http   *http.Client
}

func DialGRPC(ctx context.Context, address string, opts ...grpc.DialOption) (*GRPCAdapter, *grpc.ClientConn, error) {
	conn, err := grpc.DialContext(ctx, address, opts...)
	if err != nil {
		return nil, nil, providerError(err)
	}
	return NewGRPCAdapter(conn), conn, nil
}

func NewGRPCAdapter(conn grpc.ClientConnInterface) *GRPCAdapter {
	return &GRPCAdapter{client: storagev1.NewStorageServiceClient(conn), http: http.DefaultClient}
}

// Upload obtains a short-lived Storage URL over gRPC and streams bytes to it.
// The Model service never creates or manages the destination object itself.
func (a *GRPCAdapter) Upload(ctx context.Context, in bizstorage.UploadRequest, body io.Reader) error {
	u, err := a.CreateUploadURL(ctx, in)
	if err != nil {
		return err
	}
	if err := bizstorage.ValidateSignedURL(u, time.Now()); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.URL, body)
	if err != nil {
		return fmt.Errorf("create storage upload request: %w", err)
	}
	if in.SizeBytes > 0 {
		req.ContentLength = in.SizeBytes
	}
	if in.ChecksumSHA256 != "" {
		req.Header.Set("X-Checksum-Sha256", in.ChecksumSHA256)
	}
	client := a.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return providerError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: upload returned status %d", bizstorage.ErrProvider, resp.StatusCode)
	}
	return nil
}

func (a *GRPCAdapter) CreateUploadURL(ctx context.Context, in bizstorage.UploadRequest) (bizstorage.SignedURL, error) {
	r, err := a.client.CreateUploadURL(ctx, &storagev1.CreateUploadURLRequest{TenantId: in.TenantID, ObjectRef: in.ObjectRef, ContentType: in.ContentType, SizeBytes: in.SizeBytes, ChecksumSha256: in.ChecksumSHA256})
	if err != nil {
		return bizstorage.SignedURL{}, providerError(err)
	}
	return signedURL(r.GetUrl(), r.GetExpiresAt()), nil
}

func (a *GRPCAdapter) ObjectExists(ctx context.Context, tenant, objectRef string) (bool, error) {
	r, err := a.client.ObjectExists(ctx, &storagev1.ObjectExistsRequest{TenantId: tenant, ObjectRef: objectRef})
	if err != nil {
		return false, providerError(err)
	}
	return r.GetExists(), nil
}

func (a *GRPCAdapter) VerifyChecksum(ctx context.Context, tenant, objectRef, checksum string) error {
	r, err := a.client.VerifyChecksum(ctx, &storagev1.VerifyChecksumRequest{TenantId: tenant, ObjectRef: objectRef, ChecksumSha256: checksum})
	if err != nil {
		return providerError(err)
	}
	if !r.GetMatches() {
		return bizstorage.ErrChecksumMismatch
	}
	return nil
}

func (a *GRPCAdapter) VerifyObject(ctx context.Context, tenant, objectRef string, expectedSize int64, expected string) (bizstorage.ObjectVerification, error) {
	u, err := a.CreateDownloadURL(ctx, bizstorage.DownloadRequest{TenantID: tenant, ObjectRef: objectRef, Requester: "model-registration", TTL: 10 * time.Minute})
	if err != nil {
		return bizstorage.ObjectVerification{}, err
	}
	if err := bizstorage.ValidateSignedURL(u, time.Now()); err != nil {
		return bizstorage.ObjectVerification{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL, nil)
	if err != nil {
		return bizstorage.ObjectVerification{}, fmt.Errorf("create storage download request: %w", err)
	}
	client := a.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return bizstorage.ObjectVerification{}, providerError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return bizstorage.ObjectVerification{}, fmt.Errorf("%w: download returned status %d", bizstorage.ErrProvider, resp.StatusCode)
	}
	verification, err := bizstorage.VerifyReaderWithSize(resp.Body, expectedSize, expected)
	if err != nil {
		return bizstorage.ObjectVerification{}, providerError(err)
	}
	return verification, nil
}

func (a *GRPCAdapter) CreateDownloadURL(ctx context.Context, in bizstorage.DownloadRequest) (bizstorage.SignedURL, error) {
	r, err := a.client.CreateDownloadURL(ctx, &storagev1.CreateDownloadURLRequest{TenantId: in.TenantID, ObjectRef: in.ObjectRef, Requester: in.Requester, Ttl: durationpb.New(in.TTL)})
	if err != nil {
		return bizstorage.SignedURL{}, providerError(err)
	}
	return signedURL(r.GetUrl(), r.GetExpiresAt()), nil
}

func signedURL(url string, expiresAt *timestamppb.Timestamp) bizstorage.SignedURL {
	if expiresAt == nil {
		return bizstorage.SignedURL{URL: url}
	}
	return bizstorage.SignedURL{URL: url, ExpiresAt: expiresAt.AsTime()}
}

func providerError(err error) error { return fmt.Errorf("%w: %v", bizstorage.ErrProvider, err) }
