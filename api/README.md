# Model Service API module

Module: `github.com/zhangzhe-ctrl/ani-model-service/api`

## Ownership

- `model/v1`: model/version management, import and trained artifact registration.
- `storage/v1`: object storage upload/download contracts.

`ModelReferenceService` belongs to Inference. Import its client from
`github.com/liangzai006/ani-inference-service/api/inference/v1`; do not copy that
protocol into this module. Service roots may consume each other's API modules;
the two API modules do not need to depend on each other for these RPCs.

The existing `model/v1/conf.proto` and `validate.go` also expose runtime
configuration types and validation. Their separation is a future migration,
not part of the reference-protocol ownership change.

## Consumption and releases

Consumers must pin an immutable published API version; the runtime's `go.mod`
records its selected version. Publication must precede the runtime dependency
upgrade. This API is published from the existing `zhangzhe-ctrl` fork because
the task's SSH identity cannot publish to the upstream `liangzai006` repository.
The Model runtime root module keeps its original path.

Migrate consumers from `github.com/liangzai006/ani-model-service/api` to this
module. Each executable must contain exactly one Model protobuf Go package:
importing both publisher paths registers the same wire descriptors twice.
Inference contracts remain owned by `ani-inference-service/api/inference/v1`.
This API module does not contain an Inference protocol copy.

Import Model clients using:

```go
import (
    modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
    storagev1 "github.com/zhangzhe-ctrl/ani-model-service/api/storage/v1"
)

modelClient := modelv1.NewModelServiceClient(conn)
storageClient := storagev1.NewStorageServiceClient(conn)
```

For a tagged release, create the tag at the repository root with the `api/`
prefix (for example, `api/v0.1.0`); consumers use the version without the prefix.
This is the tag convention, not a claim that `api/v0.1.0` is already published.
Package removal is a Go API compatibility change even when the gRPC wire
contract is unchanged. Document the migration when publishing the next version.
Never use a permanent local `replace` or workspace to stand in for a release.

## RPC examples

- Models: `CreateModel`, `GetModel`, `ListModels`, `DeleteModel`.
- Versions: `CreateModelVersion`, `GetModelVersion`, `ListModelVersions`,
  `DeleteModelVersion`.
- Trained artifacts: `PrepareTrainedArtifact`, `CompleteTrainedArtifact`,
  `AbortTrainedArtifact`.
- Registration recovery: `GetTrainedArtifactRegistration` reads the original
  tenant/key/execution session without writing it. Optional object verification
  streams the original path and reports MISSING, VERIFIED, MISMATCH or
  UNAVAILABLE without marking uploaded or complete. A miss never proves a write
  was not sent. `GetTrainedArtifactUploadURL` signs only the pending original
  registration/key/execution/path, without creating another session or version.
- Finetuning preflight: `ResolveFinetuningFacts` observes a tenant-owned READY
  safetensors archive and the original target without creating a preparation
  ticket, reserving a name or writing an object. The MinIO provider requires a
  real non-null object VersionID, hashes that exact GET stream and every regular
  archive member, and returns bounded original config members. Unversioned
  storage and adapters without this capability fail closed. `ValidateFor`
  verifies the full response, target, member inventory and config byte hashes.
  The existing tenant catalogue and READY state define eligibility; there is no
  additional approval registry or caller-provided approval flag. A name conflict
  preflight does not replace the registration transaction's unique constraints.
- Storage: `CreateUploadURL`, `ObjectExists`, `VerifyChecksum`,
  `CreateDownloadURL`.

## Generation and verification

Run from the repository root:

```bash
make config                    # Regenerate protobuf output only
make generate                  # Also run Go generators and format Go source
make verify-api                # Tidy, tests, vet, build and module verification for api/
make verify-api-compatibility  # Load contracts together and test the service against local API
make verify                    # Generation, both modules and compatibility checks
```

The pinned workflow is `scripts/generate-api.sh`, using Buf v1.60.0,
protoc-gen-go v1.36.11 and protoc-gen-go-grpc v1.6.0. Do not edit generated Go
files by hand.

The root module's `go test ./...` does not traverse this nested module and uses
its published API dependency. `make test` explicitly tests both modules.
`make verify-api-compatibility` uses a disposable modfile pointing only the
Model API dependency at this checkout, loads every local API package together
with the published Inference contract to catch duplicate registration, and
runs the root tests against the local API. It does not modify repository
`go.mod`/`go.sum`, publish a version, or change normal build dependencies.
