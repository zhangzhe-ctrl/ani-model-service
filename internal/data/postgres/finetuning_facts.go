package postgres

import (
	"context"
	"github.com/google/uuid"
)

// New trained containers use the name as their external model_id too. Both
// existing unique keys include deleted rows. Registration arbitrates races.
func (s *CatalogStore) ModelNameExists(ctx context.Context, tenant, name string) (bool, error) {
	id, err := uuid.Parse(tenant)
	if err != nil {
		return false, err
	}
	var exists bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.models WHERE tenant_id=$1 AND (name=$2 OR model_id=$2))`, uuidType(id), name).Scan(&exists)
	return exists, err
}
