package ui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
// Styles
// --------------------------------------------------------------------------

var (
	purple  = lipgloss.Color("#7C3AED")
	green   = lipgloss.Color("#10B981")
	yellow  = lipgloss.Color("#F59E0B")
	red     = lipgloss.Color("#EF4444")
	gray    = lipgloss.Color("#6B7280")
	dimGray = lipgloss.Color("#9CA3AF")

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(purple)

	labelStyle  = lipgloss.NewStyle().Foreground(gray)
	activeLabel = lipgloss.NewStyle().Foreground(purple).Bold(true)
	hintStyle   = lipgloss.NewStyle().Foreground(dimGray)
	warnStyle   = lipgloss.NewStyle().Foreground(yellow)
	errorStyle  = lipgloss.NewStyle().Foreground(red).Bold(true)
	okStyle     = lipgloss.NewStyle().Foreground(green).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(dimGray)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(purple).
			Padding(1, 3)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(purple).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(gray)

	vpBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(gray)
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
//
// The logical field ordering is:
//   0          - repo
//   1..N       - token fields (dynamic)
//   N+1        - output CSV path
//   N+2        - concurrency
//   N+3        - max repos
//   N+4        - max stars
//   N+5        - delay
//   N+6        - commit search API
//
// Config field indices relative to the start of the config section:
// --------------------------------------------------------------------------

const (
	maxTokenFields = 10
	minTokenFields = 1

	// Config section offsets (relative to configStart()).
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
	emailSource string // "profile", "commit", "noreply"
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
	state   appState
	repoInput    textinput.Model
	tokenInputs  []textinput.Model
	configInputs [numConfigFields]textinput.Model
	focused int // logical index: 0=repo, 1..N=tokens, N+1..=config fields

	spinner     spinner.Model
	progressBar progress.Model
	vp          viewport.Model
	vpContent   string // accumulates result lines for the viewport

	progressCh chan scr.Progress

	// Running state
	lastStatus  string
	userResults []userResult
	stats       runStats
	total       int
	current     int
	startTime   time.Time

	// Done state
	outputPath string
	count      int
	elapsed    time.Duration

	// Error state
	errMsg   string
	repoWarn string // inline warning below the repo field

	width  int
	height int
}

// totalFields returns the current total number of logical fields.
func (m *Model) totalFields() int {
	return 1 + len(m.tokenInputs) + numConfigFields
}

// configStart returns the logical index of the first config field.
func (m *Model) configStart() int {
	return 1 + len(m.tokenInputs)
}

// isTokenFocused returns true when the focused field is one of the token fields.
func (m *Model) isTokenFocused() bool {
	f := m.focused
	return f >= 1 && f <= len(m.tokenInputs)
}

// tokenLabel returns the display label for the i-th token (0-based).
func tokenLabel(i int) string {
	if i == 0 {
		return "GitHub Token 1   (saved to ~/.config/stargazer/credentials.json)"
	}
	return fmt.Sprintf("GitHub Token %d", i+1)
}

// newMaskedInput creates a new masked token textinput.
func newMaskedInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "ghp_..."
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.CharLimit = 200
	return ti
}

func NewModel() Model {
	// Repo field
	repoInput := textinput.New()
	repoInput.Placeholder = "owner/repo"
	repoInput.SetValue("facebook/react")
	repoInput.CharLimit = 200
	repoInput.Focus()

	// Start with one token field; more may be added from creds.
	var tokenInputs []textinput.Model

	// Config fields
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
	configInputs[cfgUseSearch].CharLimit = 1

	// Load saved tokens from credentials file.
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

	// Always have at least one token field.
	if len(tokenInputs) == 0 {
		tokenInputs = append(tokenInputs, newMaskedInput())
	}

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(purple)

	pb := progress.New(
		progress.WithGradient("#7C3AED", "#10B981"),
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
			// Forward scroll keys to viewport
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
			// In done state let user scroll from the top
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

		if r := p.Result; r != nil {
			ur := userResult{
				login:       r.Login,
				email:       r.Email,
				emailSource: r.EmailSource,
				fetchFailed: r.FetchFailed,
			}
			m.userResults = append(m.userResults, ur)

			// Update running stats
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

			// Append line to viewport content and auto-scroll
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

// blurAll blurs every input field.
func (m *Model) blurAll() {
	m.repoInput.Blur()
	for i := range m.tokenInputs {
		m.tokenInputs[i].Blur()
	}
	for i := range m.configInputs {
		m.configInputs[i].Blur()
	}
}

// focusCurrent focuses the input at m.focused.
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

	// Handle token add/remove when a token field is focused.
	if m.isTokenFocused() {
		switch msg.String() {
		case "ctrl+n":
			if len(m.tokenInputs) < maxTokenFields {
				m.blurAll()
				m.tokenInputs = append(m.tokenInputs, newMaskedInput())
				m.focused = len(m.tokenInputs) // point to newly added token (1-based)
				m.focusCurrent()
			}
			return m, textinput.Blink

		case "ctrl+x":
			if len(m.tokenInputs) > minTokenFields {
				m.blurAll()
				m.tokenInputs = m.tokenInputs[:len(m.tokenInputs)-1]
				// Clamp focused index.
				if m.focused > len(m.tokenInputs) {
					m.focused = len(m.tokenInputs)
				}
				m.focusCurrent()
			}
			return m, textinput.Blink
		}
	}

	// Recalculate total after any add/remove (in case keys above changed length).
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

	// Forward keypress to the focused input.
	var cmd tea.Cmd
	cfgStart := m.configStart()
	switch {
	case m.focused == 0:
		m.repoInput, cmd = m.repoInput.Update(msg)
		// Normalise GitHub URLs pasted into the field.
		raw := m.repoInput.Value()
		normalised, warn := parseRepoInput(raw)
		m.repoWarn = warn
		if normalised != raw {
			m.repoInput.SetValue(normalised)
		}
		// Auto-sync output path when the repo field changes.
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

// parseRepoInput normalises a GitHub URL or owner/repo string into "owner/repo".
// Returns the normalised value and a warning message (empty if the format is fine).
func parseRepoInput(raw string) (string, string) {
	s := strings.TrimSpace(raw)
	// Strip common URL prefixes.
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
	// Strip trailing .git or trailing slash.
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return s, ""
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return s, "Format must be owner/repo (e.g. torvalds/linux)"
	}
	// Discard anything after the second segment (e.g. /tree/main).
	return parts[0] + "/" + parts[1], ""
}

func (m Model) submit() (tea.Model, tea.Cmd) {
	repo, warn := parseRepoInput(m.repoInput.Value())
	if warn != "" {
		m.errMsg = warn
		m.state = stateError
		return m, nil
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		m.errMsg = "Repository must be in owner/repo format (e.g. facebook/react)"
		m.state = stateError
		return m, nil
	}

	// Collect non-empty tokens and save to credentials.
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

func (m Model) formView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("  GitHub Star Scraper") + "\n")
	b.WriteString(hintStyle.Render("Scrape stargazers and extract their email addresses") + "\n\n")

	// --- Repo field ---
	if m.focused == 0 {
		b.WriteString(activeLabel.Render("▶ Repository (owner/repo)") + "\n")
	} else {
		b.WriteString(labelStyle.Render("  Repository (owner/repo)") + "\n")
	}
	b.WriteString(m.repoInput.View() + "\n")
	if m.repoWarn != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("  ⚠ "+m.repoWarn) + "\n")
	}
	b.WriteString("\n")

	// --- Token fields ---
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

	// Token add/remove hint (when any token field is focused).
	if m.isTokenFocused() {
		b.WriteString(hintStyle.Render("  Ctrl+N add token  •  Ctrl+X remove last") + "\n\n")
	}

	// Token creation guide hint (always shown after last token field).
	b.WriteString(hintStyle.Render("  Create tokens at: github.com/settings/tokens  →  Fine-grained or Classic PAT, no special scopes needed for public repos") + "\n\n")

	// Token count status
	activeTokens := 0
	for _, ti := range m.tokenInputs {
		if strings.TrimSpace(ti.Value()) != "" {
			activeTokens++
		}
	}
	if activeTokens == 0 {
		b.WriteString(warnStyle.Render("  ⚠  No token set – rate limit is 60 req/hr. Large repos will be very slow.") + "\n\n")
	} else if activeTokens > 1 {
		b.WriteString(okStyle.Render(fmt.Sprintf("  ✓  %d tokens – will rotate on rate-limit", activeTokens)) + "\n\n")
	}

	// --- Config fields ---
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

	b.WriteString(hintStyle.Render("  Tab / ↑↓ navigate  •  Enter advances field  •  Enter on last field starts scraping  •  Ctrl+C quit"))

	return boxStyle.Render(b.String())
}

func (m Model) runningView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("  GitHub Star Scraper") + "\n\n")

	if m.total == 0 {
		// Still in fetch stage
		b.WriteString(m.spinner.View() + "  " + hintStyle.Render(m.lastStatus) + "\n\n")
	} else {
		// Processing stage
		pct := 0.0
		if m.total > 0 {
			pct = float64(m.current) / float64(m.total)
		}
		elapsed := time.Since(m.startTime).Round(time.Second)

		b.WriteString(m.spinner.View() + "  " + lipgloss.NewStyle().Foreground(purple).Bold(true).Render(
			fmt.Sprintf("Processing  %d / %d", m.current, m.total),
		) + "  " + dimStyle.Render(elapsed.String()) + "\n")

		b.WriteString(m.progressBar.ViewAs(pct) + "\n\n")

		// Stats row
		b.WriteString(
			okStyle.Render(fmt.Sprintf("  ✓ profile:%-5d", m.stats.profile)) +
				okStyle.Render(fmt.Sprintf("commit:%-5d", m.stats.commit)) +
				warnStyle.Render(fmt.Sprintf("noreply:%-5d", m.stats.noreply)) +
				errorStyle.Render(fmt.Sprintf("errors:%-4d", m.stats.failed)) +
				"\n\n",
		)
	}

	// Live results viewport
	if len(m.userResults) > 0 {
		b.WriteString(hintStyle.Render("  ↑↓ scroll results") + "\n")
		b.WriteString(vpBorderStyle.Render(m.vp.View()) + "\n")
	}

	b.WriteString("\n" + hintStyle.Render("  q / Ctrl+C to quit"))

	return boxStyle.Render(b.String())
}

func (m Model) doneView() string {
	var b strings.Builder

	mins := int(m.elapsed.Minutes())
	secs := int(m.elapsed.Seconds()) % 60

	b.WriteString(titleStyle.Render("  GitHub Star Scraper") + "\n\n")
	b.WriteString(okStyle.Render(fmt.Sprintf("  ✓  Done! Scraped %d stargazers in %dm %ds", m.count, mins, secs)) + "\n\n")

	// Email source breakdown table
	b.WriteString(headerStyle.Render("  Email Sources") + "\n\n")

	total := m.count
	if total == 0 {
		total = 1
	}

	renderBar := func(label string, count int, sty lipgloss.Style) string {
		pct := float64(count) / float64(total)
		filled := int(pct * 20)
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
		return fmt.Sprintf("  %-10s %s  %4d  (%.1f%%)",
			sty.Render(label),
			sty.Render(bar),
			count,
			pct*100,
		)
	}

	b.WriteString(renderBar("profile", m.stats.profile, okStyle) + "\n")
	b.WriteString(renderBar("commit", m.stats.commit, okStyle) + "\n")
	b.WriteString(renderBar("noreply", m.stats.noreply, warnStyle) + "\n")
	if m.stats.failed > 0 {
		b.WriteString(renderBar("errors", m.stats.failed, errorStyle) + "\n")
	}

	b.WriteString("\n")
	b.WriteString("  Output: " + lipgloss.NewStyle().Foreground(purple).Underline(true).Render(m.outputPath) + "\n\n")

	// Scrollable full results
	b.WriteString(hintStyle.Render("  All results (↑↓ / PgUp / PgDn to scroll):") + "\n")
	b.WriteString(vpBorderStyle.Render(m.vp.View()) + "\n")

	b.WriteString("\n" + hintStyle.Render("  Enter / q / Ctrl+C to exit"))

	return boxStyle.Render(b.String())
}

func (m Model) errorView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("  GitHub Star Scraper") + "\n\n")
	b.WriteString(errorStyle.Render("  ✗  Error") + "\n\n")
	b.WriteString("  " + m.errMsg + "\n\n")
	b.WriteString(hintStyle.Render("  Enter / q / Ctrl+C to exit"))

	return boxStyle.Render(b.String())
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func buildResultLine(r userResult) string {
	var icon string
	var iconSty, srcSty lipgloss.Style

	switch {
	case r.fetchFailed:
		icon = "✗"
		iconSty = errorStyle
		srcSty = errorStyle
	case r.emailSource == "noreply":
		icon = "⚠"
		iconSty = warnStyle
		srcSty = warnStyle
	case r.emailSource == "profile":
		icon = "✓"
		iconSty = okStyle
		srcSty = dimStyle
	default: // commit
		icon = "✓"
		iconSty = lipgloss.NewStyle().Foreground(green)
		srcSty = dimStyle
	}

	email := r.email
	src := r.emailSource
	if r.fetchFailed {
		email = "(fetch failed)"
		src = "error"
	}

	return fmt.Sprintf("%s  %s  %s  %s",
		iconSty.Render(icon),
		hintStyle.Render(fmt.Sprintf("%-22s", "@"+r.login)),
		fmt.Sprintf("%-50s", email),
		srcSty.Render("["+src+"]"),
	)
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
