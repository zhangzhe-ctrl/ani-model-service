# Governance catalog-read

This backend mode serves the existing `/model.v1.ModelService/ListModels` RPC from
the real PostgreSQL catalog. Select it explicitly with `ANI_MODEL_MODE=catalog-read`.
The normal runtime remains separate; this mode does not start import, object
storage, or worker components and does not claim they are ready.

Required environment:

| Variable | Meaning |
| --- | --- |
| `ANI_DATABASE_DSN` | Model-owned PostgreSQL database; apply `migrations/` first |
| `ANI_MODEL_CLIENT_CA` | PEM CA used to verify incoming client certificates |
| `ANI_MODEL_TLS_CERT` | PEM server certificate, exact DNS SAN `ani-model-service` |
| `ANI_MODEL_TLS_KEY` | Matching private key |

Use the existing `-conf` flag with server configuration, for example:

```yaml
server:
  grpc: {network: tcp, addr: '0.0.0.0:19090', timeout: 5s}
  admin: {network: tcp, addr: '0.0.0.0:19091', timeout: 2s}
  shutdown_timeout: 5s
```

Startup rejects missing TLS configuration or unavailable PostgreSQL. `/readyz`
checks both process lifecycle and a bounded live database ping. PostgreSQL outage
withdraws readiness; successful reconnect restores it. No catalog data is cached
as a substitute for database queries.

TLS 1.3 requires a valid client chain and clientAuth usage with exact DNS SAN
`ani-governance`; the verified certificate determines workload identity.
Each metadata key must have exactly one value:

- `x-ani-tenant-id`: canonical lowercase nonzero UUID.
- `x-ani-actor`: `governance:user:<canonical nonzero uint32>`.
- `x-ani-request-id`: canonical lowercase nonzero UUID.

Only then is a Principal created. Request `tenant_id` must equal its tenant UUID.
The governance certificate is admitted only to ListModels; every other unary
business method and every stream is denied. Reflection is disabled.

ListModels supports optional status and limits 1–1000 (default 100). Nonempty
cursor, keyword, source, and capability are rejected as InvalidArgument. Reads
retain the tenant SQL predicate and `created_at DESC, id DESC` ordering. Database
errors return Unavailable and retain their original cause in server logs.
Database connection establishment is capped at one second, so an unreachable
database can return Unavailable before the Governance deadline. Established
queries still follow the request context and cancellation.

The Governance HTTP adapter separately limits results to 100 and projects only
id, model_id, name, display_name, source, capabilities, status. This slice does
not implement creation, import, upload, full pagination, timestamps, or production
cutover. Its private database fixture verifies reads only.

The paired Governance repository contains `backend/scripts/model-lab/` for the
Fedora-only isolated deployment and acceptance, and
`docs/model-catalog-bootstrap.md` for pinned inputs, results, and retained resources.
Private CA keys, passwords, and login tokens are never checked into either repo.
