package model

import (
	"context"
	"errors"
	"strings"
)

var ErrFinetuningFactsUnavailable = errors.New("finetuning facts unavailable")
var ErrFinetuningTargetConflict = errors.New("finetuning target name already exists")

// ArtifactInventory is measured from one exact non-null object version. The
// hashes describe actual bytes, rather than an ETag or a caller assertion.
type ArtifactInventory struct {
	Bucket, Key, VersionID, SHA256  string
	SizeBytes                       int64
	Files                           []ArtifactFile
	ConfigJSON, TokenizerConfigJSON []byte
}
type ArtifactFile struct {
	Path      string
	SizeBytes int64
	SHA256    string
}
type FinetuningArtifactReader interface {
	InspectFinetuningArtifact(context.Context, string, string, int64, string) (ArtifactInventory, error)
}
type ModelNameReader interface {
	ModelNameExists(context.Context, string, string) (bool, error)
}
type FinetuningFacts struct {
	Version                     Version
	Artifact                    ArtifactInventory
	TargetModelID, NewModelName string
}

// ResolveFinetuningFacts applies the same tenant ownership and target rules as
// trained-artifact preparation. Model has no separate approval registry: READY
// and the tenant-owned catalogue are the existing source of those semantics.
func ResolveFinetuningFacts(ctx context.Context, tenant, versionID, targetID, newName string, versions VersionReader, catalog Catalog, contents FinetuningArtifactReader) (FinetuningFacts, error) {
	if versions == nil || catalog == nil || contents == nil || tenant == "" || versionID == "" || (targetID != "" && newName != "") {
		return FinetuningFacts{}, ErrFinetuningFactsUnavailable
	}
	v, err := versions.GetVersion(ctx, tenant, versionID)
	if err != nil {
		return FinetuningFacts{}, err
	}
	if v.TenantID != tenant || v.ID != versionID || v.DeploymentInput() != nil || v.Format != "safetensors" || v.IsEncrypted || v.SizeBytes <= 0 {
		return FinetuningFacts{}, ErrModelNotReady
	}
	owner, err := catalog.GetModel(ctx, tenant, v.ModelID)
	if err != nil {
		return FinetuningFacts{}, err
	}
	if owner.TenantID != tenant || owner.ID != v.ModelID || owner.Status == "deleted" {
		return FinetuningFacts{}, ErrFinetuningFactsUnavailable
	}
	if targetID != "" {
		target, err := catalog.GetModel(ctx, tenant, targetID)
		if err != nil {
			return FinetuningFacts{}, err
		}
		if target.TenantID != tenant || target.ID != targetID || target.Status == "deleted" {
			return FinetuningFacts{}, ErrFinetuningFactsUnavailable
		}
	} else if newName != "" {
		if ValidateModelName(newName) != nil {
			return FinetuningFacts{}, ErrInvalidModel
		}
		names, ok := catalog.(ModelNameReader)
		if !ok {
			return FinetuningFacts{}, ErrFinetuningFactsUnavailable
		}
		exists, err := names.ModelNameExists(ctx, tenant, newName)
		if err != nil {
			return FinetuningFacts{}, err
		}
		if exists {
			return FinetuningFacts{}, ErrFinetuningTargetConflict
		}
	}
	inventory, err := contents.InspectFinetuningArtifact(ctx, tenant, v.ArtifactReference, v.SizeBytes, strings.ToLower(v.ArtifactSHA256))
	if err != nil {
		return FinetuningFacts{}, err
	}
	return FinetuningFacts{Version: v, Artifact: inventory, TargetModelID: targetID, NewModelName: newName}, nil
}
