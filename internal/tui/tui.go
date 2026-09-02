package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/algonormative/lookout/internal/store"
)

// tab indices
const (
	tabOverview = iota
	tabSpans
	tabAnomalies
	tabCount
)

var tabNames = [tabCount]string{"Overview", "Spans", "Anomalies"}

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))

	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("0")).
			Background(lipgloss.Color("12")).
			Padding(0, 2)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("7")).
				Padding(0, 2)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("14"))

	statLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	statValueStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15"))

	costStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("10"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	anomalyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11"))

	selectedRowStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("237"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
)

type tickMsg time.Time

type dataMsg struct {
	stats      *store.Stats
	models     []store.ModelStats
	spans      []store.SpanRecord
	anomalies  []store.SpanRecord
}

// Model is the bubbletea model for the TUI dashboard.
type Model struct {
	store     store.Store
	width     int
	height    int
	activeTab int
	cursor    int // row cursor for spans/anomalies tables
	offset    int // scroll offset for tables

	// cached data
	stats     *store.Stats
	models    []store.ModelStats
	spans     []store.SpanRecord
	anomalies []store.SpanRecord
}

// New creates a new TUI model.
func New(s store.Store) Model {
	return Model{
		store: s,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchData(),
		tickCmd(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyTab:
			m.activeTab = (m.activeTab + 1) % tabCount
			m.cursor = 0
			m.offset = 0
		case tea.KeyShiftTab:
			m.activeTab = (m.activeTab - 1 + tabCount) % tabCount
			m.cursor = 0
			m.offset = 0
		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
				if m.cursor < m.offset {
					m.offset = m.cursor
				}
			}
		case tea.KeyDown:
			maxRows := m.tableLen() - 1
			if m.cursor < maxRows {
				m.cursor++
				visibleRows := m.visibleRows()
				if m.cursor >= m.offset+visibleRows {
					m.offset = m.cursor - visibleRows + 1
				}
			}
		case tea.KeyRunes:
			switch string(msg.Runes) {
			case "q":
				return m, tea.Quit
			case "1":
				m.activeTab = tabOverview
				m.cursor = 0
				m.offset = 0
			case "2":
				m.activeTab = tabSpans
				m.cursor = 0
				m.offset = 0
			case "3":
				m.activeTab = tabAnomalies
				m.cursor = 0
				m.offset = 0
			case "r":
				return m, m.fetchData()
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(m.fetchData(), tickCmd())

	case dataMsg:
		m.stats = msg.stats
		m.models = msg.models
		m.spans = msg.spans
		m.anomalies = msg.anomalies
	}

	return m, nil
}

func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("lookout"))
	b.WriteString("  ")

	// Tabs
	for i, name := range tabNames {
		if i == m.activeTab {
			b.WriteString(activeTabStyle.Render(name))
		} else {
			b.WriteString(inactiveTabStyle.Render(name))
		}
	}
	b.WriteString("\n\n")

	// Content
	contentHeight := m.height - 5 // title + tabs + help + padding
	switch m.activeTab {
	case tabOverview:
		b.WriteString(m.viewOverview(contentHeight))
	case tabSpans:
		b.WriteString(m.viewSpans(contentHeight))
	case tabAnomalies:
		b.WriteString(m.viewAnomalies(contentHeight))
	}

	// Help
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("tab/shift-tab: switch tabs • ↑/↓: scroll • 1-3: jump to tab • r: refresh • q: quit"))

	return b.String()
}

func (m Model) viewOverview(height int) string {
	if m.stats == nil {
		return "Loading stats..."
	}

	var b strings.Builder

	// Stats grid
	b.WriteString(headerStyle.Render("Aggregate Stats"))
	b.WriteString("\n\n")

	stats := []struct{ label, value string }{
		{"Spans", fmt.Sprintf("%d", m.stats.TotalSpans)},
		{"Traces", fmt.Sprintf("%d", m.stats.TotalTraces)},
		{"Sessions", fmt.Sprintf("%d", m.stats.TotalSessions)},
		{"AI Spans", fmt.Sprintf("%d", m.stats.AISpanCount)},
		{"Input Tokens", formatTokens(m.stats.TotalInputTokens)},
		{"Output Tokens", formatTokens(m.stats.TotalOutputTokens)},
		{"Total Cost", fmt.Sprintf("$%.4f", m.stats.TotalCost)},
		{"Errors", fmt.Sprintf("%d", m.stats.ErrorCount)},
	}

	for _, s := range stats {
		b.WriteString(statLabelStyle.Render(fmt.Sprintf("  %-16s", s.label)))
		if strings.HasPrefix(s.label, "Total Cost") {
			b.WriteString(costStyle.Render(s.value))
		} else if s.label == "Errors" && m.stats.ErrorCount > 0 {
			b.WriteString(errorStyle.Render(s.value))
		} else {
			b.WriteString(statValueStyle.Render(s.value))
		}
		b.WriteString("\n")
	}

	// Model breakdown
	if len(m.models) > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("Models"))
		b.WriteString("\n\n")

		// Header
		b.WriteString(statLabelStyle.Render(fmt.Sprintf("  %-30s %8s %10s %10s %10s %8s",
			"Model", "Calls", "In Tokens", "Out Tokens", "Cost", "Errors")))
		b.WriteString("\n")

		for _, ms := range m.models {
			costStr := fmt.Sprintf("$%.4f", ms.TotalCost)
			errStr := fmt.Sprintf("%d", ms.ErrorCount)

			line := fmt.Sprintf("  %-30s %8d %10s %10s ",
				truncate(ms.Model, 30),
				ms.SpanCount,
				formatTokens(ms.InputTokens),
				formatTokens(ms.OutputTokens),
			)
			b.WriteString(line)
			b.WriteString(costStyle.Render(fmt.Sprintf("%10s", costStr)))
			b.WriteString(" ")
			if ms.ErrorCount > 0 {
				b.WriteString(errorStyle.Render(fmt.Sprintf("%8s", errStr)))
			} else {
				b.WriteString(fmt.Sprintf("%8s", errStr))
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

func (m Model) viewSpans(height int) string {
	if len(m.spans) == 0 {
		return "No spans found."
	}

	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("Recent Spans (%d)", len(m.spans))))
	b.WriteString("\n\n")

	// Header
	hdr := fmt.Sprintf("  %-20s %-25s %8s %10s %10s %10s",
		"Time", "Name", "Duration", "Model", "Tokens", "Cost")
	b.WriteString(statLabelStyle.Render(hdr))
	b.WriteString("\n")

	visibleRows := m.visibleRows()
	if visibleRows > len(m.spans)-m.offset {
		visibleRows = len(m.spans) - m.offset
	}

	for i := m.offset; i < m.offset+visibleRows && i < len(m.spans); i++ {
		s := m.spans[i]
		t := time.Unix(0, int64(s.StartTimeUnixNano))
		dur := s.DurationSeconds()
		tokens := s.AIInputTokens + s.AIOutputTokens
		model := truncate(s.AIModel, 10)
		if model == "" {
			model = "-"
		}

		line := fmt.Sprintf("  %-20s %-25s %7.2fs %10s %10s %10s",
			t.Format("15:04:05.000"),
			truncate(s.Name, 25),
			dur,
			model,
			formatTokens(tokens),
			fmt.Sprintf("$%.4f", s.CostUSD),
		)

		if i == m.cursor {
			b.WriteString(selectedRowStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m Model) viewAnomalies(height int) string {
	if len(m.anomalies) == 0 {
		return "No anomalies detected."
	}

	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("Anomalies (%d)", len(m.anomalies))))
	b.WriteString("\n\n")

	// Header
	hdr := fmt.Sprintf("  %-20s %-20s %-15s %10s %10s",
		"Time", "Name", "Anomaly", "Cost", "Model")
	b.WriteString(statLabelStyle.Render(hdr))
	b.WriteString("\n")

	visibleRows := m.visibleRows()
	if visibleRows > len(m.anomalies)-m.offset {
		visibleRows = len(m.anomalies) - m.offset
	}

	for i := m.offset; i < m.offset+visibleRows && i < len(m.anomalies); i++ {
		s := m.anomalies[i]
		t := time.Unix(0, int64(s.StartTimeUnixNano))

		line := fmt.Sprintf("  %-20s %-20s ",
			t.Format("15:04:05.000"),
			truncate(s.Name, 20),
		)

		if i == m.cursor {
			line = selectedRowStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString(anomalyStyle.Render(fmt.Sprintf("%-15s", truncate(s.Anomaly, 15))))
		b.WriteString(fmt.Sprintf(" %10s %10s",
			costStyle.Render(fmt.Sprintf("$%.4f", s.CostUSD)),
			truncate(s.AIModel, 10),
		))
		b.WriteString("\n")
	}

	return b.String()
}

func (m Model) tableLen() int {
	switch m.activeTab {
	case tabSpans:
		return len(m.spans)
	case tabAnomalies:
		return len(m.anomalies)
	default:
		return 0
	}
}

func (m Model) visibleRows() int {
	rows := m.height - 8 // title + tabs + header + help + padding
	if rows < 1 {
		rows = 1
	}
	return rows
}

func (m Model) fetchData() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		filter := store.SpanFilter{
			Since:  time.Now().Add(-24 * time.Hour),
			SortBy: "time",
			Limit:  100,
		}

		stats, _ := m.store.GetStats(ctx, filter)
		models, _ := m.store.GetStatsByModel(ctx, filter)
		spans, _ := m.store.QuerySpans(ctx, filter)
		anomalies, _ := m.store.GetAnomalies(ctx, store.SpanFilter{
			Since: time.Now().Add(-24 * time.Hour),
			Limit: 100,
		})

		return dataMsg{
			stats:     stats,
			models:    models,
			spans:     spans,
			anomalies: anomalies,
		}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func formatTokens(n int64) string {
	if n == 0 {
		return "-"
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
