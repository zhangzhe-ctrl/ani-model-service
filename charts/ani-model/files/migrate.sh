#!/bin/sh
set -eu

: "${DATABASE_DSN:?DATABASE_DSN is required}"

# ConfigMap volumes expose keys through symlinks into their `..data` directory.
# Treat those links as migration files as well as regular files.
files=$(find /migrations -maxdepth 1 \( -type f -o -type l \) -name '*.sql' -print | sort)
if [ -z "$files" ]; then
  echo "ERROR: no migration files in /migrations" >&2
  exit 1
fi

tmp_sql=$(mktemp)
trap 'rm -f "$tmp_sql"' EXIT

{
  cat <<'SQL'
\set ON_ERROR_STOP on
-- Hold the lock for the whole session so two Helm upgrades cannot interleave.
SELECT pg_advisory_lock(hashtextextended('ani-model-schema', 0));
BEGIN;
CREATE TABLE IF NOT EXISTS public.schema_migrations (
  filename text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
COMMIT;

-- Never guess a baseline for an untracked database. An operator must either
-- restore the migration ledger or explicitly migrate the existing schema.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = 'models'
  ) AND NOT EXISTS (SELECT 1 FROM public.schema_migrations) THEN
    RAISE EXCEPTION 'existing Model schema has no migration ledger; restore public.schema_migrations before retrying';
  END IF;
END
$$;
SQL

  for file in $files; do
    base=${file##*/}
    case "$base" in
      *[!A-Za-z0-9_.-]*) echo "ERROR: unsafe migration name: $base" >&2; exit 1 ;;
    esac
    # Filenames are validated above, so the literal is safe to embed. The
    # migration SQL and its ledger row are committed in the same transaction.
    printf "SELECT EXISTS (SELECT 1 FROM public.schema_migrations WHERE filename = '%s') AS migration_applied \\\gset\n" "$base"
    printf '\\if :migration_applied\n'
    printf '\\echo skip %s\n' "$base"
    printf '\\else\n'
    printf 'BEGIN;\n'
    printf '\\ir /migrations/%s\n' "$base"
    printf "INSERT INTO public.schema_migrations (filename) VALUES ('%s');\n" "$base"
    printf 'COMMIT;\n'
    printf '\\endif\n'
  done

  cat <<'SQL'
DO $$
DECLARE
  table_count integer;
BEGIN
  SELECT count(*) INTO table_count
  FROM information_schema.tables
  WHERE table_schema = 'public'
    AND table_name IN ('models', 'model_versions', 'model_artifacts', 'model_import_tasks', 'audit_events', 'trained_artifact_registrations');
  IF table_count <> 6 THEN
    RAISE EXCEPTION 'expected 6 Model tables, found %', table_count;
  END IF;
END
$$;
SELECT pg_advisory_unlock(hashtextextended('ani-model-schema', 0));
SQL
} >"$tmp_sql"

psql -v ON_ERROR_STOP=1 "$DATABASE_DSN" -f "$tmp_sql"
echo "migration complete"
