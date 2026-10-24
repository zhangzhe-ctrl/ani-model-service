# Model Trained Artifact Registration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a Model Service-only registration API that accepts a verified training `model.tar`, validates its storage bytes, and atomically creates a ModelVersion without changing ModelDev yet.

**Architecture:** Add an internal gRPC registration surface to Model Service. A durable registration session owns the upload reference, tenant, runtime tuple, target model metadata, execution id, and idempotency key. Complete validates the object through Storage, then commits the Artifact and ModelVersion in one PostgreSQL transaction; ModelDev integration is documented after this work and is not implemented here.

**Tech Stack:** Go, protobuf/buf, PostgreSQL/pgx, sqlc-generated queries, MinIO or Storage gRPC adapter, existing mTLS RPC policy.

## Global Constraints

- The first artifact is `model.tar` with `format=safetensors`; its `size_bytes` and SHA256 cover the complete tar.
- `engine_type`, `engine_image`, and `startup_command` are required and are copied verbatim or inherited verbatim from a source version; Model Service never synthesizes a command.
- Tenant identity comes from the authenticated principal; request tenant IDs must match it.
- Training and ModelDev are not modified in this plan.
- Generated protobuf files must be produced by `make generate`; run `make verify` before committing.

### Task 1: Define registration and storage contracts

**Files:**
- Modify: `api/model/v1/model.proto`
- Generate: `api/model/v1/model.pb.go`, `api/model/v1/model_grpc.pb.go`
- Test: `cmd/ani-model-service/rpc_policy_test.go`

**Interfaces:**
- Add `PrepareTrainedArtifact`, `CompleteTrainedArtifact`, and `AbortTrainedArtifact` to `ModelService`.
- Add an internal `ObjectVerifier` port. Keep the external Storage protobuf unchanged in this Model-only phase; MinIO verifies directly, while the existing gRPC adapter verifies through its download URL.
- Registration responses expose `registration_id`, upload URL/path, expiry, and final `model_id`/`model_version_id`.

- [x] Write failing policy tests for the new methods being denied to unknown identities and allowed only to the existing Governance control-plane identity.
- [x] Run `go test ./cmd/ani-model-service -run TestModelServicePolicy` and confirm failure because the methods are absent.
- [x] Add the protobuf messages and service methods with reserved fields for future packaging and multipart upload.
- [x] Run `make generate` and update generated files only through the pinned workflow.
- [x] Re-run the policy tests and confirm the generated method names are available.

### Task 2: Verify object size and digest at the Storage boundary

**Files:**
- Modify: `internal/biz/storage/storage.go`
- Modify: `internal/data/storage/minio.go`
- Modify: `internal/data/storage/grpc.go`
- Modify: `internal/data/storage/minio_test.go`
- Modify: `internal/data/storage/grpc_test.go`

**Interfaces:**
- Add a `VerifyObject` port accepting tenant, object reference, expected size, and expected SHA256, returning actual size and digest.
- Existing `VerifyChecksum` remains for old import workers; the new registration path uses `VerifyObject`.

- [x] Add failing MinIO and gRPC adapter tests for a matching size/digest and a mismatched size with matching digest.
- [x] Run the focused storage tests and confirm the new method is missing.
- [x] Implement MinIO using `StatObject` plus a streaming SHA256 pass; reject either size or digest mismatch.
- [x] Implement the gRPC adapter by downloading through the existing signed URL and counting bytes while hashing; reject an adapter that cannot obtain a valid download URL.
- [x] Run the focused storage tests and confirm they pass.

### Task 3: Persist registration sessions and commit the artifact atomically

**Files:**
- Create: `migrations/000008_trained_artifact_registration.sql`
- Create: `charts/ani-model/migrations/000008_trained_artifact_registration.sql`
- Create: `internal/biz/model/trained_artifact.go`
- Create: `internal/data/postgres/trained_artifact_store.go`
- Modify: `scripts/test-model-migrations.sh`
- Test: `internal/data/postgres/trained_artifact_integration_test.go`

**Interfaces:**
- Registration session states are `pending`, `uploaded`, `ready`, `failed`, and `aborted`.
- Complete uses one `pgx.Tx` to validate target ownership, create a new Model when needed, create ModelVersion and Artifact, update parent status, and mark the session ready.
- Replaying the same idempotency key returns the stored receipt; a changed digest returns conflict.

- [x] Add a failing integration test for a new Model and replay.
- [x] Run the integration test against the repository’s PostgreSQL test setup and confirm the session/store types are missing.
- [x] Add the registration table with unique `(tenant_id, idempotency_key)` and immutable object metadata.
- [x] Implement transactional create/complete/abort operations using the existing pgx transaction patterns.
- [x] Run the integration test and confirm the aggregate is committed once and replay returns the same receipt.

### Task 4: Expose the Model Service implementation and mTLS policy

**Files:**
- Modify: `internal/service/model.go`
- Create: `internal/service/trained_artifact.go`
- Modify: `cmd/ani-model-service/app.go`
- Modify: `cmd/ani-model-service/main.go`
- Modify: `cmd/ani-model-service/rpc_policy.go`
- Test: `internal/service/trained_artifact_test.go`, `cmd/ani-model-service/rpc_policy_test.go`

**Interfaces:**
- `PrepareTrainedArtifact` requires target model metadata when `target_model_id` is absent and validates the complete runtime tuple or source-version inheritance.
- `CompleteTrainedArtifact` returns the durable `registration_id`, `model_id`, `model_version_id`, status, and immutable artifact reference.
- `AbortTrainedArtifact` is tenant-scoped and terminal.

- [x] Add failing service tests for invalid metadata, missing runtime tuple, inherited runtime validation, object size/digest mismatch, idempotent replay, and completion receipt.
- [x] Run the focused service tests and confirm the new service implementation is absent.
- [x] Implement the methods using the Storage and registration-store ports; never call ModelDev or infer an engine command.
- [x] Authorize the three methods for the existing Governance control-plane identity during this Model-only phase; retain Inference policy unchanged. A dedicated ModelDev identity is deferred to the later cross-service integration document.
- [x] Wire the registration store and storage port into the composition root.
- [x] Run focused tests, `go test ./...`, `go vet ./...`, and `make verify`.

### Task 5: Review the Model-only result

**Files:**
- Modify: `docs/superpowers/specs/2026-10-20-trained-artifact-registration-design.md`
- Create: `docs/execution/records/2026-10-20-model-trained-artifact-registration.md`

- [x] Record the exact RPC contract, identity, state transitions, size/digest behavior, and known ModelDev integration boundary.
- [x] Confirm no ModelDev source or generated files changed.
- [x] Run `git diff --check` and record test commands and results.

