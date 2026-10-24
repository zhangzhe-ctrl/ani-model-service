package model

import (
	"context"
	"time"
)

// ListOptions keeps the stable (created_at, id) descending order and supports
// direct page offsets. Before* remains for legacy cursor callers.
type ListOptions struct {
	Status, Keyword, Source, Capability string
	Limit                               int32
	Offset                              int64
	BeforeCreatedAt                     time.Time
	BeforeID                            string
}

type Record struct {
	TenantID, ID, ExternalModelID, Name, DisplayName, Description, Source, Status string
	Capabilities                                                                  []byte
	TotalSizeBytes                                                                int64
	IdempotencyKey                                                                string
	SourceRepoID, ErrorMessage                                                    string
	CreatedAt, UpdatedAt                                                          time.Time
	LatestVersion                                                                 *Version
}

type Catalog interface {
	CreateModel(context.Context, string, string, string, string, string, string, []byte, string) (Record, error)
	GetModel(context.Context, string, string) (Record, error)
	ListModels(context.Context, string, ListOptions) ([]Record, error)
	CountModels(context.Context, string, ListOptions) (int64, error)
}

type PagedCatalog interface {
	ListModelsPage(context.Context, string, ListOptions) ([]Record, int64, error)
}

type ExternalModelCatalog interface {
	GetModelByExternalID(context.Context, string, string) (Record, error)
}

type VersionReader interface {
	GetVersion(context.Context, string, string) (Version, error)
}

type VersionReferenceReader interface {
	GetVersionByExternalRef(context.Context, string, string, string) (Version, error)
}

type VersionStateStore interface {
	MarkReady(context.Context, string, string) error
	MarkError(context.Context, string, string, string) error
}

type VersionChecksumStore interface {
	SetChecksum(context.Context, string, string, string, int64) error
}

type VersionCatalog interface {
	VersionReader
	CreateVersion(context.Context, Version) (Version, error)
	ListVersions(context.Context, string, string, ListOptions) ([]Version, error)
	CountModelVersions(context.Context, string, string, ListOptions) (int64, error)
}

type PagedVersionCatalog interface {
	ListVersionsPage(context.Context, string, string, ListOptions) ([]Version, int64, error)
}

type ArtifactStore interface {
	CreateArtifact(context.Context, Artifact) (Artifact, error)
	GetArtifact(context.Context, string, string) (Artifact, error)
}
