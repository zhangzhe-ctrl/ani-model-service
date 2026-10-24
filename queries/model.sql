-- name: GetModelVersion :one
SELECT v.tenant_id, v.id, v.model_id, v.version, v.format, v.status,
       v.is_encrypted, v.encrypt_algo, v.encrypt_hint, v.size_bytes,
       v.checksum_sha256, v.engine_type, v.engine_image, v.startup_command, v.startup_args,
       v.error_message, v.created_at, v.updated_at,
       m.model_id AS external_model_id,
       a.provider AS artifact_provider, a.reference AS artifact_reference,
       a.sha256 AS artifact_sha256, a.size_bytes AS artifact_size_bytes
FROM public.model_versions v
JOIN public.models m ON m.tenant_id = v.tenant_id AND m.id = v.model_id
LEFT JOIN public.model_artifacts a ON a.tenant_id = v.tenant_id AND a.model_version_id = v.id
WHERE v.tenant_id = $1 AND v.id = $2;

-- name: GetReadyModelVersion :one
SELECT v.tenant_id, v.id, v.model_id, v.version, v.format, v.status,
       v.is_encrypted, v.encrypt_algo, v.encrypt_hint, v.size_bytes,
       v.checksum_sha256, v.engine_type, v.engine_image, v.startup_command, v.startup_args,
       v.error_message, v.created_at, v.updated_at,
       m.model_id AS external_model_id,
       a.provider AS artifact_provider, a.reference AS artifact_reference,
       a.sha256 AS artifact_sha256, a.size_bytes AS artifact_size_bytes
FROM public.model_versions v
JOIN public.models m ON m.tenant_id = v.tenant_id AND m.id = v.model_id
JOIN public.model_artifacts a ON a.tenant_id = v.tenant_id AND a.model_version_id = v.id
WHERE v.tenant_id = $1 AND v.id = $2 AND m.status <> 'deleted' AND v.status = 'ready'
  AND a.sha256 <> ''
FOR SHARE OF m, v;

-- name: GetReadyModelVersionByExternalRef :one
SELECT v.tenant_id, v.id, v.model_id, v.version, v.format, v.status,
       v.is_encrypted, v.encrypt_algo, v.encrypt_hint, v.size_bytes,
       v.checksum_sha256, v.engine_type, v.engine_image, v.startup_command, v.startup_args,
       v.error_message, v.created_at, v.updated_at,
       m.model_id AS external_model_id,
       a.provider AS artifact_provider, a.reference AS artifact_reference,
       a.sha256 AS artifact_sha256, a.size_bytes AS artifact_size_bytes
FROM public.model_versions v
JOIN public.models m ON m.tenant_id = v.tenant_id AND m.id = v.model_id
JOIN public.model_artifacts a ON a.tenant_id = v.tenant_id AND a.model_version_id = v.id
WHERE v.tenant_id = $1 AND m.model_id = $2 AND v.version = $3
  AND m.status <> 'deleted' AND v.status = 'ready' AND a.sha256 <> ''
FOR SHARE OF m, v;

-- name: GetModel :one
SELECT tenant_id, id, model_id, name, display_name, description, source, source_repo_id,
       capabilities, status, error_message, total_size_bytes, created_at, updated_at,
       idempotency_key, request_fingerprint
FROM public.models WHERE tenant_id = $1 AND id = $2 AND status <> 'deleted';

-- name: GetModelByExternalID :one
SELECT tenant_id, id, model_id, name, display_name, description, source, source_repo_id,
       capabilities, status, error_message, total_size_bytes, created_at, updated_at,
       idempotency_key, request_fingerprint
FROM public.models
WHERE tenant_id = $1 AND model_id = $2 AND status <> 'deleted';

-- name: CreateModel :one
INSERT INTO public.models (tenant_id, id, model_id, name, display_name, description, source, capabilities, status, idempotency_key, request_fingerprint)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10)
RETURNING *;

-- name: GetModelByIdempotency :one
SELECT * FROM public.models WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: ListModels :many
SELECT tenant_id, id, model_id, name, display_name, description, source, source_repo_id,
       capabilities, status, error_message, total_size_bytes, created_at, updated_at,
       idempotency_key, request_fingerprint
FROM public.models
WHERE tenant_id = $1 AND status <> 'deleted'
  AND (sqlc.arg(status_filter)::text = '' OR status = sqlc.arg(status_filter))
  AND (sqlc.arg(source_filter)::text = '' OR source = sqlc.arg(source_filter))
  AND (sqlc.arg(capability)::text = '' OR capabilities ? sqlc.arg(capability)::text)
  AND (sqlc.arg(keyword)::text = ''
       OR strpos(lower(name), lower(sqlc.arg(keyword))) > 0
       OR strpos(lower(display_name), lower(sqlc.arg(keyword))) > 0
       OR strpos(lower(model_id), lower(sqlc.arg(keyword))) > 0)
  AND (sqlc.narg(before_created_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(before_created_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)
OFFSET sqlc.arg(page_offset);

-- name: CountModels :one
SELECT count(*)
FROM public.models
WHERE tenant_id = sqlc.arg(tenant_id)
  AND status <> 'deleted'
  AND (sqlc.arg(status_filter)::text = '' OR status = sqlc.arg(status_filter))
  AND (sqlc.arg(source_filter)::text = '' OR source = sqlc.arg(source_filter))
  AND (sqlc.arg(capability)::text = '' OR capabilities ? sqlc.arg(capability)::text)
  AND (sqlc.arg(keyword)::text = ''
       OR strpos(lower(name), lower(sqlc.arg(keyword))) > 0
       OR strpos(lower(display_name), lower(sqlc.arg(keyword))) > 0
       OR strpos(lower(model_id), lower(sqlc.arg(keyword))) > 0);

-- name: SoftDeleteModel :execrows
UPDATE public.models
SET status = 'deleted', deleted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status <> 'deleted';

-- name: CreateModelVersion :one
WITH parent AS (
 SELECT m.id FROM public.models m
 WHERE m.tenant_id = sqlc.arg(tenant_id) AND m.id = sqlc.arg(model_id) AND m.status <> 'deleted'
 FOR KEY SHARE
)
INSERT INTO public.model_versions
 (tenant_id, id, model_id, version, format, status, is_encrypted, encrypt_algo,
  encrypt_hint, size_bytes, checksum_sha256, engine_type, engine_image, startup_command, startup_args, idempotency_key, request_fingerprint)
SELECT sqlc.arg(tenant_id), sqlc.arg(id), parent.id, sqlc.arg(version), sqlc.arg(format), 'pending',
       sqlc.arg(is_encrypted), sqlc.arg(encrypt_algo), sqlc.arg(encrypt_hint), sqlc.arg(size_bytes),
       sqlc.arg(checksum_sha256), sqlc.arg(engine_type), sqlc.arg(engine_image), sqlc.arg(startup_command), sqlc.arg(startup_args),
       sqlc.arg(idempotency_key), sqlc.arg(request_fingerprint) FROM parent
RETURNING *;

-- name: MarkModelVersionReady :execrows
WITH ready_version AS (
  UPDATE public.model_versions v
  SET status = 'ready', updated_at = clock_timestamp(), error_message = ''
  WHERE v.tenant_id = $1 AND v.id = $2 AND v.status IN ('pending', 'importing')
    AND EXISTS (
      SELECT 1 FROM public.model_artifacts a
      WHERE a.tenant_id = v.tenant_id
        AND a.model_version_id = v.id
        AND a.reference <> ''
        AND a.sha256 = v.checksum_sha256
        AND a.sha256 <> ''
    )
    AND EXISTS (
      SELECT 1 FROM public.models m
      WHERE m.tenant_id = v.tenant_id AND m.id = v.model_id AND m.status <> 'deleted'
    )
  RETURNING tenant_id, model_id
)
UPDATE public.models m
SET status = 'ready', error_message = '', updated_at = clock_timestamp()
FROM ready_version v
WHERE m.tenant_id = v.tenant_id AND m.id = v.model_id;

-- name: MarkModelVersionError :execrows
UPDATE public.model_versions
SET status = 'error', error_message = $3, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status IN ('pending', 'importing');

-- name: MarkModelVersionErrorForTask :one
WITH leased_task AS (
  SELECT t.tenant_id, t.model_id, t.model_version_id
  FROM public.model_import_tasks t
  WHERE t.tenant_id = $1 AND t.id = $2 AND t.model_version_id = $3
    AND t.status = 'importing' AND t.lease_owner = $4 AND t.lease_epoch = $5
    AND t.lease_until > clock_timestamp()
  FOR UPDATE
)
UPDATE public.model_versions v
SET status = 'error', error_message = $6, updated_at = clock_timestamp()
FROM leased_task t
WHERE v.tenant_id = t.tenant_id AND v.id = t.model_version_id
  AND v.status IN ('pending', 'importing')
RETURNING v.tenant_id, v.model_id;

-- name: LockModelForVersionError :one
SELECT m.tenant_id, m.id
FROM public.models m
WHERE m.tenant_id = $1 AND m.id = $2
FOR UPDATE;

-- name: MarkModelErrorIfNoReady :execrows
UPDATE public.models m
SET status = 'error', error_message = $3, updated_at = clock_timestamp()
WHERE m.tenant_id = $1 AND m.id = $2 AND m.status IN ('pending', 'downloading', 'error')
  AND NOT EXISTS (
    SELECT 1
    FROM public.model_versions ready
    WHERE ready.tenant_id = m.tenant_id
      AND ready.model_id = m.id
      AND ready.status = 'ready'
  );

-- name: SetModelVersionChecksum :execrows
UPDATE public.model_versions
SET checksum_sha256 = $3, size_bytes = CASE WHEN $4::bigint >= 0 THEN $4 ELSE size_bytes END,
    updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND (checksum_sha256 = '' OR checksum_sha256 = $3)
  AND (
    status IN ('pending', 'importing')
    OR (status = 'ready' AND checksum_sha256 = $3
        AND ($4::bigint < 0 OR size_bytes = $4))
  );

-- name: GetModelVersionByIdempotency :one
SELECT * FROM public.model_versions WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetModelVersionForImport :one
SELECT id
FROM public.model_versions
WHERE tenant_id = $1 AND model_id = $2 AND version = $3
  AND status IN ('pending', 'importing', 'error');

-- name: BindImportTask :execrows
UPDATE public.model_import_tasks
SET model_id = $5, model_version_id = $6, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status = 'importing'
  AND lease_owner = $3 AND lease_epoch = $4 AND lease_until > clock_timestamp()
  AND model_version_id IS NULL;

-- name: ListModelVersions :many
SELECT v.tenant_id, v.id, v.model_id, v.version, v.format, v.status, v.is_encrypted, v.encrypt_algo,
       v.encrypt_hint, v.size_bytes, v.checksum_sha256, v.engine_type, v.startup_command,
       v.engine_image, v.startup_args, v.error_message, v.created_at, v.updated_at,
       m.model_id AS external_model_id,
       COALESCE(a.provider, '')::text AS artifact_provider,
       COALESCE(a.reference, '')::text AS artifact_reference
FROM public.model_versions v
JOIN public.models m ON m.tenant_id = v.tenant_id AND m.id = v.model_id
LEFT JOIN public.model_artifacts a ON a.tenant_id = v.tenant_id AND a.model_version_id = v.id
WHERE v.tenant_id = $1 AND v.model_id = $2 AND v.status <> 'deleted' AND m.status <> 'deleted'
  AND (sqlc.arg(status)::text = '' OR v.status = sqlc.arg(status)::text)
  AND (sqlc.narg(before_created_at)::timestamptz IS NULL
       OR (v.created_at, v.id) < (sqlc.narg(before_created_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY v.created_at DESC, v.id DESC
LIMIT sqlc.arg(page_limit)
OFFSET sqlc.arg(page_offset);

-- name: CountModelVersions :one
SELECT count(*)
FROM public.model_versions v
JOIN public.models m ON m.tenant_id = v.tenant_id AND m.id = v.model_id
WHERE v.tenant_id = sqlc.arg(tenant_id) AND v.model_id = sqlc.arg(model_id)
  AND v.status <> 'deleted' AND m.status <> 'deleted'
  AND (sqlc.arg(status)::text = '' OR v.status = sqlc.arg(status)::text);

-- name: LatestModelVersions :many
SELECT DISTINCT ON (v.model_id)
       v.tenant_id, v.id, v.model_id, v.version, v.format, v.status, v.is_encrypted, v.encrypt_algo,
       v.encrypt_hint, v.size_bytes, v.checksum_sha256, v.engine_type, v.startup_command,
       v.engine_image, v.startup_args, v.error_message, v.created_at, v.updated_at,
       m.model_id AS external_model_id,
       COALESCE(a.provider, '')::text AS artifact_provider,
       COALESCE(a.reference, '')::text AS artifact_reference
FROM public.model_versions v
JOIN public.models m ON m.tenant_id = v.tenant_id AND m.id = v.model_id
LEFT JOIN public.model_artifacts a ON a.tenant_id = v.tenant_id AND a.model_version_id = v.id
WHERE v.tenant_id = $1 AND v.model_id = ANY($2::uuid[]) AND v.status <> 'deleted' AND m.status <> 'deleted'
ORDER BY v.model_id, v.created_at DESC, v.id DESC;

-- name: ClaimImportTask :one
UPDATE public.model_import_tasks
SET status = 'importing', lease_owner = $3, lease_epoch = lease_epoch + 1,
    lease_until = clock_timestamp() + ($4::bigint * interval '1 microsecond'),
    attempt_count = attempt_count + 1, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status IN ('pending','importing')
  AND next_attempt_at <= clock_timestamp()
  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
RETURNING *;

-- name: CompleteImportTask :execrows
UPDATE public.model_import_tasks
SET status = 'completed', progress_pct = 100, lease_owner = '', lease_until = NULL,
    completed_at = clock_timestamp(), error_message = '', updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status = 'importing'
  AND lease_owner = $3 AND lease_epoch = $4 AND lease_until > clock_timestamp();

-- name: RetryImportTask :execrows
UPDATE public.model_import_tasks
SET status = 'pending', lease_owner = '', lease_until = NULL,
    next_attempt_at = clock_timestamp() + ($5::bigint * interval '1 microsecond'),
    error_message = $6, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status = 'importing'
  AND lease_owner = $3 AND lease_epoch = $4 AND lease_until > clock_timestamp();

-- name: FailImportTask :execrows
UPDATE public.model_import_tasks
SET status = 'failed', lease_owner = '', lease_until = NULL,
    error_message = $5, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status = 'importing'
  AND lease_owner = $3 AND lease_epoch = $4 AND lease_until > clock_timestamp();

-- name: InsertAuditEvent :exec
INSERT INTO public.audit_events
 (tenant_id, id, actor, workload, request_id, task_id, action,
  before_state, after_state, error_class, model_id, model_version_id)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, '00000000-0000-0000-0000-000000000000')::uuid,
        $7, $8, $9, $10, sqlc.narg(model_id)::uuid, sqlc.narg(model_version_id)::uuid);

-- name: CreateImportTask :one
INSERT INTO public.model_import_tasks
 (tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
  revision, idempotency_key, request_fingerprint, status)
VALUES ($1, $2, $3, NULLIF($4, '00000000-0000-0000-0000-000000000000')::uuid,
        $5, $6, $7, $8, $9, $10, 'pending')
RETURNING *;

-- name: ListDueImportTasks :many
SELECT tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
       revision, idempotency_key, status, attempt_count, progress_pct,
       lease_owner, lease_epoch, lease_until, next_attempt_at, error_message,
       created_at, updated_at, completed_at
FROM public.model_import_tasks
WHERE ($1::uuid IS NULL OR tenant_id = $1)
  AND status IN ('pending','importing')
  AND next_attempt_at <= clock_timestamp()
  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
ORDER BY next_attempt_at, created_at, id
LIMIT $2;

-- name: ListDueImportTasksAll :many
SELECT tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
       revision, idempotency_key, status, attempt_count, progress_pct,
       lease_owner, lease_epoch, lease_until, next_attempt_at, error_message,
       created_at, updated_at, completed_at
FROM public.model_import_tasks
WHERE status IN ('pending','importing')
  AND next_attempt_at <= clock_timestamp()
  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
ORDER BY next_attempt_at, created_at, id
LIMIT $1;

-- name: GetImportTaskByIdempotency :one
SELECT tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
       revision, idempotency_key, request_fingerprint, status, attempt_count,
       progress_pct, lease_owner, lease_epoch, lease_until, next_attempt_at,
       error_message, created_at, updated_at, completed_at
FROM public.model_import_tasks
WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetImportTask :one
SELECT tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
       revision, idempotency_key, request_fingerprint, status, attempt_count,
       progress_pct, lease_owner, lease_epoch, lease_until, next_attempt_at,
       error_message, created_at, updated_at, completed_at
FROM public.model_import_tasks
WHERE tenant_id = $1 AND id = $2;

-- name: RetryFailedImportTask :one
UPDATE public.model_import_tasks
SET status = 'pending', attempt_count = 0, progress_pct = 0,
    lease_owner = '', lease_until = NULL, next_attempt_at = clock_timestamp(),
    error_message = '', completed_at = NULL, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status = 'failed'
RETURNING tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
          revision, idempotency_key, request_fingerprint, status, attempt_count,
          progress_pct, lease_owner, lease_epoch, lease_until, next_attempt_at,
          error_message, created_at, updated_at, completed_at;

-- name: RetryFailedImportTaskAndResetVersion :one
WITH failed_task AS (
  SELECT t0.tenant_id, t0.id, t0.model_version_id
  FROM public.model_import_tasks t0
  WHERE t0.tenant_id = sqlc.arg(tenant_id) AND t0.id = sqlc.arg(id) AND t0.status = 'failed'
  FOR UPDATE
), reset_version AS (
  UPDATE public.model_versions v
  SET status = 'pending', error_message = '', updated_at = clock_timestamp()
  FROM failed_task t
  WHERE v.tenant_id = t.tenant_id AND v.id = t.model_version_id AND v.status = 'error'
)
UPDATE public.model_import_tasks t
SET status = 'pending', attempt_count = 0, progress_pct = 0, lease_owner = '',
    lease_until = NULL, next_attempt_at = clock_timestamp(), error_message = '',
    completed_at = NULL, updated_at = clock_timestamp()
FROM failed_task
WHERE t.tenant_id = failed_task.tenant_id AND t.id = failed_task.id
RETURNING t.*;

-- name: RenewImportTaskLease :execrows
UPDATE public.model_import_tasks
SET lease_until = clock_timestamp() + ($5::bigint * interval '1 microsecond'),
    updated_at = clock_timestamp()
WHERE tenant_id = $1 AND id = $2 AND status = 'importing'
  AND lease_owner = $3 AND lease_epoch = $4 AND lease_until > clock_timestamp();

-- name: CreateModelArtifact :one
INSERT INTO public.model_artifacts
 (tenant_id, id, model_version_id, provider, reference, format, size_bytes,
  sha256, is_encrypted, encrypt_algo)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetModelArtifact :one
SELECT tenant_id, id, model_version_id, provider, reference, format, size_bytes,
       sha256, is_encrypted, encrypt_algo, created_at
FROM public.model_artifacts
WHERE tenant_id = $1 AND model_version_id = $2;

-- name: GetBundleModelByIdempotency :one
SELECT * FROM public.models
WHERE tenant_id = sqlc.arg(tenant_id) AND idempotency_key = sqlc.arg(idempotency_key);

-- name: GetBundleVersionByIdempotency :one
SELECT * FROM public.model_versions
WHERE tenant_id = sqlc.arg(tenant_id) AND idempotency_key = sqlc.arg(idempotency_key);

-- name: GetBundleTaskByIdempotency :one
SELECT tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
       revision, idempotency_key, request_fingerprint, status, attempt_count,
       progress_pct, lease_owner, lease_epoch, lease_until, next_attempt_at,
       error_message, created_at, updated_at, completed_at
FROM public.model_import_tasks
WHERE tenant_id = sqlc.arg(tenant_id) AND idempotency_key = sqlc.arg(idempotency_key);

-- name: CreateBundleModel :one
INSERT INTO public.models
 (tenant_id, id, model_id, name, display_name, description, source, source_repo_id,
  capabilities, status, idempotency_key, request_fingerprint)
VALUES (sqlc.arg(tenant_id), sqlc.arg(id), sqlc.arg(model_id), sqlc.arg(name),
        sqlc.arg(display_name), sqlc.arg(description), sqlc.arg(source),
        sqlc.arg(source_repo_id), sqlc.arg(capabilities), 'pending',
        sqlc.arg(idempotency_key), sqlc.arg(request_fingerprint))
RETURNING *;

-- name: CreateBundleVersion :one
INSERT INTO public.model_versions
 (tenant_id, id, model_id, version, format, status, is_encrypted, encrypt_algo,
  encrypt_hint, size_bytes, checksum_sha256, engine_type, engine_image,
  startup_command, startup_args, idempotency_key, request_fingerprint)
VALUES (sqlc.arg(tenant_id), sqlc.arg(id), sqlc.arg(model_id), sqlc.arg(version),
        sqlc.arg(format), 'pending', false, '', '', 0, '', sqlc.arg(engine_type),
        sqlc.arg(engine_image), sqlc.arg(startup_command), sqlc.arg(startup_args),
        sqlc.arg(idempotency_key), sqlc.arg(request_fingerprint))
RETURNING *;

-- name: CreateBundleTask :one
INSERT INTO public.model_import_tasks
 (tenant_id, id, model_id, model_version_id, task_type, source, repo_id,
  revision, idempotency_key, request_fingerprint, status)
VALUES (sqlc.arg(tenant_id), sqlc.arg(id), sqlc.arg(model_id), sqlc.arg(model_version_id),
        sqlc.arg(task_type), sqlc.arg(source), sqlc.arg(repo_id), sqlc.arg(revision),
        sqlc.arg(idempotency_key), sqlc.arg(request_fingerprint), 'pending')
RETURNING *;

-- name: ListModelOperations :many
SELECT e.*, COALESCE(e.model_id, t.model_id) AS resolved_model_id,
       COALESCE(e.model_version_id, t.model_version_id) AS resolved_model_version_id
FROM public.audit_events e
LEFT JOIN public.model_import_tasks t ON t.tenant_id = e.tenant_id AND t.id = e.task_id
WHERE e.tenant_id = sqlc.arg(tenant_id)
  AND COALESCE(e.model_id, t.model_id) = sqlc.arg(model_id)::uuid
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountModelOperations :one
SELECT count(*) FROM public.audit_events e
LEFT JOIN public.model_import_tasks t ON t.tenant_id = e.tenant_id AND t.id = e.task_id
WHERE e.tenant_id = sqlc.arg(tenant_id)
  AND COALESCE(e.model_id, t.model_id) = sqlc.arg(model_id)::uuid;
