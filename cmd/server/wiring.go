package main

import (
	"qoder2api/internal/pool"
	"qoder2api/internal/server"
)

// realmAwareAvailableForModel routes session stickiness by realm-aware model availability,
// preventing cross-realm sticky leaks between CN and Global accounts.
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}
