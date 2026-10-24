package inference

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	inferencev1 "github.com/liangzai006/ani-inference-service/api/inference/v1"
	"google.golang.org/grpc/metadata"
)

type Client struct {
	api         inferencev1.ModelReferenceServiceClient
	tenantScope string
}

func NewClient(api inferencev1.ModelReferenceServiceClient, tenantScope string) *Client {
	return &Client{api: api, tenantScope: tenantScope}
}

func (c *Client) HasActiveVersionReferences(ctx context.Context, tenant string, versions []string) (bool, error) {
	if c == nil || c.api == nil {
		return false, errors.New("inference reference client unavailable")
	}
	if tenant == "" || (c.tenantScope != "" && tenant != c.tenantScope) {
		return false, errors.New("inference credential tenant mismatch")
	}
	if len(versions) < 1 || len(versions) > 256 {
		return false, errors.New("expected 1 to 256 version IDs")
	}
	for _, v := range versions {
		if id, err := uuid.Parse(v); err != nil || id == uuid.Nil {
			return false, errors.New("invalid model version ID")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The mTLS receiver establishes the service identity; these values only
	// bind the already-authenticated call to its tenant and audit request.
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-ani-tenant-id", tenant,
		"x-tenant-id", tenant,
		"x-ani-actor", "model:service",
		"x-ani-request-id", uuid.NewString(),
	))
	out, err := c.api.CheckModelVersionReferences(ctx, &inferencev1.CheckModelVersionReferencesRequest{ModelVersionIds: versions})
	if err != nil {
		return false, err
	}
	if out == nil {
		return false, errors.New("empty inference reference response")
	}
	return out.HasActiveReferences, nil
}
