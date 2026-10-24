-- Preserve model/version identity independently of task lifecycle and deletion.
-- Old task-linked events can be attributed; old events without a task or model
-- identity cannot safely be backfilled by guessing from actor or request_id.
ALTER TABLE public.audit_events ADD COLUMN model_id UUID;
ALTER TABLE public.audit_events ADD COLUMN model_version_id UUID;
UPDATE public.audit_events e
SET model_id = t.model_id, model_version_id = t.model_version_id
FROM public.model_import_tasks t
WHERE e.tenant_id = t.tenant_id AND e.task_id = t.id;
CREATE INDEX audit_events_model_history_idx
ON public.audit_events (tenant_id, model_id, created_at DESC, id DESC);
