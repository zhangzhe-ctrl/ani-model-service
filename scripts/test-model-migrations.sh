#!/usr/bin/env bash
set -euo pipefail

# Integration check for the Helm migration payload. It uses an already-running
# disposable PostgreSQL container and removes its temporary database on exit.
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
pg_container=${PG_TEST_CONTAINER:-recycling-postgres}
db_name="ani_model_migration_test_${RANDOM}_$$"
partial_db="ani_model_migration_partial_${RANDOM}_$$"
db_user=${PG_TEST_USER:-recycling_user}
db_password=${PG_TEST_PASSWORD:-change-me-before-development}
network=$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{end}}' "$pg_container")
dsn="postgresql://${db_user}:${db_password}@${pg_container}:5432/${db_name}"
tmp_dir=$(mktemp -d)
cleanup() {
  docker exec "$pg_container" dropdb -U "$db_user" --if-exists "$db_name" >/dev/null 2>&1 || true
  docker exec "$pg_container" dropdb -U "$db_user" --if-exists "$partial_db" >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

docker exec "$pg_container" createdb -U "$db_user" "$db_name"

run_migration() {
  dsn_arg=$1
  migration_dir=$2
  docker run --rm --network "$network" \
    -e DATABASE_DSN="$dsn_arg" \
    -e PGPASSWORD="$db_password" \
    -v "$repo_root/charts/ani-model/files/migrate.sh:/migrate.sh:ro" \
    -v "$migration_dir:/migrations:ro" \
    postgres:16-alpine sh /migrate.sh
}

run_migration "$dsn" "$repo_root/charts/ani-model/migrations"
run_migration "$dsn" "$repo_root/charts/ani-model/migrations"

count=$(docker exec "$pg_container" psql -U "$db_user" -d "$db_name" -Atqc \
  "SELECT count(*) FROM public.schema_migrations")
expected_count=$(find "$repo_root/charts/ani-model/migrations" -maxdepth 1 -name '*.sql' -type f | wc -l)
test "$count" = "$expected_count"

cp -a "$repo_root/charts/ani-model/migrations/." "$tmp_dir/"
cat >"$tmp_dir/999999_rollback.sql" <<'SQL'
CREATE TABLE public.should_rollback (id integer NOT NULL);
SELECT no_such_migration_function();
SQL
if run_migration "$dsn" "$tmp_dir"; then
  echo "expected the broken migration to fail" >&2
  exit 1
fi

docker exec "$pg_container" createdb -U "$db_user" "$partial_db"
docker exec "$pg_container" psql -U "$db_user" -d "$partial_db" -v ON_ERROR_STOP=1 \
  -c 'CREATE TABLE public.models (model_id text NOT NULL)'
partial_dsn="postgresql://${db_user}:${db_password}@${pg_container}:5432/${partial_db}"
if run_migration "$partial_dsn" "$repo_root/charts/ani-model/migrations"; then
  echo "expected an untracked existing schema to be rejected" >&2
  exit 1
fi

if docker exec "$pg_container" psql -U "$db_user" -d "$db_name" -Atqc \
  "SELECT to_regclass('public.should_rollback') IS NOT NULL" | grep -qx t; then
  echo "broken migration was not rolled back" >&2
  exit 1
fi
if docker exec "$pg_container" psql -U "$db_user" -d "$db_name" -Atqc \
  "SELECT count(*) FROM public.schema_migrations WHERE filename = '999999_rollback.sql'" | grep -vq '^0$'; then
  echo "broken migration was recorded as applied" >&2
  exit 1
fi

echo "model migration integration checks passed"
