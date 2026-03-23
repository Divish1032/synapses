package federation

import (
	"context"
	"log"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// CrossProjectSearch handles cross-project entity lookups, BFS context
// carving, dependency enrichment, and memory/episode search across
// sibling stores. This is the component that cross-project knowledge
// queries call.
type CrossProjectSearch struct {
	resolver *Resolver
}

func newCrossProjectSearch(r *Resolver) *CrossProjectSearch {
	return &CrossProjectSearch{
		resolver: r,
	}
}

// EntityExists checks whether an entity exists in a sibling's store.
// Fail-open: returns false on any error.
func (s *CrossProjectSearch) EntityExists(ctx context.Context, alias string, entityName string) bool {
	if ctx.Err() != nil {
		return false
	}
	st := s.resolver.getStore(alias)
	if st == nil {
		return false
	}
	exists, err := st.NodeExistsByNameCtx(ctx, entityName)
	if err != nil {
		return false
	}
	return exists
}

// FindEntities searches sibling stores for entities matching query.
// If aliases is nil or empty, all siblings are searched.
// Errors on individual siblings are silently skipped.
// BUG-023: queries run in parallel via errgroup for O(1) latency.
func (s *CrossProjectSearch) FindEntities(ctx context.Context, query string, aliases []string, limit int) []FederatedSearchResult {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	targets := s.resolver.filterEntries(aliases)

	var mu sync.Mutex
	var results []FederatedSearchResult

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8) // bound parallelism to avoid overwhelming sibling stores
	for _, e := range targets {
		e := e // capture loop variable
		g.Go(func() error {
			st := s.resolver.getStore(e.Alias)
			if st == nil {
				return nil
			}

			nodes, err := st.FindNodesByNameCtx(gctx, query, limit)
			if err != nil {
				log.Printf("federation: find_entity in %q: %v", e.Alias, err)
				return nil // fail-open
			}
			if len(nodes) == 0 {
				return nil
			}

			mu.Lock()
			results = append(results, FederatedSearchResult{
				Alias:   e.Alias,
				Results: nodes,
			})
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // errors are nil (fail-open)
	return results
}

// GetDepsForEntity returns cross-project deps for a specific local entity.
// Used by prepare_context to enrich responses. Returns nil when the entity
// has no cross-project dependencies or the local store is unavailable.
func (s *CrossProjectSearch) GetDepsForEntity(ctx context.Context, entityID string, localStore *store.Store) []CrossProjectDepStatus {
	if localStore == nil || ctx.Err() != nil {
		return nil
	}
	deps, err := localStore.GetCrossProjectDeps(entityID)
	if err != nil || len(deps) == 0 {
		return nil
	}

	var results []CrossProjectDepStatus
	for _, dep := range deps {
		if ctx.Err() != nil {
			break
		}
		status := CrossProjectDepStatus{
			Project: dep.ToProject,
			Entity:  dep.ToEntity,
			File:    dep.ToFile,
		}

		// Enrich with graph-based drift check if sibling store is available,
		// fresh (re-indexed after latest commit), and we have a stored
		// signature to compare against. If the store is stale, skip
		// enrichment entirely — the agent gets no false confidence, and
		// the authoritative CheckDrift from session_init handles it.
		if dep.VerifiedSignature != "" {
			sibStore := s.resolver.getStore(dep.ToProject)
			repoPath := s.resolver.entryPath(dep.ToProject)
			if sibStore != nil && repoPath != "" && s.resolver.isSiblingStoreFresh(ctx, sibStore, s.resolver.cachedHead(ctx, dep.ToProject), repoPath) {
				nodes, findErr := sibStore.FindNodesByNameCtx(ctx, dep.ToEntity, 1)
				if findErr == nil && len(nodes) > 0 {
					if nodes[0].Signature != dep.VerifiedSignature {
						status.Drifted = true
						status.DiffSummary = structuralSignatureDiff(dep.VerifiedSignature, nodes[0].Signature)
					}
				} else if findErr == nil && len(nodes) == 0 {
					status.Drifted = true
					status.DiffSummary = "Entity no longer exists in sibling project"
				}
				// findErr != nil → fail-open, leave as not drifted
			}
		}

		results = append(results, status)
	}
	return results
}

// GetEntityContext loads a sibling graph and carves BFS context for an entity.
// This is the "full BFS" option — opt-in via projects= parameter on tools.
// NOT called automatically during enrichment. Returns nil on any error.
func (s *CrossProjectSearch) GetEntityContext(ctx context.Context, entity string, alias string, depth int) *FederatedContext {
	if ctx.Err() != nil {
		return nil
	}
	st := s.resolver.getStore(alias)
	if st == nil {
		return nil
	}

	g, err := st.LoadGraph()
	if err != nil || g == nil {
		if err != nil {
			log.Printf("federation: load graph for %q: %v", alias, err)
		}
		return nil
	}

	nodes := g.FindByName(entity)
	if len(nodes) == 0 {
		return nil
	}

	if ctx.Err() != nil {
		return nil
	}

	// Carve BFS context around the first matching node.
	root := nodes[0]
	cfg := graph.DefaultCarveConfig()
	if depth > 0 {
		cfg.MaxDepth = depth
	}
	sub, err := g.CarveEgoGraph(root.ID, cfg)
	if err != nil || sub == nil {
		if err != nil {
			log.Printf("federation: carve ego graph for %q in %q: %v", entity, alias, err)
		}
		return nil
	}

	return &FederatedContext{
		Alias:     alias,
		Entity:    entity,
		NodeCount: len(sub.Nodes),
		Nodes:     sub.Nodes,
		Edges:     sub.Edges,
	}
}

// SearchEpisodes queries sibling stores' episodes tables using FTS5 search.
// Results are labeled with their source alias. If aliases is nil or empty,
// all siblings are searched. Errors on individual siblings are silently skipped.
// BUG-023: queries run in parallel via errgroup for O(1) latency.
func (s *CrossProjectSearch) SearchEpisodes(ctx context.Context, query string, aliases []string, limit int) []FederatedEpisode {
	if limit <= 0 {
		limit = 5
	}

	targets := s.resolver.filterEntries(aliases)

	var mu sync.Mutex
	var results []FederatedEpisode

	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(8) // bound parallelism to avoid overwhelming sibling stores
	for _, e := range targets {
		e := e
		eg.Go(func() error {
			st := s.resolver.getStore(e.Alias)
			if st == nil {
				return nil
			}

			// Check if the sibling store has the episodes_fts table.
			// Older stores might not have episodic memory tables.
			if !s.hasEpisodesTable(st) {
				return nil
			}

			episodes, err := st.RecallEpisodesCtx(egctx, query, "", "", "", "", limit, 0)
			if err != nil {
				log.Printf("federation: search episodes in %q: %v", e.Alias, err)
				return nil
			}

			mu.Lock()
			for _, ep := range episodes {
				results = append(results, FederatedEpisode{
					Alias:   e.Alias,
					Episode: ep,
				})
			}
			mu.Unlock()
			return nil
		})
	}
	_ = eg.Wait()
	return results
}

// SearchMemoriesForEntity searches sibling stores for episodic memories
// related to a specific entity. Uses graph-anchored search (node ID in
// affected_nodes) as the primary path — more precise than text matching.
// Falls back to FTS text search if no node ID is found.
// Returns 1-line hints for prepare_context. At most 3 hints per sibling.
// BUG-023: queries run in parallel via errgroup for O(1) latency.
func (s *CrossProjectSearch) SearchMemoriesForEntity(ctx context.Context, entityName string, aliases []string) []FederatedMemoryHint {
	targets := s.resolver.filterEntries(aliases)

	var mu sync.Mutex
	var hints []FederatedMemoryHint

	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(8) // bound parallelism to avoid overwhelming sibling stores
	for _, e := range targets {
		e := e
		eg.Go(func() error {
			st := s.resolver.getStore(e.Alias)
			if st == nil {
				return nil
			}
			if !s.hasEpisodesTable(st) {
				return nil
			}

			// Primary: graph-anchored search via node ID in affected_nodes.
			// This is precise — finds only memories explicitly linked to the entity,
			// not just any memory that mentions the name in text.
			var episodes []store.Episode
			nodes, err := st.FindNodesByNameCtx(egctx, entityName, 1)
			if err == nil && len(nodes) > 0 {
				episodes, _ = st.FindEpisodesByNodeID(nodes[0].ID, 3)
			}

			// Fallback: FTS text search on entity name.
			// Used when the entity has no node in the sibling store (e.g., removed
			// entity with memories still referencing it by name).
			if len(episodes) == 0 {
				episodes, _ = st.RecallEpisodesCtx(egctx, entityName, "", "", "", "", 3, 0)
			}

			mu.Lock()
			for _, ep := range episodes {
				summary := ep.Decision
				if len(summary) > 120 {
					summary = summary[:117] + "..."
				}
				hints = append(hints, FederatedMemoryHint{
					Alias:   e.Alias,
					Summary: summary,
					Query:   entityName,
				})
			}
			mu.Unlock()
			return nil
		})
	}
	_ = eg.Wait()
	return hints
}

// hasEpisodesTable checks if a sibling store has the episodes and
// episodes_fts tables required for cross-project memory search.
// Uses sqlite_master introspection — no probe queries, no side effects.
func (s *CrossProjectSearch) hasEpisodesTable(st *store.Store) bool {
	return st.HasTable("episodes") && st.HasTable("episodes_fts")
}
