package engine

import (
	"testing"
)

func TestClassifyEntity_GenericARI(t *testing.T) {
	tests := []struct {
		input    string
		expected EntityType
	}{
		{"DKYAYQ3S195JMSND", EntityGenericARI},
		{"PE29GD8AFPK054NZ", EntityGenericARI},
		{"VB9OOAEDOECVE6PR", EntityGenericARI},
		{"5N8I4P40MGG1W103", EntityGenericARI},
		{"DGDZVGPMM82I1LR3", EntityGenericARI},
		{"8DUDD467KD6FLEEP", EntityGenericARI},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			entity := classifyEntity(tt.input)
			if entity.Type != tt.expected {
				t.Errorf("classifyEntity(%q) = %v, want %v", tt.input, entity.Type, tt.expected)
			}
		})
	}
}

func TestClassifyEntity_SpecificPatterns(t *testing.T) {
	tests := []struct {
		input    string
		expected EntityType
	}{
		{"1234-5678-ABCD", EntityUserID},
		{"ABCD-1234", EntityChargeID},
		{"TK-ABCDEF1234567", EntityToken},
		{"CS-ABCD-1234", EntityCaseID},
		{"ORDR-ABCD-1234", EntityOrderID},
		{"PY-ABCDEF-12", EntityPaymentID},
		{"PYR-ABCDEF-12", EntityPaymentReversal},
		{"I-ABCDEFGHIJKLMN", EntityInvoiceID},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			entity := classifyEntity(tt.input)
			if entity.Type != tt.expected {
				t.Errorf("classifyEntity(%q) = %v, want %v", tt.input, entity.Type, tt.expected)
			}
		})
	}
}

func TestClassifyEntity_URL(t *testing.T) {
	url := "https://www.checkout.example/products/checkout?public_api_key=5N8I4P40MGG1W103&checkout_ari=VB9OOAEDOECVE6PR&locale=en_US"
	entity := classifyEntity(url)

	if entity.Type != EntityURL {
		t.Errorf("expected EntityURL, got %v", entity.Type)
	}
	if entity.Metadata["public_api_key"] != "5N8I4P40MGG1W103" {
		t.Errorf("expected public_api_key=5N8I4P40MGG1W103, got %q", entity.Metadata["public_api_key"])
	}
	if entity.Metadata["checkout_ari"] != "VB9OOAEDOECVE6PR" {
		t.Errorf("expected checkout_ari=VB9OOAEDOECVE6PR, got %q", entity.Metadata["checkout_ari"])
	}
}

func TestClassifyEntity_PartnerName(t *testing.T) {
	tests := []struct {
		input    string
		expected EntityType
	}{
		{"thrive", EntityPartnerName},
		// Note: "viewer-core" (with a dash) is deliberately NOT used here --
		// it happens to also fit checkoutTokenPattern's "2-6 alnum, dash,
		// 2-6 alnum" shape, so classifyEntity would tag it as
		// EntityCheckoutToken instead (checked before isPartnerName). "Viewer
		// Core" (space-separated) exercises the same partner-name heuristic
		// without that accidental collision.
		{"Viewer Core", EntityPartnerName},
		{"Openscreen", EntityPartnerName},
		{"Google Pay", EntityPartnerName},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			entity := classifyEntity(tt.input)
			if entity.Type != tt.expected {
				t.Errorf("classifyEntity(%q) = %v, want %v", tt.input, entity.Type, tt.expected)
			}
		})
	}
}

func TestSelectStrategy(t *testing.T) {
	tests := []struct {
		action   string
		entities []TypedEntity
		expected Strategy
	}{
		{"investigate", nil, StrategyInvestigate},
		{"query", nil, StrategyQuery},
		{"record", nil, StrategyRecord},
		{"test", nil, StrategyExecute},
		{"search", nil, StrategySearch},
		{"lookup", []TypedEntity{{Type: EntityGenericARI}}, StrategyLookup},
		{"lookup", nil, StrategyQuery}, // no specific ARI → query instead
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			intent := &Intent{Action: tt.action}
			strategy := selectStrategy(intent, tt.entities)
			if strategy != tt.expected {
				t.Errorf("selectStrategy(%q) = %v, want %v", tt.action, strategy, tt.expected)
			}
		})
	}
}

func TestClassify_FullFlow(t *testing.T) {
	intent := &Intent{
		RawQuery:    "what happened with this checkout? VB9OOAEDOECVE6PR",
		RawEntities: []string{"VB9OOAEDOECVE6PR"},
		Action:      "investigate",
		Keywords:    []string{"checkout"},
	}

	classified := Classify(intent)

	if classified.Strategy != StrategyInvestigate {
		t.Errorf("expected StrategyInvestigate, got %v", classified.Strategy)
	}
	if len(classified.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(classified.Entities))
	}
	if classified.Entities[0].Type != EntityGenericARI {
		t.Errorf("expected EntityGenericARI, got %v", classified.Entities[0].Type)
	}
}

func TestClassify_URLWithARIs(t *testing.T) {
	intent := &Intent{
		RawQuery: "What happened here? https://www.checkout.example/products/checkout?public_api_key=5N8I4P40MGG1W103&checkout_ari=VB9OOAEDOECVE6PR",
		RawEntities: []string{
			"https://www.checkout.example/products/checkout?public_api_key=5N8I4P40MGG1W103&checkout_ari=VB9OOAEDOECVE6PR",
		},
		Action:   "investigate",
		Keywords: []string{"checkout"},
	}

	classified := Classify(intent)

	if classified.Strategy != StrategyInvestigate {
		t.Errorf("expected StrategyInvestigate, got %v", classified.Strategy)
	}
	if len(classified.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(classified.Entities))
	}
	entity := classified.Entities[0]
	if entity.Type != EntityURL {
		t.Errorf("expected EntityURL, got %v", entity.Type)
	}
	if entity.Metadata["public_api_key"] != "5N8I4P40MGG1W103" {
		t.Errorf("expected public_api_key=5N8I4P40MGG1W103, got %q", entity.Metadata["public_api_key"])
	}
}

func TestClassifyEntity_ExternalOrderID(t *testing.T) {
	tests := []struct {
		input    string
		expected EntityType
	}{
		{"P01-8823461-7734182", EntityExternalOrderID},
		{"P01-4471290-9938156", EntityExternalOrderID},
		{"P01-5512837-6629401", EntityExternalOrderID},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			entity := classifyEntity(tt.input)
			if entity.Type != tt.expected {
				t.Errorf("classifyEntity(%q) = %v, want %v", tt.input, entity.Type, tt.expected)
			}
		})
	}
}

func TestClassify_P01RefundInvestigation(t *testing.T) {
	intent := &Intent{
		RawQuery:    "can you help look into the refund status on these two bookings? P01-8823461-7734182 P01-4471290-9938156",
		RawEntities: []string{"P01-8823461-7734182", "P01-4471290-9938156"},
		Action:      "investigate",
		Keywords:    []string{"refund status", "bookings"},
	}

	classified := Classify(intent)

	if classified.Strategy != StrategyInvestigate {
		t.Errorf("expected StrategyInvestigate, got %v", classified.Strategy)
	}
	if len(classified.Entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(classified.Entities))
	}
	for _, e := range classified.Entities {
		if e.Type != EntityExternalOrderID {
			t.Errorf("expected EntityExternalOrderID, got %v for %q", e.Type, e.Raw)
		}
	}
}

func TestClassify_ACHDepositQuery(t *testing.T) {
	// Real-world test: "what was the latest ACH deposit for thrive game and what was the ID?"
	// LLM parses this as action=query with entity="thrive game" and timeframe="latest"
	intent := &Intent{
		RawQuery:    "what was the latest ACH deposit for thrive game and what was the ID?",
		RawEntities: []string{"thrive game"},
		Action:      "query",
		Keywords:    []string{"ACH deposit", "thrive game", "latest", "ID"},
		Timeframe:   "latest",
	}

	classified := Classify(intent)

	if classified.Strategy != StrategyQuery {
		t.Errorf("expected StrategyQuery, got %v", classified.Strategy)
	}
	if len(classified.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(classified.Entities))
	}
	if classified.Entities[0].Type != EntityPartnerName {
		t.Errorf("expected EntityPartnerName, got %v", classified.Entities[0].Type)
	}
}

func TestPromoteTaskIntent(t *testing.T) {
	tests := []struct {
		name           string
		intent         Intent
		wantAction     string
		wantTaskAction string
	}{
		{
			name:           "lookup with #N entity is promoted",
			intent:         Intent{Action: "lookup", RawEntities: []string{"#117"}},
			wantAction:     "task",
			wantTaskAction: "lookup",
		},
		{
			name:           "query with #N entity is promoted",
			intent:         Intent{Action: "query", RawEntities: []string{"#42"}},
			wantAction:     "task",
			wantTaskAction: "lookup",
		},
		{
			name:           "already-task intent is left alone",
			intent:         Intent{Action: "task", TaskAction: "list", RawEntities: []string{"#117"}},
			wantAction:     "task",
			wantTaskAction: "list",
		},
		{
			name:           "lookup without #N is left alone",
			intent:         Intent{Action: "lookup", RawEntities: []string{"first-chair"}},
			wantAction:     "lookup",
			wantTaskAction: "",
		},
		{
			name:           "non-#N tokens that look numeric are left alone",
			intent:         Intent{Action: "lookup", RawEntities: []string{"117", "#abc", "foo#117"}},
			wantAction:     "lookup",
			wantTaskAction: "",
		},
		{
			name:           "#N alongside other entities still promotes",
			intent:         Intent{Action: "lookup", RawEntities: []string{"first-chair", "#117"}},
			wantAction:     "task",
			wantTaskAction: "lookup",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := tt.intent
			PromoteTaskIntent(&i)
			if i.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", i.Action, tt.wantAction)
			}
			if i.TaskAction != tt.wantTaskAction {
				t.Errorf("TaskAction = %q, want %q", i.TaskAction, tt.wantTaskAction)
			}
		})
	}
}
