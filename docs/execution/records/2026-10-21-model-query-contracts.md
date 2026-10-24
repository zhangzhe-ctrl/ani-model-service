# Model version filters and operation history

## Contract

`ListModelVersionsRequest.status` filters in PostgreSQL before pagination. The
same predicate applies to the total count. Accepted canonical values are empty
(all visible states), `pending`, `importing`, `ready`, and `error`. The UI aliases
`downloading` and `failed` resolve to `importing` and `error`. Invalid states are
rejected; deleted models/versions remain excluded. Returned state values remain
canonical.

`ListModelOperations` accepts `tenant_id`, `model_id`, `page`, and `page_size`.
It resolves the tenant-scoped model selector, returns `items` plus matching
`total`, and orders by `(created_at, id)` descending. Governance is authorized
to call it; the Inference workload is not. Default page size and bounds are the
same as the existing model list.

Each item contains action, actor, timestamp, result, model/version identifiers,
request/task identifiers and before/after states. `accepted` means an import
was queued, not completed. `failed` reflects a recorded failure state or error
class. `succeeded` describes successful synchronous actions. This does not
rewrite an old acceptance record when its asynchronous task later finishes;
the task API remains the source of its current outcome.

## Persistence and coverage

Migration `000009_model_operation_history.sql` adds model/version identity to
audit events in both root SQL schema and Helm migrations. The five existing
audit writers (model creation, version creation, import, bundle import, retry)
now preserve this identity. Historical task-linked events are backfilled; the
read path also resolves task identity for tasks bound after event insertion.
Historical events without model identity or a task cannot safely be attributed
and are not fabricated. This query does not claim new universal audit coverage
for operations that previously did not emit events, such as deletion.

## Validation

- Protobuf generated through the pinned Buf workflow; SQL through sqlc v1.31.1.
- `scripts/verify-api-compatibility`: all service tests passed against the local
  unpublished API, including process-level startup tests. The compatibility
  script now propagates the temporary modfile to subprocess Go builds.
- `TestModelQueriesPostgres`: passed against a disposable local PostgreSQL
  database, covering filtered counts and pages, aliases, deleted exclusion,
  cross-tenant isolation, operation results and empty history pages.
- `scripts/test-model-migrations.sh`: passed initial application, repeated
  application, failed-migration rollback, and untracked-schema rejection.
- Database fixtures were rolled back and the disposable database was removed.

The root runtime module retains its immutable published API dependency. New
RPCs must be released and consumer dependencies updated before ordinary builds
can compile these new service handlers. Local compatibility verification uses
only temporary modfiles; no runtime `replace` or workspace was introduced.
No API release, image update, or cluster migration was performed for this work.
