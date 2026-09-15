package ui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Ensure termenv is used so the import is not flagged as unused.
// HasDarkBackground can be used elsewhere to adapt output.
var hasDarkBG = termenv.HasDarkBackground()

// Styles for terminal output. AdaptiveColor picks the right shade
// depending on whether the terminal background is light or dark.
var (
	// Headers & labels
	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#fafafa"})

	LabelStyle = lipgloss.NewStyle().
			Faint(true)

	ValueStyle = lipgloss.NewStyle()

	// Status indicators
	SuccessStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#1e7a1e", Dark: "#73e673"})

	ErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#cc0000", Dark: "#ff6666"}).
			Bold(true)

	WarnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#b38600", Dark: "#ffd700"})

	DimStyle = lipgloss.NewStyle().
			Faint(true)

	// Source display
	SourceStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#0077b6", Dark: "#61dafb"})

	ScoreStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#8b008b", Dark: "#e680ff"})
)

// Unicode prefixes used as lightweight icons in CLI output.
const (
	ParseIcon   = "\U0001F50D"   // magnifying glass
	ProfileIcon = "\U0001F4C2"   // open file folder
	RouteIcon   = "\U0001F500"   // shuffle arrows
	SpinIcon    = "\u23F3"       // hourglass
	SuccessIcon = "\u2705"       // check mark
	ErrorIcon   = "\u274C"       // cross mark
	WarnIcon    = "\u26A0\uFE0F" // warning sign
)
