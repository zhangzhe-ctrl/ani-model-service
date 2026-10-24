package client

import (
	"context"
	"github.com/go-kratos/kratos/v3/errors"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"time"
)

type RuntimeModel struct {
	VersionID, ModelID, ArtifactRef, ArtifactSHA256 string
}
type ArtifactDownload struct {
	URL, StoragePath string
	ExpiresAt        time.Time
}
type ModelClient struct{ api modelv1.ModelServiceClient }

func NewModelClient(api modelv1.ModelServiceClient) *ModelClient { return &ModelClient{api: api} }
func (c *ModelClient) EnsureModel(ctx context.Context, tenant, versionID string) (RuntimeModel, error) {
	r, err := c.api.GetModelVersion(ctx, &modelv1.GetModelVersionRequest{TenantId: tenant, ModelVersionId: versionID})
	if err != nil {
		return RuntimeModel{}, err
	}
	if r.GetVersion() == nil {
		return RuntimeModel{}, errors.NotFound("MODEL_VERSION_NOT_FOUND", "model version missing")
	}
	v := r.GetVersion()
	return runtimeModelFromVersion(v)
}

func runtimeModelFromVersion(v *modelv1.ModelVersion) (RuntimeModel, error) {
	if v == nil {
		return RuntimeModel{}, errors.NotFound("MODEL_VERSION_NOT_FOUND", "model version missing")
	}
	if v.GetStatus() != "ready" || v.GetStoragePath() == "" || v.GetChecksumSha256() == "" {
		return RuntimeModel{}, errors.New(412, "MODEL_NOT_READY", "model version is not deployment ready")
	}
	return RuntimeModel{VersionID: v.GetId(), ModelID: v.GetModelId(), ArtifactRef: v.GetStoragePath(), ArtifactSHA256: v.GetChecksumSha256()}, nil
}

func (c *ModelClient) GetArtifactDownloadURL(ctx context.Context, tenant, versionID, requester string) (ArtifactDownload, error) {
	r, err := c.api.GetModelDownloadURL(ctx, &modelv1.GetModelDownloadURLRequest{TenantId: tenant, ModelVersionId: versionID, Requester: requester})
	if err != nil {
		return ArtifactDownload{}, err
	}
	if r.GetDownloadUrl() == "" || r.GetExpiresAt() == nil {
		return ArtifactDownload{}, errors.New(502, "MODEL_INVALID_DOWNLOAD_URL", "model returned invalid download URL")
	}
	exp := r.GetExpiresAt().AsTime()
	if !exp.After(time.Now()) {
		return ArtifactDownload{}, errors.New(502, "MODEL_DOWNLOAD_EXPIRED", "model returned expired download URL")
	}
	return ArtifactDownload{URL: r.GetDownloadUrl(), StoragePath: r.GetStoragePath(), ExpiresAt: exp}, nil
}
