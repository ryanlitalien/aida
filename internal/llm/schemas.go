package llm

// IntentSchema returns the JSON schema for structured intent parsing output.
// This schema is used with CompleteJSON to enforce that the LLM returns
// a well-formed intent object.
func IntentSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"raw_entities": map[string]interface{}{
				"type":        "array",
				"description": "Any identifiers, names, ARIs, URLs, or API keys found in the query",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"timeframe": map[string]interface{}{
				"type":        []interface{}{"string", "null"},
				"description": "Relative time reference like 'yesterday', '1h', 'last week', or null",
			},
			"action": map[string]interface{}{
				"type":        "string",
				"description": "The classified action type for this query",
				"enum": []interface{}{
					"investigate",
					"query",
					"lookup",
					"record",
					"test",
					"search",
					"task",
					"agent",
				},
			},
			"task_action": map[string]interface{}{
				"type":        []interface{}{"string", "null"},
				"description": "If action is task: the specific task operation (create, list, done, lookup), or null",
			},
			"task_title": map[string]interface{}{
				"type":        []interface{}{"string", "null"},
				"description": "If creating a task: the task title/description, or null",
			},
			"keywords": map[string]interface{}{
				"type":        "array",
				"description": "Relevant search terms extracted from the query",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"amount": map[string]interface{}{
				"type":        []interface{}{"number", "null"},
				"description": "A numeric amount if present in the query, or null",
			},
			"category": map[string]interface{}{
				"type":        []interface{}{"string", "null"},
				"description": "Category if applicable (e.g., food, transport), or null",
			},
			"comprehensive_intent": map[string]interface{}{
				"type":        "boolean",
				"description": "True when the user asks for ALL/EVERY/COMPLETE results, not just the primary one. E.g., 'all ARIs', 'every merchant', 'complete list of', 'list all'.",
			},
			"effective_question": map[string]interface{}{
				"type":        []interface{}{"string", "null"},
				"description": "Set ONLY when the current question is a referential follow-up ('do the same thing, but for issues', 'what about last month') AND a PRIOR TURN block was provided. In that case, rewrite the current question so it stands alone - weave the inherited entities/scope from the prior turn into the question text so downstream stages (executor, synthesizer) have the full context without needing to know about the prior turn. Example: prior question 'what are my open PRs across acme-widgets and aida* github repos' + current 'do the same thing, but for issues' → effective_question 'what are my open issues across acme-widgets and aida* github repos'. Return null for non-referential questions.",
			},
		},
		"required": []interface{}{
			"raw_entities",
			"action",
			"keywords",
			"timeframe",
			"amount",
			"category",
			"comprehensive_intent",
		},
	}
}
