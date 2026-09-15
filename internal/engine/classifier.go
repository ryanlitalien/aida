package engine

import (
	"net/url"
	"regexp"
	"strings"
)

// Strategy represents how the query should be executed.
type Strategy string

const (
	StrategyLookup      Strategy = "lookup"      // simple: one source
	StrategyQuery       Strategy = "query"       // fan-out: parallel sources
	StrategyInvestigate Strategy = "investigate" // chain: resolve → diagnose → contextualize
	StrategyRecord      Strategy = "record"      // write: append to a source
	StrategyExecute     Strategy = "execute"     // run: start a tool
	StrategySearch      Strategy = "search"      // search: grep across sources
	StrategyAgent       Strategy = "agent"       // autonomous: LLM-driven tool loop
)

// EntityType identifies what kind of entity was found.
type EntityType string

const (
	EntityGenericARI      EntityType = "generic_ari"
	EntityUserID          EntityType = "user_id"
	EntityChargeID        EntityType = "charge_id"
	EntityToken           EntityType = "token"
	EntityCaseID          EntityType = "case_id"
	EntityOrderID         EntityType = "order_id"
	EntityPaymentID       EntityType = "payment_id"
	EntityPaymentReversal EntityType = "payment_reversal"
	EntityInvoiceID       EntityType = "invoice_id"
	EntityExternalOrderID EntityType = "external_order_id"
	EntityURL             EntityType = "url"
	EntityPartnerName     EntityType = "partner_name"
	EntityUnknown         EntityType = "unknown"
)

// taskIDPattern matches `#N` task IDs in raw_entities. Used by
// PromoteTaskIntent to recognize task-lookup queries even when the parser
// missed setting Action="task".
var taskIDPattern = regexp.MustCompile(`^#\d+$`)

// PromoteTaskIntent is a deterministic post-parse normalizer: if the parser
// returned any `#N` token in raw_entities but didn't classify the query as
// a task action, promote it to action=task / task_action=lookup. The parser
// is an LLM and occasionally drifts on these - `#N` literals are unambiguous
// so it's safe to override.
//
// Mutates the intent in place. No-op when the intent is already a task
// action or no `#N` token is present.
func PromoteTaskIntent(intent *Intent) {
	if intent == nil || intent.Action == "task" {
		return
	}
	for _, raw := range intent.RawEntities {
		if taskIDPattern.MatchString(strings.TrimSpace(raw)) {
			intent.Action = "task"
			intent.TaskAction = "lookup"
			return
		}
	}
}

// TypedEntity is an entity with its classified type.
type TypedEntity struct {
	Raw      string
	Type     EntityType
	Metadata map[string]string // extracted URL params, etc.
}

// ClassifiedIntent is the output of the classifier step.
type ClassifiedIntent struct {
	Intent   *Intent
	Entities []TypedEntity
	Strategy Strategy
}

// ARI pattern matchers.
var ariPatterns = map[EntityType]*regexp.Regexp{
	EntityGenericARI:      regexp.MustCompile(`^[A-Z0-9]{16}$`),
	EntityExternalOrderID: regexp.MustCompile(`^P\d{2}-\d{7}-\d{7}$`),
	EntityUserID:          regexp.MustCompile(`^\d{4}-\d{4}-[A-Z]{4}$`),
	EntityChargeID:        regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`),
	EntityToken:           regexp.MustCompile(`^TK-[A-Z0-9]{13}$`),
	EntityCaseID:          regexp.MustCompile(`^CS-[A-Z0-9]{4}-[A-Z0-9]{4}$`),
	EntityOrderID:         regexp.MustCompile(`^ORDR-[A-Z0-9]{4}-[A-Z0-9]{4}$`),
	EntityPaymentID:       regexp.MustCompile(`^PY-[A-Z0-9]{6}-[A-Z0-9]{2}$`),
	EntityPaymentReversal: regexp.MustCompile(`^PYR-[A-Z0-9]{6}-[A-Z0-9]{2}$`),
	EntityInvoiceID:       regexp.MustCompile(`^I-[A-Z0-9]{14}$`),
}

// urlPattern matches HTTP/HTTPS URLs.
var urlPattern = regexp.MustCompile(`^https?://`)

// Classify is Step 2 of the pipeline: deterministic ARI pattern matching and strategy selection.
func Classify(intent *Intent) *ClassifiedIntent {
	classified := &ClassifiedIntent{
		Intent:   intent,
		Entities: classifyEntities(intent.RawEntities),
	}
	classified.Strategy = selectStrategy(intent, classified.Entities)
	return classified
}

// classifyEntities identifies the type of each raw entity.
func classifyEntities(rawEntities []string) []TypedEntity {
	var entities []TypedEntity
	for _, raw := range rawEntities {
		entity := classifyEntity(raw)
		entities = append(entities, entity)
	}
	return entities
}

// classifyEntity determines the type of a single entity string.
func classifyEntity(raw string) TypedEntity {
	trimmed := strings.TrimSpace(raw)

	// Check for URL first (may contain embedded ARIs)
	if urlPattern.MatchString(trimmed) {
		return classifyURL(trimmed)
	}

	// Check ARI patterns (most specific first)
	upper := strings.ToUpper(trimmed)
	for entityType, pattern := range ariPatterns {
		if entityType == EntityGenericARI {
			continue // check last (most general)
		}
		if pattern.MatchString(upper) {
			return TypedEntity{Raw: trimmed, Type: entityType}
		}
	}

	// Generic 16-char ARI
	if ariPatterns[EntityGenericARI].MatchString(upper) {
		return TypedEntity{Raw: trimmed, Type: EntityGenericARI}
	}

	// Likely a partner/entity name
	if isPartnerName(trimmed) {
		return TypedEntity{Raw: trimmed, Type: EntityPartnerName}
	}

	return TypedEntity{Raw: trimmed, Type: EntityUnknown}
}

// classifyURL parses a URL and extracts known query parameters as metadata.
func classifyURL(rawURL string) TypedEntity {
	entity := TypedEntity{
		Raw:      rawURL,
		Type:     EntityURL,
		Metadata: make(map[string]string),
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return entity
	}

	// Extract known partner-checkout URL parameters
	knownParams := []string{
		"public_api_key", "checkout_ari", "resource_ari",
		"locale", "country_code", "order_id",
	}
	for _, param := range knownParams {
		if val := parsed.Query().Get(param); val != "" {
			entity.Metadata[param] = val
		}
	}

	// Classify any extracted ARIs
	if apiKey := entity.Metadata["public_api_key"]; apiKey != "" {
		if ariPatterns[EntityGenericARI].MatchString(strings.ToUpper(apiKey)) {
			entity.Metadata["public_api_key_type"] = "generic_ari"
		}
	}
	if checkoutARI := entity.Metadata["checkout_ari"]; checkoutARI != "" {
		if ariPatterns[EntityGenericARI].MatchString(strings.ToUpper(checkoutARI)) {
			entity.Metadata["checkout_ari_type"] = "generic_ari"
		}
	}

	return entity
}

// isPartnerName heuristically determines if a string is a partner/company name.
func isPartnerName(s string) bool {
	// Not an ID pattern, contains letters, reasonable length
	if len(s) > 50 || len(s) < 2 {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
			break
		}
	}
	return hasLetter
}

// selectStrategy picks the execution strategy based on action and entity types.
func selectStrategy(intent *Intent, entities []TypedEntity) Strategy {
	switch intent.Action {
	case "investigate":
		return StrategyInvestigate
	case "record":
		return StrategyRecord
	case "test":
		return StrategyExecute
	case "search":
		return StrategySearch
	case "agent":
		return StrategyAgent
	case "lookup":
		// If we have specific ARIs, it's a simple lookup
		for _, e := range entities {
			switch e.Type {
			case EntityGenericARI, EntityUserID, EntityChargeID,
				EntityToken, EntityCaseID, EntityOrderID,
				EntityPaymentID, EntityPaymentReversal, EntityInvoiceID:
				return StrategyLookup
			}
		}
		return StrategyQuery
	case "query":
		return StrategyQuery
	default:
		// URL with embedded ARIs → investigate
		for _, e := range entities {
			if e.Type == EntityURL && len(e.Metadata) > 0 {
				return StrategyInvestigate
			}
		}
		return StrategyQuery
	}
}
