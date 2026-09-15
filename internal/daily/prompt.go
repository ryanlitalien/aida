// Package daily renders the profile-aware prompt for `aida daily`.
//
// The prompt template is embedded at build time from prompt.tmpl.md.
// Each profile's daily: config in ~/.aida/config.yaml feeds the template
// vars (project_dir, email_to, gmail_labels, etc).
package daily

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/ryanlitalien/aida/internal/config"
)

//go:embed prompt.tmpl.md
var promptTemplate string

//go:embed backup.tmpl.md
var backupTemplate string

// RenderPrompt returns the daily briefing prompt with values from dc
// substituted in. Returns an error if the template fails to parse or execute,
// or if required fields are missing.
func RenderPrompt(dc *config.DailyConfig) (string, error) {
	if dc == nil {
		return "", fmt.Errorf("daily config is nil")
	}
	if dc.ProjectDir == "" {
		return "", fmt.Errorf("daily.project_dir is required")
	}
	if dc.EmailTo == "" {
		return "", fmt.Errorf("daily.email_to is required")
	}
	if dc.TaskTag == "" {
		return "", fmt.Errorf("daily.task_tag is required")
	}

	tmpl, err := template.New("daily").Parse(promptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, dc); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// RenderBackupPrompt returns the Notion-meeting-backup-only prompt for
// `aida daily --notion-backup`. Only requires ProjectDir; email/task settings
// are irrelevant for the archival job.
func RenderBackupPrompt(dc *config.DailyConfig) (string, error) {
	if dc == nil {
		return "", fmt.Errorf("daily config is nil")
	}
	if dc.ProjectDir == "" {
		return "", fmt.Errorf("daily.project_dir is required")
	}

	tmpl, err := template.New("backup").Parse(backupTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, dc); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}
