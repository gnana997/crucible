package tui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// Palette — "cold steel, hot metal": the ember accent is the live sandboxes
// (the code being forged, the thing your eye lands on), frozen blue is
// snapshots, steel neutrals are structure/chrome, and red/green stay reserved
// for outcomes. Hierarchy reads by temperature: ember advances, steel sits, ice
// recedes. AdaptiveColor rows render correctly on both light and dark terminals.
var (
	colAccent    = lipgloss.AdaptiveColor{Light: "#C25A16", Dark: "#F2913D"} // ember / brand
	colAccentAlt = lipgloss.AdaptiveColor{Light: "#0E7C8C", Dark: "#56C6D6"} // cyan / live data
	colTitleFg   = lipgloss.Color("#17130E")                                 // near-black warm, on ember
	colTitleBg   = lipgloss.Color("#F2913D")                                 // ember chip
	colSandbox   = lipgloss.AdaptiveColor{Light: "#C55A12", Dark: "#F5A65A"} // the primary entity
	colSnapshot  = lipgloss.AdaptiveColor{Light: "#2E6E9E", Dark: "#6FA8CE"} // frozen blue
	colTreeLine  = lipgloss.AdaptiveColor{Light: "#B7BFC9", Dark: "#47525F"} // dim steel connectors
	colText      = lipgloss.AdaptiveColor{Light: "#23282F", Dark: "#C6D0DB"}
	colMeta      = lipgloss.AdaptiveColor{Light: "#586372", Dark: "#8792A1"}
	colFaint     = lipgloss.AdaptiveColor{Light: "#8A94A2", Dark: "#59636F"}
	colSelFg     = lipgloss.AdaptiveColor{Light: "#1B2129", Dark: "#EAF0F5"}
	colSelBg     = lipgloss.AdaptiveColor{Light: "#E3E9EF", Dark: "#223140"} // cool steel highlight
	colHeaderRow = lipgloss.AdaptiveColor{Light: "#4A6D77", Dark: "#86A9B4"}
	colBorder    = lipgloss.AdaptiveColor{Light: "#CAD0D8", Dark: "#333B46"}
	colSuccess   = lipgloss.AdaptiveColor{Light: "#2E8B44", Dark: "#6FD08A"}
	colError     = lipgloss.AdaptiveColor{Light: "#C23B3B", Dark: "#F06D6D"}
	colWarn      = lipgloss.AdaptiveColor{Light: "#9E7016", Dark: "#E8C15A"} // caution gold, yellower than ember
	colStderr    = lipgloss.AdaptiveColor{Light: "#B4522E", Dark: "#E39B7B"} // terracotta "other channel"
)

// Semantic styles. Node ids are the only thing at full weight on any line;
// structure and meta recede.
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(colTitleFg).Background(colTitleBg).Padding(0, 1)
	metaStyle     = lipgloss.NewStyle().Foreground(colMeta)
	helpStyle     = lipgloss.NewStyle().Foreground(colFaint)
	errStyle      = lipgloss.NewStyle().Foreground(colError).Bold(true)
	okStyle       = lipgloss.NewStyle().Foreground(colSuccess).Bold(true)
	warnStyle     = lipgloss.NewStyle().Foreground(colWarn).Bold(true)
	disabledStyle = lipgloss.NewStyle().Foreground(colFaint).Strikethrough(true) // an action the scope forbids
	altStyle      = lipgloss.NewStyle().Foreground(colAccentAlt)
	promptStyle   = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	stderrStyle   = lipgloss.NewStyle().Foreground(colStderr)
	dividerStyle  = lipgloss.NewStyle().Foreground(colBorder)
	sbxNodeStyle  = lipgloss.NewStyle().Foreground(colSandbox).Bold(true)    // live sandbox advances
	snapNodeStyle = lipgloss.NewStyle().Foreground(colSnapshot).Italic(true) // frozen snapshot recedes
	treeStyle     = lipgloss.NewStyle().Foreground(colTreeLine)
	exitOKChip    = lipgloss.NewStyle().Bold(true).Foreground(colTitleFg).Background(colSuccess).Padding(0, 1)
	exitBadChip   = lipgloss.NewStyle().Bold(true).Foreground(colTitleFg).Background(colError).Padding(0, 1)
)

// newTableStyles applies the theme to a bubbles table: cool steel selection (so
// it never clashes with the ember sandbox ids), steel-cyan bold headers.
func newTableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.Foreground(colHeaderRow).Bold(true).BorderBottom(true).BorderForeground(colBorder)
	s.Selected = s.Selected.Foreground(colSelFg).Background(colSelBg).Bold(true)
	s.Cell = s.Cell.Foreground(colText)
	return s
}
