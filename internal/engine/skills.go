package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
)

// SkillResult holds the output of a skill execution.
type SkillResult struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"` // "success", "error"
	Output   string        `json:"output"`
	Duration time.Duration `json:"duration"`
}

// ExecuteSkill runs a skill from the library. A skill is a directory containing:
//   - SKILL.md - the procedure (markdown with instructions for the LLM)
//   - *.sh - optional helper scripts the LLM can reference
//
// The LLM reads the procedure and generates the execution steps, which
// are then run as shell commands. Inputs are substituted into the procedure.
func ExecuteSkill(ctx context.Context, client *llm.Client, skillDir string, inputs map[string]string) (*SkillResult, error) {
	start := time.Now()
	result := &SkillResult{
		Name:   filepath.Base(skillDir),
		Status: "success",
	}

	// Read SKILL.md
	procedurePath := filepath.Join(skillDir, "SKILL.md")
	procedureData, err := os.ReadFile(procedurePath)
	if err != nil {
		return nil, fmt.Errorf("reading skill procedure: %w", err)
	}
	procedure := string(procedureData)

	// Substitute inputs into the procedure
	for key, value := range inputs {
		procedure = strings.ReplaceAll(procedure, "{{"+key+"}}", value)
	}

	// List available helper scripts
	entries, _ := os.ReadDir(skillDir)
	var helpers []string
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".sh") || strings.HasSuffix(e.Name(), ".py")) {
			helpers = append(helpers, e.Name())
		}
	}

	// Build the LLM prompt to execute the skill
	systemPrompt := `You are executing a skill procedure. Read the procedure below and produce the exact shell commands to run.
Return ONLY the commands, one per line. No explanation, no markdown fences.
Each command will be executed in sequence. If a command fails, execution stops.
The working directory is the skill directory.`

	userPrompt := fmt.Sprintf("## Procedure\n\n%s", procedure)
	if len(helpers) > 0 {
		userPrompt += fmt.Sprintf("\n\n## Available scripts: %s", strings.Join(helpers, ", "))
	}
	if len(inputs) > 0 {
		userPrompt += "\n\n## Inputs\n"
		for k, v := range inputs {
			userPrompt += fmt.Sprintf("- %s: %s\n", k, v)
		}
	}

	// Get commands from LLM
	commands, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("skill planning: %w", err)
	}

	ui.PrintVerbose("Skill", fmt.Sprintf("executing %d commands", len(strings.Split(strings.TrimSpace(commands), "\n"))))

	// Execute each command
	var output strings.Builder
	for _, line := range strings.Split(commands, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		ui.PrintVerbose("Skill cmd", line)

		res, err := execx.RunCombined(ctx, "sh", []string{"-c", line}, execx.RunOpts{Dir: skillDir})
		output.WriteString(fmt.Sprintf("$ %s\n%s\n", line, string(res.Combined)))

		if err != nil {
			result.Status = "error"
			output.WriteString(fmt.Sprintf("Error: %s\n", err))
			break
		}
	}

	result.Output = output.String()
	result.Duration = time.Since(start)
	return result, nil
}
