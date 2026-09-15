package engine

// ResolvedEntity is an entity with additional resolved identifiers.
type ResolvedEntity struct {
	Raw      string
	Type     EntityType
	Resolved map[string]string // additional resolved fields (resource_ari, checkout_ari, etc.)
}

// ResolvedContext is the output of the resolver step.
type ResolvedContext struct {
	Entities    []ResolvedEntity
	Preliminary map[string]interface{} // data from resolve-step queries
}

// Resolve is Step 3 of the pipeline: deterministic entity resolution.
// It types each classified entity and passes its extracted identifiers
// through for the planner and prompt builders. Entity-to-source affinity
// is expressed by sources' `entities:` token lists and `routes.yaml`
// `match_entity` routes, not resolved here.
func Resolve(classified *ClassifiedIntent) *ResolvedContext {
	resolved := &ResolvedContext{
		Entities:    make([]ResolvedEntity, 0, len(classified.Entities)),
		Preliminary: make(map[string]interface{}),
	}

	for _, entity := range classified.Entities {
		resolved.Entities = append(resolved.Entities, resolveEntity(entity))
	}

	return resolved
}

// resolveEntity flattens a typed entity's metadata into the Resolved map.
func resolveEntity(entity TypedEntity) ResolvedEntity {
	re := ResolvedEntity{
		Raw:      entity.Raw,
		Type:     entity.Type,
		Resolved: make(map[string]string),
	}

	// Copy metadata from classified entity
	for k, v := range entity.Metadata {
		re.Resolved[k] = v
	}

	if entity.Type == EntityExternalOrderID {
		re.Resolved["external_order_id"] = entity.Raw
	}

	return re
}

// GetResolvedValues returns a flattened map of all resolved values for LLM prompts.
func (rc *ResolvedContext) GetResolvedValues() map[string]string {
	values := make(map[string]string)
	for _, entity := range rc.Entities {
		for k, v := range entity.Resolved {
			values[k] = v
		}
	}
	return values
}
