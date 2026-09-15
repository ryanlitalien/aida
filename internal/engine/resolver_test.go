package engine

import "testing"

func TestResolve_ExternalOrderIDPassthrough(t *testing.T) {
	classified := &ClassifiedIntent{
		Intent: &Intent{
			RawQuery:    "refund status on P01-8823461-7734182",
			RawEntities: []string{"P01-8823461-7734182"},
			Action:      "investigate",
			Keywords:    []string{"refund status", "bookings"},
		},
		Entities: []TypedEntity{
			{Raw: "P01-8823461-7734182", Type: EntityExternalOrderID},
		},
		Strategy: StrategyInvestigate,
	}

	resolved := Resolve(classified)

	if len(resolved.Entities) != 1 {
		t.Fatalf("expected 1 resolved entity, got %d", len(resolved.Entities))
	}
	e := resolved.Entities[0]
	if e.Resolved["external_order_id"] != e.Raw {
		t.Errorf("expected external_order_id=%q, got %q", e.Raw, e.Resolved["external_order_id"])
	}
}

func TestResolve_MetadataCopiedThrough(t *testing.T) {
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:   "lookup",
			Keywords: []string{},
		},
		Entities: []TypedEntity{
			{
				Raw:      "DKYAYQ3S195JMSND",
				Type:     EntityGenericARI,
				Metadata: map[string]string{"source_hint": "resource_ari"},
			},
		},
		Strategy: StrategyLookup,
	}

	resolved := Resolve(classified)

	if len(resolved.Entities) != 1 {
		t.Fatalf("expected 1 resolved entity, got %d", len(resolved.Entities))
	}
	if got := resolved.Entities[0].Resolved["source_hint"]; got != "resource_ari" {
		t.Errorf("expected metadata to carry through, got %q", got)
	}

	values := resolved.GetResolvedValues()
	if values["source_hint"] != "resource_ari" {
		t.Errorf("expected GetResolvedValues to flatten metadata, got %q", values["source_hint"])
	}
}

func TestResolve_EmptyEntities(t *testing.T) {
	classified := &ClassifiedIntent{
		Intent:   &Intent{Action: "query", Keywords: []string{}},
		Entities: []TypedEntity{},
		Strategy: StrategyQuery,
	}

	resolved := Resolve(classified)

	if len(resolved.Entities) != 0 {
		t.Errorf("expected no resolved entities, got %d", len(resolved.Entities))
	}
	if values := resolved.GetResolvedValues(); len(values) != 0 {
		t.Errorf("expected no resolved values, got %v", values)
	}
}
