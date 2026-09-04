package ui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/calebcav/token-usage/internal/app"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	refreshInterval    = 5 * time.Second
	wideLayoutMinWidth = 100
	miniBarWidth       = 14
	compactLimitWidth  = 12
)

type model struct {
	ctx         context.Context
	service     *app.Service
	results     []app.Result
	width       int
	height      int
	selected    int
	loading     bool
	navigating  bool
	err         error
	navigateErr error
	lastRefresh time.Time
}

type refreshMsg struct {
	results []app.Result
	err     error
	at      time.Time
}

type tickMsg time.Time

type navigateMsg struct {
	err error
}

type contextSeverity uint8

const (
	contextHealthy contextSeverity = iota
	contextWarning
	contextCritical
)

func Run(ctx context.Context, service *app.Service) error {
	initial := model{ctx: ctx, service: service, loading: true}
	_, err := tea.NewProgram(initial).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick())
}

func (m model) refresh() tea.Cmd {
	return func() tea.Msg {
		results, err := m.service.Collect(m.ctx, app.CollectOptions{Publish: true, IncludeWorking: true})
		return refreshMsg{results: app.UniqueSessions(results), err: err, at: time.Now()}
	}
}

func (m model) navigate() tea.Cmd {
	paneID := m.results[m.selected].Target.PaneID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		return navigateMsg{err: m.service.FocusPane(ctx, paneID)}
	}
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(value time.Time) tea.Msg { return tickMsg(value) })
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			if !m.loading && !m.navigating {
				m.loading = true
				return m, m.refresh()
			}
		case "enter":
			if !m.navigating && m.selected >= 0 && m.selected < len(m.results) {
				m.navigating = true
				m.navigateErr = nil
				return m, m.navigate()
			}
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected+1 < len(m.results) {
				m.selected++
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case refreshMsg:
		m.results = msg.results
		m.err = msg.err
		m.loading = false
		m.lastRefresh = msg.at
		if m.selected >= len(m.results) {
			m.selected = max(0, len(m.results)-1)
		}
	case navigateMsg:
		m.navigating = false
		m.navigateErr = msg.err
		if msg.err == nil {
			return m, tea.Quit
		}
	case tickMsg:
		commands := []tea.Cmd{tick()}
		if !m.loading {
			m.loading = true
			commands = append(commands, m.refresh())
		}
		return m, tea.Batch(commands...)
	}
	return m, nil
}

var (
	accent   = lipgloss.NewStyle().Foreground(lipgloss.Color("#67E8F9")).Bold(true)
	muted    = lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8"))
	subtle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#475569"))
	good     = lipgloss.NewStyle().Foreground(lipgloss.Color("#86EFAC"))
	warn     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FDE68A"))
	bad      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FDA4AF"))
	selected = lipgloss.NewStyle().Background(lipgloss.Color("#243244")).Foreground(lipgloss.Color("#F8FAFC"))
	panel    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#475569")).Padding(0, 1)
	label    = lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8")).Bold(true)
)

func (m model) View() tea.View {
	width := m.width
	if width <= 0 {
		width = 96
	}
	contentWidth := max(1, width-4)

	var body strings.Builder
	body.WriteString(m.renderHeader(contentWidth))
	body.WriteString("\n\n")

	if m.err != nil {
		message := "Could not read the Herdr session: " + usage.SanitizeText(m.err.Error(), 512)
		body.WriteString(bad.Render(wrapWords(message, contentWidth)))
		body.WriteString("\n\n")
	} else if len(m.results) == 0 && m.loading {
		body.WriteString(muted.Render("Collecting local usage…"))
		body.WriteString("\n")
	} else if len(m.results) == 0 {
		body.WriteString(muted.Render("No supported coding-harness sessions are active."))
		body.WriteString("\n")
	} else {
		body.WriteString(renderTotals(m.results, contentWidth))
		body.WriteString("\n\n")
		start, end := m.visibleRange(contentWidth)
		visible := m
		visible.results = m.results[start:end]
		visible.selected = m.selected - start
		if contentWidth >= wideLayoutMinWidth {
			body.WriteString(visible.renderTable(contentWidth))
		} else {
			body.WriteString(visible.renderCards(contentWidth))
		}
		if start > 0 || end < len(m.results) {
			body.WriteString("\n")
			body.WriteString(muted.Render(fmt.Sprintf("sessions %d–%d of %d", start+1, end, len(m.results))))
		}
		body.WriteString("\n")
		body.WriteString(m.renderDetail(contentWidth))
	}

	if m.navigateErr != nil {
		body.WriteString("\n")
		message := "Could not open session: " + usage.SanitizeText(m.navigateErr.Error(), 512)
		body.WriteString(bad.Render(wrapWords(message, contentWidth)))
	}
	body.WriteString("\n")
	body.WriteString(m.renderFooter(contentWidth))

	view := tea.NewView(lipgloss.NewStyle().Padding(1, 2).MaxWidth(width).Render(body.String()))
	view.AltScreen = true
	view.WindowTitle = "Herdr Token Usage"
	return view
}

func (m model) visibleRange(width int) (int, int) {
	count := len(m.results)
	if count == 0 || m.height <= 0 {
		return 0, count
	}
	linesPerResult := 1
	listHeaderRows := 1
	if width < wideLayoutMinWidth {
		linesPerResult = 2
		listHeaderRows = 0
	}
	// Measure the actual chrome because narrow widths can wrap the title,
	// totals, detail panel, and footer to different heights.
	fixedRows := lipgloss.Height(m.renderHeader(width)) + 1 +
		lipgloss.Height(renderTotals(m.results, width)) + 1 +
		listHeaderRows +
		lipgloss.Height(m.renderDetail(width)) +
		lipgloss.Height(m.renderFooter(width)) + 3
	visible := max(1, (m.height-fixedRows)/linesPerResult)
	if visible >= count {
		return 0, count
	}
	// A clipped list adds a one-line range marker.
	visible = max(1, (m.height-fixedRows-1)/linesPerResult)
	start := m.selected - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > count {
		start = count - visible
	}
	return start, start + visible
}

func (m model) subtitle() string {
	parts := []string{"private", fmt.Sprintf("%d session%s", len(m.results), plural(len(m.results)))}
	if !m.lastRefresh.IsZero() {
		parts = append(parts, "updated "+m.lastRefresh.Format("15:04:05"))
	}
	return strings.Join(parts, "  •  ")
}

func (m model) renderHeader(width int) string {
	subtitle := m.subtitleForWidth(width)
	title := accent.Render("TOKEN USAGE")
	if lipgloss.Width("TOKEN USAGE  "+subtitle) > width {
		return title + "\n" + muted.Render(wrapWords(subtitle, width))
	}
	return title + "  " + muted.Render(subtitle)
}

func (m model) renderFooter(width int) string {
	footer := footerForWidth(width)
	if m.navigating {
		footer = "opening session…"
	} else if m.loading {
		footer = "refreshing…  •  " + footer
	}
	return muted.Render(wrapWords(footer, width))
}

func (m model) subtitleForWidth(width int) string {
	if width < 28 {
		return fmt.Sprintf("%d session%s", len(m.results), plural(len(m.results)))
	}
	if width < 56 {
		return fmt.Sprintf("local  •  %d session%s", len(m.results), plural(len(m.results)))
	}
	return m.subtitle()
}

func footerForWidth(width int) string {
	if width < 28 {
		return "enter open  •  q close"
	}
	if width < 52 {
		return "↑/↓  •  enter open  •  q close"
	}
	return "↑/↓ select  •  enter open  •  r refresh  •  q close"
}

func renderTotals(results []app.Result, width int) string {
	var input, cacheRead, output, spent uint64
	var successes int
	for _, result := range results {
		if result.Snapshot == nil {
			continue
		}
		successes++
		input += result.Snapshot.Tokens.FreshInput
		cacheRead += result.Snapshot.Tokens.CacheRead
		output += result.Snapshot.Tokens.Output
		spent += result.Snapshot.Tokens.Spent()
	}
	value := fmt.Sprintf("SPENT  %s    FRESH INPUT  %s    CACHE READ  %s    OUTPUT  %s",
		usage.FormatCount(spent), usage.FormatCount(input), usage.FormatCount(cacheRead), usage.FormatCount(output))
	if successes == 0 {
		value = "No usage snapshots available"
	}
	return renderPanel(wrapWords(value, panelInnerWidth(width)), width)
}

func (m model) renderTable(width int) string {
	const (
		harnessWidth    = 9
		paneWidth       = 10
		modelWidth      = 16
		contextWidth    = miniBarWidth + 5
		limitWidth      = compactLimitWidth
		countWidth      = 8
		stateWidth      = 8
		confidenceWidth = 9
	)
	header := "  " +
		padRight("HARNESS", harnessWidth) + " " +
		padRight("PANE", paneWidth) + " " +
		padRight("MODEL", modelWidth) + " " +
		padRight("CONTEXT", contextWidth) + " " +
		padRight("LIMIT", limitWidth) + " " +
		padLeft("SPENT", countWidth) + " " +
		padRight("STATE", stateWidth) + " " +
		padRight("MATCH", confidenceWidth)
	var rows []string
	rows = append(rows, muted.Render(header))
	for index, result := range m.results {
		prefix := "  "
		if index == m.selected {
			prefix = "› "
		}
		var row string
		if result.Snapshot == nil {
			errorWidth := max(1, width-harnessWidth-paneWidth-5)
			row = prefix +
				padRight(clip(result.Target.Harness, harnessWidth), harnessWidth) + " " +
				padRight(clip(result.Target.PaneID, paneWidth), paneWidth) + " " +
				bad.Render(clip(defaultValue(result.Error, "Usage unavailable"), errorWidth))
		} else {
			snapshot := result.Snapshot
			row = prefix +
				padRight(clip(snapshot.Harness, harnessWidth), harnessWidth) + " " +
				padRight(clip(snapshot.PaneID, paneWidth), paneWidth) + " " +
				padRight(clip(defaultValue(snapshot.Model, "—"), modelWidth), modelWidth) + " " +
				padRight(renderCompactContext(snapshot.Context, miniBarWidth, index != m.selected), contextWidth) + " " +
				padRight(renderCompactLimit(snapshot, index != m.selected), limitWidth) + " " +
				padLeft(usage.FormatCount(snapshot.Tokens.Spent()), countWidth) + " " +
				padRight(clip(defaultValue(snapshot.State, "unknown"), stateWidth), stateWidth) + " " +
				padRight(renderConfidence(snapshot.Confidence, index != m.selected), confidenceWidth)
		}
		if index == m.selected {
			row = selected.Width(width).Render(row)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func (m model) renderCards(width int) string {
	rows := make([]string, 0, len(m.results)*2)
	for index, result := range m.results {
		prefix := "  "
		if index == m.selected {
			prefix = "› "
		}
		var first, second string
		if result.Snapshot == nil {
			identity := fmt.Sprintf("%s  %s",
				strings.ToUpper(usage.SanitizeText(result.Target.Harness, 80)),
				usage.SanitizeText(result.Target.PaneID, 80))
			first = prefix + clip(identity, max(1, width-lipgloss.Width(prefix)))
			second = "    " + bad.Render(clip(result.Error, max(1, width-4)))
		} else {
			snapshot := result.Snapshot
			identity := fmt.Sprintf("%s  %s  %s",
				strings.ToUpper(snapshot.Harness), snapshot.PaneID, defaultValue(snapshot.Model, "model unknown"))
			first = prefix + clip(identity, max(1, width-lipgloss.Width(prefix)))
			second = renderCardContext(snapshot, width)
		}
		if index == m.selected {
			first = selected.Width(width).Render(first)
		}
		rows = append(rows, first, second)
	}
	return strings.Join(rows, "\n")
}

func (m model) renderDetail(width int) string {
	if m.selected < 0 || m.selected >= len(m.results) {
		return ""
	}
	result := m.results[m.selected]
	if result.Snapshot == nil {
		return renderPanel(bad.Render(defaultValue(result.Error, "Usage unavailable")), width)
	}
	snapshot := result.Snapshot
	innerWidth := panelInnerWidth(width)
	lines := make([]string, 0, 6)
	if snapshot.Context == nil || snapshot.Context.Limit == 0 {
		if innerWidth < 44 {
			lines = append(lines, label.Render("CONTEXT WINDOW"), muted.Render("not reported by this harness"))
		} else {
			lines = append(lines,
				label.Render("CONTEXT WINDOW")+"  "+muted.Render("not reported by this harness"))
		}
	} else {
		percent := displayPercent(snapshot.Context.Percent())
		lines = append(lines,
			label.Render("CONTEXT WINDOW")+"  "+contextStyle(snapshot.Context.Percent()).Render(percent+" used"),
			renderContextBar(snapshot.Context, min(52, innerWidth)),
			muted.Render(wrapWords(contextUsageLine(snapshot.Context), innerWidth)),
		)
	}
	if snapshot.Quota == nil || len(snapshot.Quota.Windows) == 0 {
		if innerWidth < 44 {
			lines = append(lines, label.Render("LIMITS")+"  "+muted.Render("not reported"))
		} else {
			lines = append(lines, label.Render("ACCOUNT LIMITS")+"  "+muted.Render("not reported by this harness"))
		}
	} else {
		lines = append(lines, label.Render("ACCOUNT LIMITS"))
		for _, window := range snapshot.Quota.Windows {
			lines = append(lines, wrapWords(quotaWindowLine(window), innerWidth))
		}
	}
	tokenParts := []string{
		"fresh input " + usage.FormatCount(snapshot.Tokens.FreshInput),
		"cache read " + usage.FormatCount(snapshot.Tokens.CacheRead),
		"cache write " + usage.FormatCount(snapshot.Tokens.CacheWrite),
		"output " + usage.FormatCount(snapshot.Tokens.Output),
		"spent " + usage.FormatCount(snapshot.Tokens.Spent()),
		"processed " + usage.FormatCount(snapshot.Tokens.Total),
	}
	metaParts := make([]string, 0, 3)
	if snapshot.Tokens.Reasoning > 0 {
		metaParts = append(metaParts, "reasoning "+usage.FormatCount(snapshot.Tokens.Reasoning)+" (inside output)")
	}
	metaParts = append(metaParts, defaultValue(snapshot.State, "unknown"), confidenceDescription(snapshot.Confidence))
	lines = append(lines,
		wrapWords("TOKENS  "+strings.Join(tokenParts, "  •  "), innerWidth),
		muted.Render(wrapWords(strings.Join(metaParts, "  •  "), innerWidth)),
	)
	return renderPanel(strings.Join(lines, "\n"), width)
}

func renderCardContext(snapshot *usage.Snapshot, width int) string {
	const indent = "    "
	spent := "spent " + usage.FormatCount(snapshot.Tokens.Spent())
	limit := compactCardLimit(snapshot)
	meta := compactCardMeta(snapshot)
	matchPrefix := ""
	if snapshot.Confidence == usage.ConfidenceEstimated {
		matchPrefix = "≈ "
	}
	if snapshot.Context == nil || snapshot.Context.Limit == 0 {
		if width < 42 {
			line := clip(matchPrefix+spent+"  "+meta+"  context not reported  "+limit, max(1, width-lipgloss.Width(indent)))
			return indent + muted.Render(line)
		}
		line := spent + "  " + meta + "  ctx not reported  " + limit
		return indent + muted.Render(clip(line, max(1, width-lipgloss.Width(indent))))
	}
	percent := displayPercent(snapshot.Context.Percent())
	if width < 36 {
		line := clip(matchPrefix+spent+"  ctx "+percent+"  "+limit, max(1, width-lipgloss.Width(indent)))
		return indent + contextStyle(snapshot.Context.Percent()).Render(line)
	}
	barWidth := min(16, max(4, width-lipgloss.Width(indent)-lipgloss.Width(percent)-lipgloss.Width(limit)-lipgloss.Width(spent)-lipgloss.Width(meta)-10))
	return indent + renderContextBar(snapshot.Context, barWidth) + " " +
		contextStyle(snapshot.Context.Percent()).Render(percent) + "  " + spent + "  " + meta + "  " + limit
}

func compactCardMeta(snapshot *usage.Snapshot) string {
	state := clip(defaultValue(snapshot.State, "unknown"), 6)
	if snapshot.Confidence == usage.ConfidenceEstimated {
		return state + "·≈"
	}
	return state
}

func renderCompactContext(window *usage.ContextWindow, barWidth int, styled bool) string {
	if window == nil || window.Limit == 0 {
		if !styled {
			return "— not reported"
		}
		return muted.Render("— not reported")
	}
	if !styled {
		filled, empty := barSegments(window.Percent(), barWidth)
		return strings.Repeat("━", filled) + strings.Repeat("─", empty) + " " + displayPercent(window.Percent())
	}
	return renderContextBar(window, barWidth) + " " +
		contextStyle(window.Percent()).Render(displayPercent(window.Percent()))
}

func renderCompactLimit(snapshot *usage.Snapshot, styled bool) string {
	window, ok := snapshot.MostConstrainedQuota()
	if !ok {
		value := "not reported"
		if !styled {
			return value
		}
		return muted.Render(value)
	}
	value := clip(window.Label+" "+displayPercent(window.UsedPercent), compactLimitWidth)
	if !styled {
		return value
	}
	return contextStyle(window.UsedPercent).Render(value)
}

func compactCardLimit(snapshot *usage.Snapshot) string {
	if value := snapshot.CompactLimit(); value != "" {
		return "limit " + clip(value, 20)
	}
	return "limit not reported"
}

func quotaWindowLine(window usage.QuotaWindow) string {
	value := window.Label + " " + displayPercent(window.UsedPercent) + " used"
	if window.ResetsAt != nil {
		value += "  •  resets " + window.ResetsAt.Local().Format("Jan 2 15:04 MST")
	}
	return contextStyle(window.UsedPercent).Render(value)
}

func renderContextBar(window *usage.ContextWindow, width int) string {
	if window == nil || window.Limit == 0 || width <= 0 {
		return ""
	}
	filled, empty := barSegments(window.Percent(), width)
	return contextStyle(window.Percent()).Render(strings.Repeat("━", filled)) +
		subtle.Render(strings.Repeat("─", empty))
}

func barSegments(percent float64, width int) (int, int) {
	if width <= 0 {
		return 0, 0
	}
	ratio := math.Max(0, math.Min(1, percent/100))
	filled := int(math.Round(ratio * float64(width)))
	return filled, width - filled
}

func contextStyle(percent float64) lipgloss.Style {
	switch contextSeverityFor(percent) {
	case contextCritical:
		return bad
	case contextWarning:
		return warn
	default:
		return good
	}
}

func contextSeverityFor(percent float64) contextSeverity {
	switch {
	case percent >= 90:
		return contextCritical
	case percent >= 70:
		return contextWarning
	default:
		return contextHealthy
	}
}

func displayPercent(percent float64) string {
	return fmt.Sprintf("%.0f%%", math.Round(math.Min(999, math.Max(0, percent))))
}

func contextUsageLine(window *usage.ContextWindow) string {
	if window == nil || window.Limit == 0 {
		return "context window unavailable"
	}
	if window.Used > window.Limit {
		return fmt.Sprintf("%s used  •  %s over  •  %s limit",
			usage.FormatCount(window.Used), usage.FormatCount(window.Used-window.Limit), usage.FormatCount(window.Limit))
	}
	return fmt.Sprintf("%s used  •  %s left  •  %s limit",
		usage.FormatCount(window.Used), usage.FormatCount(window.Limit-window.Used), usage.FormatCount(window.Limit))
}

func renderConfidence(confidence usage.Confidence, styled bool) string {
	label := string(confidence)
	if confidence == usage.ConfidenceEstimated {
		if !styled {
			return "≈ est."
		}
		return warn.Render("≈ est.")
	}
	if label == "" {
		if !styled {
			return "unknown"
		}
		return muted.Render("unknown")
	}
	return label
}

func confidenceDescription(confidence usage.Confidence) string {
	if confidence == usage.ConfidenceEstimated {
		return "match ≈ estimated"
	}
	if confidence == "" {
		return "match unknown"
	}
	return "match " + string(confidence)
}

func wrapWords(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	words := strings.Fields(value)
	var lines []string
	var line string
	for _, word := range words {
		candidate := word
		if line != "" {
			candidate = line + " " + word
		}
		if lipgloss.Width(candidate) > width && line != "" {
			lines = append(lines, line)
			line = word
		} else {
			line = candidate
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func renderPanel(content string, width int) string {
	if width < 5 {
		return ansi.Truncate(content, max(1, width), "…")
	}
	return panel.Width(width).Render(content)
}

func panelInnerWidth(width int) int {
	// Rounded borders and one cell of horizontal padding consume four cells.
	return max(1, width-4)
}

func padRight(value string, width int) string {
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func padLeft(value string, width int) string {
	return strings.Repeat(" ", max(0, width-lipgloss.Width(value))) + value
}

func clip(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = usage.SanitizeText(value, 0)
	if lipgloss.Width(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "…")
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
