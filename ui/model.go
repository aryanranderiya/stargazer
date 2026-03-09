package ui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	colorful "github.com/lucasb-eyer/go-colorful"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	gh "stargazer/internal/github"
	creds "stargazer/internal/credentials"
	scr "stargazer/internal/scraper"
)

// --------------------------------------------------------------------------
// App state
// --------------------------------------------------------------------------

type appState int

const (
	stateForm appState = iota
	stateRunning
	stateDone
	stateError
)

// --------------------------------------------------------------------------
// Color palette
// --------------------------------------------------------------------------

var (
	clrPrimary  = lipgloss.Color("#7C3AED")
	clrIndigo   = lipgloss.Color("#4F46E5")
	clrSuccess  = lipgloss.Color("#10B981")
	clrWarning  = lipgloss.Color("#F59E0B")
	clrDanger   = lipgloss.Color("#EF4444")
	clrMuted    = lipgloss.Color("#6B7280")
	clrSubtle   = lipgloss.Color("#9CA3AF")
	clrCyan     = lipgloss.Color("#06B6D4")
	clrHighBg   = lipgloss.Color("#1E1B4B") // deep indigo bg for highlight
)

// --------------------------------------------------------------------------
// Styles
// --------------------------------------------------------------------------

var (
	// Text styles
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(clrPrimary)
	subtitleStyle = lipgloss.NewStyle().Foreground(clrSubtle).Italic(true)
	labelStyle  = lipgloss.NewStyle().Foreground(clrMuted)
	activeLabel = lipgloss.NewStyle().Foreground(clrPrimary).Bold(true)
	hintStyle   = lipgloss.NewStyle().Foreground(clrSubtle)
	warnStyle   = lipgloss.NewStyle().Foreground(clrWarning).Bold(true)
	errorStyle  = lipgloss.NewStyle().Foreground(clrDanger).Bold(true)
	okStyle     = lipgloss.NewStyle().Foreground(clrSuccess).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(clrSubtle)
	cyanStyle   = lipgloss.NewStyle().Foreground(clrCyan)
	indigoStyle = lipgloss.NewStyle().Foreground(clrIndigo).Bold(true)

	// Containers
	appStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clrPrimary).
		Padding(1, 3)

	vpBorderStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clrMuted)

	statCardStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clrIndigo).
		Padding(0, 2)

	errorBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clrDanger).
		Padding(0, 2)

	doneBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clrSuccess).
		Padding(0, 2)
)

// --------------------------------------------------------------------------
// BubbleTea messages
// --------------------------------------------------------------------------

type progressMsg scr.Progress

func listenProgress(ch <-chan scr.Progress) tea.Cmd {
	return func() tea.Msg {
		return progressMsg(<-ch)
	}
}

// --------------------------------------------------------------------------
// Dynamic field layout constants
// --------------------------------------------------------------------------

const (
	maxTokenFields = 10
	minTokenFields = 1

	cfgOutputPath   = 0
	cfgConcurrency  = 1
	cfgMaxRepos     = 2
	cfgMaxForkRepos = 3
	cfgMaxStars     = 4
	cfgDelay        = 5
	cfgUseSearch    = 6
	numConfigFields = 7
)

var configLabels = [numConfigFields]string{
	"Output CSV Path",
	"Concurrency          (parallel workers, 1–20)",
	"Max Repos per User   (own repos to scan for commit email, 1–10)",
	"Max Fork Repos       (forked repos to scan, 1–5)",
	"Max Stars            (0 = fetch all)",
	"Request Delay (ms)   (pause between API calls)",
	"Commit Search API    (y/n – searches ALL of GitHub, uses separate 30 req/min limit)",
}

// --------------------------------------------------------------------------
// Per-user result (for the live log)
// --------------------------------------------------------------------------

type userResult struct {
	login       string
	email       string
	emailSource string
	fetchFailed bool
}

type runStats struct {
	profile int
	commit  int
	noreply int
	failed  int
}

// --------------------------------------------------------------------------
// Model
// --------------------------------------------------------------------------

type Model struct {
	state        appState
	repoInput    textinput.Model
	tokenInputs  []textinput.Model
	configInputs [numConfigFields]textinput.Model
	focused      int

	spinner     spinner.Model
	progressBar progress.Model
	vp          viewport.Model
	vpContent   string

	progressCh chan scr.Progress

	lastStatus  string
	userResults []userResult
	stats       runStats
	total       int
	current     int
	startTime   time.Time

	outputPath string
	count      int
	elapsed    time.Duration

	errMsg     string
	repoWarn   string
	rateLimits []gh.RateLimitStatus

	width  int
	height int
}

func (m *Model) totalFields() int {
	return 1 + len(m.tokenInputs) + numConfigFields
}

func (m *Model) configStart() int {
	return 1 + len(m.tokenInputs)
}

func (m *Model) isTokenFocused() bool {
	f := m.focused
	return f >= 1 && f <= len(m.tokenInputs)
}

func tokenLabel(i int) string {
	if i == 0 {
		return "GitHub Token 1   (saved to ~/.config/stargazer/credentials.json)"
	}
	return fmt.Sprintf("GitHub Token %d", i+1)
}

func newMaskedInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "ghp_..."
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.CharLimit = 200
	return ti
}

func NewModel() Model {
	repoInput := textinput.New()
	repoInput.Placeholder = "owner/repo"
	repoInput.SetValue("facebook/react")
	repoInput.CharLimit = 200
	repoInput.Focus()

	var tokenInputs []textinput.Model
	var configInputs [numConfigFields]textinput.Model

	configInputs[cfgOutputPath] = textinput.New()
	configInputs[cfgOutputPath].Placeholder = "stars/owner/repo.csv"
	configInputs[cfgOutputPath].SetValue(filepath.Join("stars", "facebook", "react.csv"))
	configInputs[cfgOutputPath].CharLimit = 500

	configInputs[cfgConcurrency] = textinput.New()
	configInputs[cfgConcurrency].SetValue("5")
	configInputs[cfgConcurrency].CharLimit = 3

	configInputs[cfgMaxRepos] = textinput.New()
	configInputs[cfgMaxRepos].SetValue("3")
	configInputs[cfgMaxRepos].CharLimit = 3

	configInputs[cfgMaxForkRepos] = textinput.New()
	configInputs[cfgMaxForkRepos].SetValue("2")
	configInputs[cfgMaxForkRepos].CharLimit = 3

	configInputs[cfgMaxStars] = textinput.New()
	configInputs[cfgMaxStars].SetValue("0")
	configInputs[cfgMaxStars].Placeholder = "0 = all"
	configInputs[cfgMaxStars].CharLimit = 7

	configInputs[cfgDelay] = textinput.New()
	configInputs[cfgDelay].SetValue("100")
	configInputs[cfgDelay].CharLimit = 6

	configInputs[cfgUseSearch] = textinput.New()
	configInputs[cfgUseSearch].SetValue("n")
	configInputs[cfgUseSearch].Placeholder = "y or n"
	configInputs[cfgUseSearch].CharLimit = 3

	if c, err := creds.Load(); err == nil && len(c.Tokens) > 0 {
		count := len(c.Tokens)
		if count > maxTokenFields {
			count = maxTokenFields
		}
		for i := 0; i < count; i++ {
			ti := newMaskedInput()
			ti.SetValue(c.Tokens[i])
			tokenInputs = append(tokenInputs, ti)
		}
	}
	if len(tokenInputs) == 0 {
		tokenInputs = append(tokenInputs, newMaskedInput())
	}

	sp := spinner.New()
	sp.Spinner = spinner.Points
	sp.Style = lipgloss.NewStyle().Foreground(clrPrimary)

	pb := progress.New(
		progress.WithGradient("#7C3AED", "#06B6D4"),
		progress.WithoutPercentage(),
	)

	vp := viewport.New(80, 10)

	return Model{
		state:        stateForm,
		repoInput:    repoInput,
		tokenInputs:  tokenInputs,
		configInputs: configInputs,
		focused:      0,
		spinner:      sp,
		progressBar:  pb,
		vp:           vp,
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

// --------------------------------------------------------------------------
// Update
// --------------------------------------------------------------------------

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		vpW := msg.Width - 14
		if vpW < 40 {
			vpW = 40
		}
		vpH := msg.Height - 22
		if vpH < 5 {
			vpH = 5
		}
		m.progressBar.Width = vpW
		m.vp.Width = vpW
		m.vp.Height = vpH
		m.vp.SetContent(m.vpContent)
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.state {
		case stateForm:
			return m.updateForm(msg)

		case stateRunning:
			if msg.String() == "q" {
				return m, tea.Quit
			}
			switch msg.String() {
			case "up", "down", "pgup", "pgdown", "home", "end":
				var cmd tea.Cmd
				m.vp, cmd = m.vp.Update(msg)
				return m, cmd
			}

		case stateDone, stateError:
			switch msg.String() {
			case "q", "esc", "enter":
				return m, tea.Quit
			default:
				var cmd tea.Cmd
				m.vp, cmd = m.vp.Update(msg)
				return m, cmd
			}
		}

	case progressMsg:
		p := scr.Progress(msg)

		if p.Error != nil {
			m.state = stateError
			m.errMsg = p.Error.Error()
			return m, nil
		}

		if p.Done {
			m.state = stateDone
			m.outputPath = p.OutputPath
			m.count = p.Count
			m.elapsed = time.Since(m.startTime)
			m.vp.GotoTop()
			return m, nil
		}

		if p.Total > 0 {
			m.total = p.Total
		}
		if p.Current > 0 {
			m.current = p.Current
		}
		if p.Status != "" {
			m.lastStatus = p.Status
		}
		if len(p.RateLimits) > 0 {
			m.rateLimits = p.RateLimits
		}

		if r := p.Result; r != nil {
			ur := userResult{
				login:       r.Login,
				email:       r.Email,
				emailSource: r.EmailSource,
				fetchFailed: r.FetchFailed,
			}
			m.userResults = append(m.userResults, ur)

			switch {
			case r.FetchFailed:
				m.stats.failed++
			case r.EmailSource == "profile":
				m.stats.profile++
			case r.EmailSource == "commit":
				m.stats.commit++
			default:
				m.stats.noreply++
			}

			m.vpContent += buildResultLine(ur) + "\n"
			m.vp.SetContent(m.vpContent)
			m.vp.GotoBottom()
		}

		return m, listenProgress(m.progressCh)

	case progress.FrameMsg:
		progressModel, cmd := m.progressBar.Update(msg)
		m.progressBar = progressModel.(progress.Model)
		return m, cmd

	case spinner.TickMsg:
		if m.state == stateRunning {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

// --------------------------------------------------------------------------
// Form logic
// --------------------------------------------------------------------------

func (m *Model) blurAll() {
	m.repoInput.Blur()
	for i := range m.tokenInputs {
		m.tokenInputs[i].Blur()
	}
	for i := range m.configInputs {
		m.configInputs[i].Blur()
	}
}

func (m *Model) focusCurrent() {
	total := m.totalFields()
	cfgStart := m.configStart()

	switch {
	case m.focused == 0:
		m.repoInput.Focus()
	case m.focused >= 1 && m.focused <= len(m.tokenInputs):
		m.tokenInputs[m.focused-1].Focus()
	case m.focused >= cfgStart && m.focused < total:
		m.configInputs[m.focused-cfgStart].Focus()
	}
}

func (m Model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	total := m.totalFields()

	if m.isTokenFocused() {
		switch msg.String() {
		case "ctrl+n":
			if len(m.tokenInputs) < maxTokenFields {
				m.blurAll()
				m.tokenInputs = append(m.tokenInputs, newMaskedInput())
				m.focused = len(m.tokenInputs)
				m.focusCurrent()
			}
			return m, textinput.Blink

		case "ctrl+x":
			if len(m.tokenInputs) > minTokenFields {
				m.blurAll()
				m.tokenInputs = m.tokenInputs[:len(m.tokenInputs)-1]
				if m.focused > len(m.tokenInputs) {
					m.focused = len(m.tokenInputs)
				}
				m.focusCurrent()
			}
			return m, textinput.Blink
		}
	}

	total = m.totalFields()

	switch msg.String() {
	case "tab", "down":
		m.blurAll()
		m.focused = (m.focused + 1) % total
		m.focusCurrent()
		return m, textinput.Blink

	case "shift+tab", "up":
		m.blurAll()
		m.focused = (m.focused - 1 + total) % total
		m.focusCurrent()
		return m, textinput.Blink

	case "enter":
		if m.focused < total-1 {
			m.blurAll()
			m.focused++
			m.focusCurrent()
			return m, textinput.Blink
		}
		return m.submit()
	}

	var cmd tea.Cmd
	cfgStart := m.configStart()
	switch {
	case m.focused == 0:
		m.repoInput, cmd = m.repoInput.Update(msg)
		raw := m.repoInput.Value()
		normalised, warn := parseRepoInput(raw)
		m.repoWarn = warn
		if normalised != raw {
			m.repoInput.SetValue(normalised)
		}
		repo := m.repoInput.Value()
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			cur := m.configInputs[cfgOutputPath].Value()
			if strings.HasPrefix(cur, "stars") || cur == "" {
				m.configInputs[cfgOutputPath].SetValue(filepath.Join("stars", parts[0], parts[1]+".csv"))
			}
		}

	case m.focused >= 1 && m.focused <= len(m.tokenInputs):
		idx := m.focused - 1
		m.tokenInputs[idx], cmd = m.tokenInputs[idx].Update(msg)

	case m.focused >= cfgStart && m.focused < total:
		idx := m.focused - cfgStart
		m.configInputs[idx], cmd = m.configInputs[idx].Update(msg)
	}

	return m, cmd
}

func parseRepoInput(raw string) (string, string) {
	s := strings.TrimSpace(raw)
	for _, prefix := range []string{
		"https://github.com/",
		"http://github.com/",
		"github.com/",
	} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return s, ""
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return s, "Format must be owner/repo (e.g. torvalds/linux)"
	}
	return parts[0] + "/" + parts[1], ""
}

func (m Model) submit() (tea.Model, tea.Cmd) {
	repo, warn := parseRepoInput(m.repoInput.Value())
	if warn != "" {
		m.repoWarn = warn
		m.focused = 0
		m.blurAll()
		m.focusCurrent()
		return m, textinput.Blink
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		m.repoWarn = "Repository must be in owner/repo format (e.g. facebook/react)"
		m.focused = 0
		m.blurAll()
		m.focusCurrent()
		return m, textinput.Blink
	}

	var tokens []string
	for _, ti := range m.tokenInputs {
		if t := strings.TrimSpace(ti.Value()); t != "" {
			tokens = append(tokens, t)
		}
	}
	_ = creds.Save(&creds.Credentials{Tokens: tokens})

	concurrency := clampInt(m.configInputs[cfgConcurrency].Value(), 5, 1, 20)
	maxRepos := clampInt(m.configInputs[cfgMaxRepos].Value(), 3, 1, 10)
	maxForkRepos := clampInt(m.configInputs[cfgMaxForkRepos].Value(), 2, 1, 5)
	maxStars := clampInt(m.configInputs[cfgMaxStars].Value(), 0, 0, 10_000_000)
	delayMs := clampInt(m.configInputs[cfgDelay].Value(), 100, 0, 60_000)
	useSearch := strings.ToLower(strings.TrimSpace(m.configInputs[cfgUseSearch].Value())) == "y"

	outputPath := strings.TrimSpace(m.configInputs[cfgOutputPath].Value())
	if outputPath == "" {
		outputPath = filepath.Join("stars", parts[0], parts[1]+".csv")
	}

	cfg := scr.Config{
		Owner:        parts[0],
		Repo:         parts[1],
		Tokens:       tokens,
		OutputPath:   outputPath,
		Concurrency:  concurrency,
		MaxRepos:     maxRepos,
		MaxForkRepos: maxForkRepos,
		MaxStars:     maxStars,
		Delay:        time.Duration(delayMs) * time.Millisecond,
		UseSearchAPI: useSearch,
	}

	m.progressCh = make(chan scr.Progress, 500)
	m.outputPath = outputPath
	m.state = stateRunning
	m.startTime = time.Now()

	go scr.Run(cfg, m.progressCh)

	return m, tea.Batch(
		listenProgress(m.progressCh),
		m.spinner.Tick,
	)
}

// --------------------------------------------------------------------------
// Views
// --------------------------------------------------------------------------

func (m Model) View() string {
	switch m.state {
	case stateForm:
		return m.formView()
	case stateRunning:
		return m.runningView()
	case stateDone:
		return m.doneView()
	case stateError:
		return m.errorView()
	}
	return ""
}

// sectionDivider renders a styled "── Label ─────────" divider.
func sectionDivider(label string, width int) string {
	prefix := "── " + label + " "
	prefixWidth := lipgloss.Width(prefix)
	fill := width - prefixWidth - 2
	if fill < 2 {
		fill = 2
	}
	return lipgloss.NewStyle().Foreground(clrIndigo).Bold(true).Render(
		prefix + strings.Repeat("─", fill),
	)
}

// lerpColor linearly interpolates between two hex colors, returning a hex string.
func lerpColor(from, to colorful.Color, t float64) string {
	r := from.R + (to.R-from.R)*t
	g := from.G + (to.G-from.G)*t
	b := from.B + (to.B-from.B)*t
	return colorful.Color{R: r, G: g, B: b}.Hex()
}

// gradientBanner renders text centered on a full-width gradient background strip.
// Each cell gets its own interpolated bg color, white bold italic foreground.
func gradientBanner(text string, width int) string {
	from, _ := colorful.Hex("#5B21B6") // deep violet
	to, _ := colorful.Hex("#0E7490")   // deep cyan

	// Build rune slice for the full banner width (spaces + centered text)
	runes := []rune(strings.Repeat(" ", width))
	textRunes := []rune(text)
	offset := (width - len(textRunes)) / 2
	if offset < 0 {
		offset = 0
	}
	for i, r := range textRunes {
		if offset+i < len(runes) {
			runes[offset+i] = r
		}
	}

	var sb strings.Builder
	fg := lipgloss.Color("#F5F3FF")
	for i, r := range runes {
		t := float64(i) / float64(max(width-1, 1))
		bg := lipgloss.Color(lerpColor(from, to, t))
		sb.WriteString(
			lipgloss.NewStyle().
				Background(bg).
				Foreground(fg).
				Bold(true).
				Italic(true).
				Render(string(r)),
		)
	}
	return sb.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// renderHeader renders the app title banner.
func renderHeader(width int) string {
	bannerWidth := width
	if bannerWidth < 20 {
		bannerWidth = 20
	}
	banner := gradientBanner("  ★  STARGAZER  ", bannerWidth)
	sub := subtitleStyle.Render("  Extract stargazer emails from any public repository")
	return banner + "\n" + sub
}

func (m Model) contentWidth() int {
	w := m.width - 10 // account for box padding/borders
	if w < 60 {
		w = 60
	}
	if w > 110 {
		w = 110
	}
	return w
}

func (m Model) formView() string {
	cw := m.contentWidth()
	var b strings.Builder

	b.WriteString(renderHeader(cw) + "\n\n")

	// ── Target ──────────────────────────────
	b.WriteString(sectionDivider("Target", cw) + "\n\n")

	if m.focused == 0 {
		b.WriteString(activeLabel.Render("▶ Repository (owner/repo)") + "\n")
	} else {
		b.WriteString(labelStyle.Render("  Repository (owner/repo)") + "\n")
	}
	b.WriteString(m.repoInput.View() + "\n")
	if m.repoWarn != "" {
		b.WriteString(warnStyle.Render("  ⚠  " + m.repoWarn) + "\n")
	}
	b.WriteString("\n")

	// ── Authentication ──────────────────────
	b.WriteString(sectionDivider("Authentication", cw) + "\n\n")

	for i, ti := range m.tokenInputs {
		logicalIdx := i + 1
		lbl := tokenLabel(i)
		if m.focused == logicalIdx {
			b.WriteString(activeLabel.Render("▶ "+lbl) + "\n")
		} else {
			b.WriteString(labelStyle.Render("  "+lbl) + "\n")
		}
		b.WriteString(ti.View() + "\n\n")
	}

	if m.isTokenFocused() {
		b.WriteString(
			hintStyle.Render("  ") +
				cyanStyle.Render("Ctrl+N") +
				hintStyle.Render(" add token  •  ") +
				cyanStyle.Render("Ctrl+X") +
				hintStyle.Render(" remove last") +
				"\n\n",
		)
	}

	// Token hints
	b.WriteString(hintStyle.Render("  Create tokens at: github.com/settings/tokens  →  No special scopes needed for public repos") + "\n\n")

	activeTokens := 0
	for _, ti := range m.tokenInputs {
		if strings.TrimSpace(ti.Value()) != "" {
			activeTokens++
		}
	}
	switch {
	case activeTokens == 0:
		b.WriteString(warnStyle.Render("  ⚠  No token set – rate limit is 60 req/hr. Large repos will be very slow.") + "\n\n")
	case activeTokens > 1:
		b.WriteString(okStyle.Render(fmt.Sprintf("  ✓  %d tokens active – will rotate on rate-limit", activeTokens)) + "\n\n")
	default:
		b.WriteString(okStyle.Render("  ✓  Token saved") + "\n\n")
	}

	// ── Configuration ───────────────────────
	b.WriteString(sectionDivider("Configuration", cw) + "\n\n")

	cfgStart := m.configStart()
	for i := 0; i < numConfigFields; i++ {
		logicalIdx := cfgStart + i
		lbl := configLabels[i]
		if m.focused == logicalIdx {
			b.WriteString(activeLabel.Render("▶ "+lbl) + "\n")
		} else {
			b.WriteString(labelStyle.Render("  "+lbl) + "\n")
		}
		b.WriteString(m.configInputs[i].View() + "\n\n")
	}

	// Footer hints
	b.WriteString(strings.Repeat("─", cw) + "\n")
	b.WriteString(
		hintStyle.Render("  ") +
			cyanStyle.Render("Tab / ↑↓") +
			hintStyle.Render(" navigate  •  ") +
			cyanStyle.Render("Enter") +
			hintStyle.Render(" advance  •  ") +
			cyanStyle.Render("Enter on last field") +
			hintStyle.Render(" starts scraping  •  ") +
			cyanStyle.Render("Ctrl+C") +
			hintStyle.Render(" quit"),
	)

	return appStyle.Render(b.String())
}

func (m Model) runningView() string {
	cw := m.contentWidth()
	var b strings.Builder

	b.WriteString(renderHeader(cw) + "\n\n")

	if m.total == 0 {
		// Fetch stage
		b.WriteString(
			m.spinner.View() + "  " +
				indigoStyle.Render("Fetching stargazers...") + "\n",
		)
		if m.lastStatus != "" {
			b.WriteString(dimStyle.Render("    "+m.lastStatus) + "\n")
		}
		b.WriteString("\n")
	} else {
		// Processing stage
		pct := float64(m.current) / float64(m.total)
		elapsed := time.Since(m.startTime).Round(time.Second)

		// Progress line
		pctStr := fmt.Sprintf("%.1f%%", pct*100)
		b.WriteString(
			m.spinner.View() + "  " +
				indigoStyle.Render(fmt.Sprintf("Processing  %d / %d", m.current, m.total)) +
				"  " + cyanStyle.Render(pctStr) +
				"  " + dimStyle.Render(elapsed.String()) +
				"\n",
		)
		b.WriteString(m.progressBar.ViewAs(pct) + "\n\n")

		// Stats in a 2x2 grid using JoinHorizontal
		profileCard := statCardStyle.Render(
			okStyle.Render("✓ profile") + "\n" +
				lipgloss.NewStyle().Foreground(clrSuccess).Bold(true).
					Render(fmt.Sprintf("  %d", m.stats.profile)),
		)
		commitCard := statCardStyle.Render(
			okStyle.Render("✓ commit") + "\n" +
				lipgloss.NewStyle().Foreground(clrSuccess).Bold(true).
					Render(fmt.Sprintf("  %d", m.stats.commit)),
		)
		noreplyCard := statCardStyle.Render(
			warnStyle.Render("⚠ noreply") + "\n" +
				lipgloss.NewStyle().Foreground(clrWarning).Bold(true).
					Render(fmt.Sprintf("  %d", m.stats.noreply)),
		)
		errCard := statCardStyle.Render(
			errorStyle.Render("✗ errors") + "\n" +
				lipgloss.NewStyle().Foreground(clrDanger).Bold(true).
					Render(fmt.Sprintf("  %d", m.stats.failed)),
		)
		statsRow := lipgloss.JoinHorizontal(lipgloss.Top,
			profileCard, "  ", commitCard, "  ", noreplyCard, "  ", errCard,
		)
		b.WriteString(statsRow + "\n\n")
	}

	// Rate limit panel
	if len(m.rateLimits) > 0 {
		b.WriteString(sectionDivider("Rate Limits", cw) + "\n")
		now := time.Now()
		for i, rl := range m.rateLimits {
			resetsIn := rl.Core.Reset.Sub(now).Round(time.Minute)
			var resetStr string
			if resetsIn <= 0 {
				resetStr = "now"
			} else {
				resetStr = fmt.Sprintf("%dm", int(resetsIn.Minutes()))
			}
			line := fmt.Sprintf("  Token %d (%s): Core %d/%d, resets in %s",
				i+1, rl.Token, rl.Core.Remaining, rl.Core.Limit, resetStr)
			b.WriteString(dimStyle.Render(line) + "\n")
		}
		b.WriteString("\n")
	}

	// Live results viewport
	if len(m.userResults) > 0 {
		b.WriteString(sectionDivider("Live Results", cw) + "\n")
		b.WriteString(hintStyle.Render("  ↑↓ scroll") + "\n")
		b.WriteString(vpBorderStyle.Render(m.vp.View()) + "\n")
	}

	b.WriteString("\n" +
		hintStyle.Render("  ") +
		cyanStyle.Render("q / Ctrl+C") +
		hintStyle.Render(" to quit"),
	)

	return appStyle.Render(b.String())
}

func (m Model) doneView() string {
	cw := m.contentWidth()
	var b strings.Builder

	mins := int(m.elapsed.Minutes())
	secs := int(m.elapsed.Seconds()) % 60

	b.WriteString(renderHeader(cw) + "\n\n")

	// Done summary box
	summaryContent := okStyle.Render(fmt.Sprintf("✓  Scraped %s stargazers in %dm %ds",
		formatCount(m.count), mins, secs))
	b.WriteString(doneBoxStyle.Render(summaryContent) + "\n\n")

	// Email Sources breakdown
	b.WriteString(sectionDivider("Email Sources", cw) + "\n\n")

	total := m.count
	if total == 0 {
		total = 1
	}

	barWidth := 24
	renderBar := func(icon, label string, count int, sty lipgloss.Style) string {
		pct := float64(count) / float64(total)
		filled := int(pct * float64(barWidth))
		if filled > barWidth {
			filled = barWidth
		}
		bar := lipgloss.NewStyle().Foreground(clrPrimary).Render(strings.Repeat("█", filled)) +
			dimStyle.Render(strings.Repeat("░", barWidth-filled))
		return fmt.Sprintf("  %s  %-8s  %s  %5d  %s",
			sty.Render(icon),
			sty.Render(label),
			bar,
			count,
			dimStyle.Render(fmt.Sprintf("(%.1f%%)", pct*100)),
		)
	}

	b.WriteString(renderBar("✓", "profile", m.stats.profile, okStyle) + "\n")
	b.WriteString(renderBar("✓", "commit", m.stats.commit, okStyle) + "\n")
	b.WriteString(renderBar("⚠", "noreply", m.stats.noreply, warnStyle) + "\n")
	if m.stats.failed > 0 {
		b.WriteString(renderBar("✗", "errors", m.stats.failed, errorStyle) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(
		"  " + dimStyle.Render("Output: ") +
			lipgloss.NewStyle().Foreground(clrCyan).Underline(true).Render(m.outputPath) +
			"\n\n",
	)

	// Scrollable full results
	b.WriteString(sectionDivider("All Results", cw) + "\n")
	b.WriteString(hintStyle.Render("  ↑↓ / PgUp / PgDn to scroll") + "\n")
	b.WriteString(vpBorderStyle.Render(m.vp.View()) + "\n")

	b.WriteString("\n" +
		hintStyle.Render("  ") +
		cyanStyle.Render("Enter / q / Ctrl+C") +
		hintStyle.Render(" to exit"),
	)

	return appStyle.Render(b.String())
}

func (m Model) errorView() string {
	cw := m.contentWidth()
	var b strings.Builder

	b.WriteString(renderHeader(cw) + "\n\n")

	errContent := errorStyle.Render("✗  Error") + "\n\n" +
		lipgloss.NewStyle().Foreground(clrSubtle).Render(m.errMsg)
	b.WriteString(errorBoxStyle.Render(errContent) + "\n\n")

	b.WriteString(
		hintStyle.Render("  ") +
			cyanStyle.Render("Enter / q / Ctrl+C") +
			hintStyle.Render(" to exit"),
	)

	return appStyle.Render(b.String())
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func buildResultLine(r userResult) string {
	var icon string
	var iconSty, emailSty, srcSty lipgloss.Style

	switch {
	case r.fetchFailed:
		icon = "✗"
		iconSty = errorStyle
		emailSty = dimStyle
		srcSty = errorStyle
	case r.emailSource == "noreply":
		icon = "⚠"
		iconSty = warnStyle
		emailSty = dimStyle
		srcSty = warnStyle
	case r.emailSource == "profile":
		icon = "✓"
		iconSty = okStyle
		emailSty = lipgloss.NewStyle().Foreground(clrSuccess)
		srcSty = dimStyle
	default: // commit
		icon = "✓"
		iconSty = cyanStyle
		emailSty = cyanStyle
		srcSty = dimStyle
	}

	email := r.email
	src := r.emailSource
	if r.fetchFailed {
		email = "(fetch failed)"
		src = "error"
	}

	loginPart := hintStyle.Render(fmt.Sprintf("%-22s", "@"+r.login))
	emailPart := emailSty.Render(fmt.Sprintf("%-50s", email))
	srcPart := srcSty.Render("[" + src + "]")

	return fmt.Sprintf("%s  %s  %s  %s",
		iconSty.Render(icon),
		loginPart,
		emailPart,
		srcPart,
	)
}

func formatCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}

func clampInt(s string, def, min, max int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
