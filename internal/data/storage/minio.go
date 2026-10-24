package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	bizstorage "github.com/liangzai006/ani-model-service/internal/biz/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOAdapter uses either a configured shared bucket or a tenant-named bucket.
// It never deletes buckets or manages MinIO/PVC lifecycle.
type MinIOAdapter struct {
	client        *minio.Client
	buckets       bucketAPI
	bucket        string
	tenantBuckets bool
}

type bucketAPI interface {
	BucketExists(context.Context, string) (bool, error)
	MakeBucket(context.Context, string, minio.MakeBucketOptions) error
}

func NewMinIOAdapter(endpoint, accessKey, secretKey, bucket string, secure bool) (*MinIOAdapter, error) {
	endpoint = strings.TrimSpace(endpoint)
	bucket = strings.TrimSpace(bucket)
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, fmt.Errorf("minio endpoint, credentials and bucket are required")
	}
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}
	return &MinIOAdapter{client: c, buckets: c, bucket: bucket}, nil
}

// NewMinIOTenantBucketAdapter stores each tenant's objects in a bucket named
// after that tenant's UUID. Buckets are checked/created lazily by EnsureBucket.
func NewMinIOTenantBucketAdapter(endpoint, accessKey, secretKey string, secure bool) (*MinIOAdapter, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("minio endpoint and credentials are required")
	}
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}
	return &MinIOAdapter{client: c, buckets: c, tenantBuckets: true}, nil
}

// CheckBucket verifies credentials and that the provisioned bucket exists.
// It is intentionally read-only; provisioning is handled outside the service.
func (a *MinIOAdapter) CheckBucket(ctx context.Context) error {
	if a.tenantBuckets {
		return fmt.Errorf("%w: tenant bucket requires tenant id", bizstorage.ErrProvider)
	}
	ok, err := a.bucketClient().BucketExists(ctx, a.bucket)
	if err != nil {
		return providerError(err)
	}
	if !ok {
		return fmt.Errorf("%w: bucket %q does not exist", bizstorage.ErrProvider, a.bucket)
	}
	return nil
}

// CheckTenantConnection verifies access without requiring the tenant bucket
// to exist. The bucket is created lazily by EnsureBucket before a download.
func (a *MinIOAdapter) CheckTenantConnection(ctx context.Context, tenantID string) error {
	bucket, err := tenantBucket(tenantID)
	if err != nil {
		return err
	}
	_, err = a.bucketClient().BucketExists(ctx, bucket)
	if err != nil {
		return providerError(err)
	}
	return nil
}

// EnsureBucket checks the tenant bucket and creates it when absent. Creation
// is idempotent when another caller wins the race.
func (a *MinIOAdapter) EnsureBucket(ctx context.Context, tenantID string) error {
	bucket, err := a.bucketForTenant(tenantID)
	if err != nil {
		return err
	}
	ok, err := a.bucketClient().BucketExists(ctx, bucket)
	if err != nil {
		return providerError(err)
	}
	if ok {
		return nil
	}
	if err := a.bucketClient().MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "BucketAlreadyExists" || resp.Code == "BucketAlreadyOwnedByYou" || resp.StatusCode == 409 {
			return nil
		}
		return providerError(err)
	}
	return nil
}

func (a *MinIOAdapter) CreateUploadURL(ctx context.Context, in bizstorage.UploadRequest) (bizstorage.SignedURL, error) {
	key, err := tenantKey(in.TenantID, in.ObjectRef)
	if err != nil {
		return bizstorage.SignedURL{}, err
	}
	expires := 15 * time.Minute
	bucket, err := a.bucketForTenant(in.TenantID)
	if err != nil {
		return bizstorage.SignedURL{}, err
	}
	u, err := a.client.PresignedPutObject(ctx, bucket, key, expires)
	if err != nil {
		return bizstorage.SignedURL{}, providerError(err)
	}
	return bizstorage.SignedURL{URL: u.String(), ExpiresAt: time.Now().Add(expires)}, nil
}

func (a *MinIOAdapter) Upload(ctx context.Context, in bizstorage.UploadRequest, body io.Reader) error {
	key, err := tenantKey(in.TenantID, in.ObjectRef)
	if err != nil {
		return err
	}
	options := minio.PutObjectOptions{ContentType: in.ContentType}
	if in.ChecksumSHA256 != "" {
		options.UserMetadata = map[string]string{"X-Amz-Meta-Sha256": in.ChecksumSHA256}
	}
	size := in.SizeBytes
	if size == 0 {
		size = -1
	}
	bucket, err := a.bucketForTenant(in.TenantID)
	if err != nil {
		return err
	}
	if _, err := a.client.PutObject(ctx, bucket, key, body, size, options); err != nil {
		return providerError(err)
	}
	return nil
}

func (a *MinIOAdapter) ObjectExists(ctx context.Context, tenant, objectRef string) (bool, error) {
	key, err := tenantKey(tenant, objectRef)
	if err != nil {
		return false, err
	}
	bucket, bucketErr := a.bucketForTenant(tenant)
	if bucketErr != nil {
		return false, bucketErr
	}
	_, err = a.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.StatusCode == 404 || resp.Code == "NoSuchKey" || resp.Code == "NoSuchObject" {
			return false, nil
		}
		return false, providerError(err)
	}
	return true, nil
}

func (a *MinIOAdapter) VerifyChecksum(ctx context.Context, tenant, objectRef, expected string) error {
	key, err := tenantKey(tenant, objectRef)
	if err != nil {
		return err
	}
	if len(expected) != sha256.Size*2 {
		return bizstorage.ErrChecksumMismatch
	}
	bucket, bucketErr := a.bucketForTenant(tenant)
	if bucketErr != nil {
		return bucketErr
	}
	obj, err := a.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return providerError(err)
	}
	defer obj.Close()
	h := sha256.New()
	if _, err := io.Copy(h, obj); err != nil {
		return providerError(err)
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), expected) {
		return bizstorage.ErrChecksumMismatch
	}
	return nil
}

func (a *MinIOAdapter) VerifyObject(ctx context.Context, tenant, objectRef string, expectedSize int64, expected string) (bizstorage.ObjectVerification, error) {
	key, err := tenantKey(tenant, objectRef)
	if err != nil {
		return bizstorage.ObjectVerification{}, err
	}
	if expectedSize <= 0 || len(expected) != sha256.Size*2 {
		return bizstorage.ObjectVerification{}, bizstorage.ErrChecksumMismatch
	}
	bucket, err := a.bucketForTenant(tenant)
	if err != nil {
		return bizstorage.ObjectVerification{}, err
	}
	stat, err := a.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return bizstorage.ObjectVerification{}, providerError(err)
	}
	obj, err := a.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return bizstorage.ObjectVerification{}, providerError(err)
	}
	defer obj.Close()
	verification, err := bizstorage.VerifyReaderWithSize(obj, expectedSize, expected)
	if err != nil {
		return bizstorage.ObjectVerification{}, providerError(err)
	}
	// StatObject catches metadata-level size changes before the streaming read;
	// the streamed count remains authoritative for the bytes actually verified.
	if stat.Size != verification.SizeBytes {
		verification.Matches = false
	}
	return verification, nil
}

func (a *MinIOAdapter) CreateDownloadURL(ctx context.Context, in bizstorage.DownloadRequest) (bizstorage.SignedURL, error) {
	key, err := tenantKey(in.TenantID, in.ObjectRef)
	if err != nil {
		return bizstorage.SignedURL{}, err
	}
	expires := in.TTL
	if expires <= 0 || expires > 30*time.Minute {
		expires = 5 * time.Minute
	}
	bucket, err := a.bucketForTenant(in.TenantID)
	if err != nil {
		return bizstorage.SignedURL{}, err
	}
	u, err := a.client.PresignedGetObject(ctx, bucket, key, expires, url.Values{})
	if err != nil {
		return bizstorage.SignedURL{}, providerError(err)
	}
	return bizstorage.SignedURL{URL: u.String(), ExpiresAt: time.Now().Add(expires)}, nil
}

func (a *MinIOAdapter) bucketClient() bucketAPI {
	if a.buckets != nil {
		return a.buckets
	}
	return a.client
}

func (a *MinIOAdapter) bucketForTenant(tenantID string) (string, error) {
	if a.tenantBuckets {
		return tenantBucket(tenantID)
	}
	if a.bucket == "" {
		return "", fmt.Errorf("minio bucket is not configured")
	}
	return a.bucket, nil
}

var errInvalidTenantBucket = errors.New("invalid tenant bucket")

func tenantBucket(tenantID string) (string, error) {
	tenantID = strings.TrimSpace(tenantID)
	if _, err := uuid.Parse(tenantID); err != nil {
		return "", fmt.Errorf("%w: %q", errInvalidTenantBucket, tenantID)
	}
	return strings.ToLower(tenantID), nil
}

func tenantKey(tenant, objectRef string) (string, error) {
	tenant = strings.Trim(strings.TrimSpace(tenant), "/")
	key := strings.Trim(strings.TrimSpace(objectRef), "/")

	// SECURITY: Prevent path traversal attacks
	if tenant == "" || key == "" {
		return "", fmt.Errorf("invalid tenant/object reference: empty value")
	}
	if strings.Contains(tenant, "/") {
		return "", fmt.Errorf("invalid tenant/object reference: tenant contains slash")
	}
	// Check for path traversal patterns
	if strings.Contains(key, "..") || strings.Contains(key, "./") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid tenant/object reference: path traversal detected")
	}

	if first := strings.SplitN(key, "/", 2)[0]; first != tenant {
		if _, tenantErr := uuid.Parse(tenant); tenantErr == nil {
			if _, prefixErr := uuid.Parse(first); prefixErr == nil {
				return "", fmt.Errorf("object reference belongs to another tenant")
			}
		}
	}
	if key == tenant || strings.HasPrefix(key, tenant+"/") {
		return key, nil
	}
	return tenant + "/" + key, nil
}
