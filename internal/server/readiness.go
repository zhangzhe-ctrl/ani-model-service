package server

import (
	"context"
	"sync/atomic"
	"time"
)

// Readiness records only the local process lifecycle. A service with external
// dependencies must add domain-specific readiness checks at composition time.
type Readiness struct {
	// Set once during composition, before servers start.
	probe                     func(context.Context) error
	ready                     atomic.Bool
	postgres, storage, worker atomic.Bool
	dependenciesRequired      atomic.Bool
}

func NewReadiness() *Readiness {
	return &Readiness{}
}

func (r *Readiness) Set(ready bool) {
	r.ready.Store(ready)
}

func (r *Readiness) Ready() bool {
	if !r.ready.Load() {
		return false
	}
	if r.probe != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return r.probe(ctx) == nil
	}
	if !r.dependenciesRequired.Load() {
		return true
	}
	return r.postgres.Load() && r.storage.Load() && r.worker.Load()
}

// NewCatalogReadiness checks live PostgreSQL on every readiness observation.
// It does not claim Storage or worker readiness.
func NewCatalogReadiness(probe func(context.Context) error) *Readiness {
	r := NewReadiness()
	if probe == nil {
		r.RequireDependencies()
	} else {
		r.probe = probe
	}
	return r
}

// RequireDependencies switches readiness to dependency-gated mode. Callers
// must report each external dependency explicitly; absent providers remain
// unready.
func (r *Readiness) RequireDependencies()    { r.dependenciesRequired.Store(true) }
func (r *Readiness) SetPostgresReady(v bool) { r.postgres.Store(v) }
func (r *Readiness) SetStorageReady(v bool)  { r.storage.Store(v) }
func (r *Readiness) SetWorkerReady(v bool)   { r.worker.Store(v) }
