package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

	"clawsynapse/internal/api"
	"clawsynapse/internal/protocol"
	"clawsynapse/pkg/types"
)

const (
	dashboardRefreshInterval  = 3 * time.Second
	dashboardMessagesLimit    = 12
	dashboardTabCount         = 4
	dashboardMinWidth         = 60
	dashboardMinHeight        = 20
	dashboardNarrowBreakpoint = 100
	dashboardPageStep         = 6
	dashboardSpinnerInterval  = 100 * time.Millisecond
)

// Unicode box-drawing characters.
const (
	boxTL = "╭"
	boxTR = "╮"
	boxBL = "╰"
	boxBR = "╯"
	boxH  = "─"
	boxV  = "│"
	boxHH = "━" // heavy horizontal
)

// Visual indicators.
const (
	dotFull  = "●"
	dotHalf  = "◐"
	dotEmpty = "○"
	arrowUp  = "▲"
	arrowDn  = "▼"
	arrowR   = "▸"
	diamond  = "◈"
)

// Spinner braille frames.
var spinnerFrames = []rune{'⣾', '⣽', '⣻', '⢿', '⡿', '⣟', '⣯', '⣷'}

// Tag names for styled lines.
const (
	dashboardTagAccent   = "accent"
	dashboardTagMuted    = "muted"
	dashboardTagSection  = "section"
	dashboardTagGood     = "good"
	dashboardTagWarn     = "warn"
	dashboardTagBad      = "bad"
	dashboardTagSelected = "selected"
	dashboardTagDim      = "dim"
	dashboardTagLogInfo  = "log_info"
	dashboardTagLogWarn  = "log_warn"
	dashboardTagLogError = "log_error"
	dashboardTagLogDebug = "log_debug"
)

var dashboardANSIRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ---------------------------------------------------------------------------
// Interfaces & data types
// ---------------------------------------------------------------------------

type dashboardClient interface {
	Get(ctx context.Context, endpoint string) (types.APIResult, error)
}

type dashboardConfig struct {
	APIAddr string
	Timeout time.Duration
}

type dashboardSnapshot struct {
	Health   dashboardHealth
	Peers    []types.Peer
	Messages []protocol.MessageEnvelope
	Logs     string
	Updated  time.Time
}

type dashboardHealth struct {
	PeersCount int
	NATS       dashboardNATS
}

type dashboardNATS struct {
	Name             string
	ServerURL        string
	Connected        bool
	Status           string
	Disconnects      int64
	Reconnects       int64
	InMsgs           int64
	OutMsgs          int64
	InBytes          int64
	OutBytes         int64
	LastError        string
	ConnectedAt      int64
	LastDisconnectAt int64
	LastReconnectAt  int64
}

type dashboardRefreshMsg struct {
	snapshot dashboardSnapshot
	err      error
}

type spinnerTickMsg struct{}

// dashboardLayout holds pre-computed layout dimensions for the current
// terminal size. Recalculated on every WindowSizeMsg.
type dashboardLayout struct {
	width  int
	height int
	narrow bool // true when width < dashboardNarrowBreakpoint

	bodyHeight int

	// Split-panel dimensions (3:2 ratio, for Peers / Messages / Overview).
	splitLeftW  int
	splitRightW int

	// Logs split-panel dimensions (7:3 ratio).
	logsLeftW  int
	logsRightW int

	// Overview card width.
	cardWidth int
}

type dashboardPanelTheme struct {
	border func(string) string
	title  func(string) string
}

type dashboardStyledLine struct {
	text string
	tag  string
}

type parsedLogEntry struct {
	Time    string
	Level   string
	Msg     string
	NodeID  string
	Service string
	Comp    string // component
	Event   string
	Peer    string
	From    string
	To      string
	MsgID   string   // messageId
	ReqID   string   // requestId
	SessKey string   // sessionKey
	Err     string   // error
	Extra   []string // remaining key=value pairs
	Raw     string
	IsJSON  bool
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type dashboardModel struct {
	client         dashboardClient
	logs           logProvider
	apiAddr        string
	timeout        time.Duration
	width          int
	height         int
	layout         dashboardLayout
	activeTab      int
	lastUpdated    time.Time
	snapshot       dashboardSnapshot
	loading        bool
	errText        string
	cursors        [dashboardTabCount]int
	offsets        [dashboardTabCount]int
	spinnerFrame   int
	logsFollowTail bool
}

func (m *dashboardModel) recalcLayout() {
	w := m.width
	h := m.height
	narrow := w < dashboardNarrowBreakpoint

	lo := dashboardLayout{
		width:  w,
		height: h,
		narrow: narrow,
	}

	if narrow {
		lo.splitLeftW = w
		lo.splitRightW = w
		lo.logsLeftW = w
		lo.logsRightW = w
	} else {
		lo.splitLeftW = w * 3 / 5
		lo.splitRightW = w - lo.splitLeftW - 1
		lo.logsLeftW = w * 7 / 10
		lo.logsRightW = w - lo.logsLeftW - 1
	}

	// Overview cards: 4 cards + 3 gaps of 1 char.
	lo.cardWidth = maxInt(18, (w-3)/4)

	m.layout = lo
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

func runDashboard(args []string, stdout, stderr *os.File) error {
	cfg, err := parseDashboardArgs(args, stderr)
	if err != nil {
		return err
	}

	model := dashboardModel{
		client:         api.NewClient(cfg.APIAddr, cfg.Timeout),
		logs:           defaultLogProvider{runner: execServiceRunner{}},
		apiAddr:        cfg.APIAddr,
		timeout:        cfg.Timeout,
		loading:        true,
		logsFollowTail: true,
	}

	p := tea.NewProgram(model)
	_, err = p.Run()
	return err
}

func parseDashboardArgs(args []string, stderr io.Writer) (dashboardConfig, error) {
	defaultAPIAddr := strings.TrimSpace(os.Getenv("LOCAL_API_ADDR"))
	if defaultAPIAddr == "" {
		defaultAPIAddr = "127.0.0.1:18080"
	}

	cfg := dashboardConfig{
		APIAddr: defaultAPIAddr,
		Timeout: 5 * time.Second,
	}

	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.APIAddr, "api-addr", cfg.APIAddr, "local API address")
	fs.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "local API timeout")
	if err := fs.Parse(args); err != nil {
		return dashboardConfig{}, err
	}
	if len(fs.Args()) > 0 {
		return dashboardConfig{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Tea lifecycle
// ---------------------------------------------------------------------------

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), dashboardTickCmd(), spinnerTickCmd())
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalcLayout()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab", "right", "l":
			m.activeTab = (m.activeTab + 1) % dashboardTabCount
		case "shift+tab", "left", "h":
			m.activeTab--
			if m.activeTab < 0 {
				m.activeTab = dashboardTabCount - 1
			}
		case "1":
			m.activeTab = 0
		case "2":
			m.activeTab = 1
		case "3":
			m.activeTab = 2
		case "4":
			m.activeTab = 3
		case "down", "j":
			m.moveSelection(1)
		case "up", "k":
			m.moveSelection(-1)
		case "pgdown", "d":
			m.moveSelection(dashboardPageStep)
		case "pgup", "u":
			m.moveSelection(-dashboardPageStep)
		case "G":
			if m.activeTab == 3 {
				m.logsFollowTail = true
			}
		case "g":
			if m.activeTab == 3 {
				m.offsets[3] = 0
				m.logsFollowTail = false
			}
		case "r":
			m.loading = true
			return m, m.refreshCmd()
		}
	case dashboardRefreshMsg:
		m.loading = false
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		m.snapshot = msg.snapshot
		m.lastUpdated = msg.snapshot.Updated
		m.errText = ""
		m.clampSelections()
	case spinnerTickMsg:
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		return m, spinnerTickCmd()
	case time.Time:
		if m.loading {
			return m, dashboardTickCmd()
		}
		m.loading = true
		return m, tea.Batch(m.refreshCmd(), dashboardTickCmd())
	}

	return m, nil
}

func (m dashboardModel) View() tea.View {
	// Show a friendly message when the terminal is too small.
	if m.width > 0 && m.width < dashboardMinWidth || m.height > 0 && m.height < dashboardMinHeight {
		hint := fmt.Sprintf("Terminal too small (%dx%d). Need at least %dx%d.\nPress q to quit.",
			m.width, m.height, dashboardMinWidth, dashboardMinHeight)
		v := tea.NewView(hint)
		v.AltScreen = true
		return v
	}

	width := m.width
	height := m.height
	if width == 0 {
		width = dashboardMinWidth
	}
	if height == 0 {
		height = dashboardMinHeight
	}

	// Ensure layout is in sync (handles direct model construction in tests).
	if m.layout.width != width || m.layout.height != height {
		m.width = width
		m.height = height
		m.recalcLayout()
	}

	header := m.headerView(width)
	tabs := m.tabsView(width)
	footer := m.footerView(width)
	bodyHeight := maxInt(8, height-countLines(header)-countLines(tabs)-countLines(footer))

	full := strings.Join([]string{
		header,
		tabs,
		m.bodyView(width, bodyHeight),
		footer,
	}, "\n")

	v := tea.NewView(full)
	v.AltScreen = true
	return v
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

func (m dashboardModel) headerView(width int) string {
	// Status indicator.
	statusIcon := dotFull
	statusLabel := "READY"
	if m.loading {
		statusIcon = string(spinnerFrames[m.spinnerFrame])
		statusLabel = "SYNCING"
	}
	if m.errText != "" {
		statusIcon = dotEmpty
		statusLabel = "ERROR"
	}

	updated := "never"
	if !m.lastUpdated.IsZero() {
		updated = m.lastUpdated.Format("15:04:05")
	}

	// NATS indicator.
	natsIcon := dotFull
	natsTag := dashboardTagGood
	if !m.snapshot.Health.NATS.Connected {
		natsIcon = dotEmpty
		natsTag = dashboardTagBad
	}

	var lines []string
	if m.layout.narrow {
		// Narrow: split header info across multiple lines.
		lines = []string{
			taggedLine(dashboardTagAccent, fmt.Sprintf("%s %s %s %s %s",
				diamond, version, boxV, statusIcon, statusLabel)),
			taggedLine(dashboardTagAccent, fmt.Sprintf("API %s %s Updated %s",
				m.apiAddr, boxV, updated)),
			fmt.Sprintf("NATS %s %s %s Peers %d",
				taggedLine(natsTag, natsIcon),
				taggedLine(natsTag, m.natsStatusLabel()),
				boxV, len(m.snapshot.Peers)),
			fmt.Sprintf("%s %d %s %d msgs",
				arrowUp, m.snapshot.Health.NATS.InMsgs,
				arrowDn, m.snapshot.Health.NATS.OutMsgs),
		}
	} else {
		lines = []string{
			taggedLine(dashboardTagAccent, fmt.Sprintf(
				"%s %s %s API %s %s %s %s %s Updated %s",
				diamond, version, boxV, m.apiAddr, boxV, statusIcon, statusLabel, boxV, updated,
			)),
			fmt.Sprintf(
				"NATS %s %s %s Peers %d %s %s %d %s %d msgs %s Press r to refresh",
				taggedLine(natsTag, natsIcon),
				taggedLine(natsTag, m.natsStatusLabel()),
				boxV, len(m.snapshot.Peers), boxV,
				arrowUp, m.snapshot.Health.NATS.InMsgs,
				arrowDn, m.snapshot.Health.NATS.OutMsgs, boxV,
			),
		}
	}
	if m.errText != "" {
		lines = append(lines, taggedLine(dashboardTagBad, "Last refresh failed: "+truncateRight(m.errText, maxInt(12, width-22))))
	}
	return renderPanelWithTheme("ClawSynapse Dashboard", width, panelHeightForLines(width, lines), lines, dashboardHeaderTheme())
}

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------

func (m dashboardModel) tabsView(width int) string {
	fullLabels := []string{"Overview", "Peers", "Messages", "Logs"}
	shortLabels := []string{"Ovw", "Prs", "Msg", "Log"}
	nums := []string{"1", "2", "3", "4"}

	// Choose labels based on available width.
	labels := fullLabels
	sep := "  "
	// Estimate full width: each cell " N Label " + separators.
	estWidth := 0
	for _, l := range fullLabels {
		estWidth += displayWidth(" "+nums[0]+" "+l+" ") + 2
	}
	if estWidth > width {
		labels = shortLabels
		sep = " "
	}

	parts := make([]string, 0, len(labels))
	underParts := make([]string, 0, len(labels))

	for i, label := range labels {
		cell := " " + nums[i] + " " + label + " "
		cellWidth := displayWidth(cell)
		if i == m.activeTab {
			parts = append(parts, dashboardStyleTabActive(cell))
			underParts = append(underParts, dashboardStyleAccent(strings.Repeat(boxHH, cellWidth)))
		} else {
			parts = append(parts, dashboardStyleTabInactive(cell))
			underParts = append(underParts, strings.Repeat(" ", cellWidth))
		}
	}

	line1 := strings.Join(parts, sep)
	line2 := strings.Join(underParts, sep)
	return truncateRightVisible(line1, width) + "\n" + truncateRightVisible(line2, width)
}

// ---------------------------------------------------------------------------
// Body dispatch
// ---------------------------------------------------------------------------

func (m dashboardModel) bodyView(width, height int) string {
	switch m.activeTab {
	case 0:
		return m.overviewView(width, height)
	case 1:
		return m.peersView(width, height)
	case 2:
		return m.messagesView(width, height)
	case 3:
		return m.logsView(width, height)
	default:
		return renderPanel("Dashboard", width, height, []string{taggedLine(dashboardTagBad, "unknown view")})
	}
}

// ---------------------------------------------------------------------------
// Overview tab
// ---------------------------------------------------------------------------

func (m dashboardModel) overviewView(width, height int) string {
	lo := m.layout
	narrow := lo.narrow

	// Cards: ensure total width = 4*cardWidth + 3 gaps <= width.
	cardWidth := lo.cardWidth
	totalCardsW := cardWidth*4 + 3
	if totalCardsW > width {
		cardWidth = maxInt(18, (width-3)/4)
	}

	cards := []string{
		renderMetricPanel("Peers", fmt.Sprintf("%d", len(m.snapshot.Peers)), "discovered nodes", cardWidth),
		renderMetricPanel("NATS", boolWord(m.snapshot.Health.NATS.Connected, "connected", "offline"), fallbackString(m.snapshot.Health.NATS.Status, "unknown"), cardWidth),
		renderMetricPanel("Inbound", fmt.Sprintf("%d msgs", m.snapshot.Health.NATS.InMsgs), formatBytes(m.snapshot.Health.NATS.InBytes), cardWidth),
		renderMetricPanel("Outbound", fmt.Sprintf("%d msgs", m.snapshot.Health.NATS.OutMsgs), formatBytes(m.snapshot.Health.NATS.OutBytes), cardWidth),
	}
	top := joinHorizontal(cards, 1)
	topLines := countLines(top)
	remaining := maxInt(8, height-topLines)

	if narrow {
		natsH := maxInt(8, remaining/2)
		actH := maxInt(8, remaining-natsH)
		left := renderPanel("NATS", width, natsH, m.overviewNATSLines())
		right := renderPanel("Activity", width, actH, m.overviewActivityLines())
		return strings.Join([]string{top, left, right}, "\n")
	}

	left := renderPanel("NATS", lo.splitLeftW, remaining, m.overviewNATSLines())
	right := renderPanel("Activity", lo.splitRightW, remaining, m.overviewActivityLines())
	return strings.Join([]string{top, joinHorizontal([]string{left, right}, 1)}, "\n")
}

func (m dashboardModel) overviewNATSLines() []string {
	h := m.snapshot.Health.NATS
	connDot := dotFull
	connTag := dashboardTagGood
	if !h.Connected {
		connDot = dotEmpty
		connTag = dashboardTagBad
	}
	lines := []string{
		taggedLine(dashboardTagSection, arrowR+" Connection"),
		fmt.Sprintf("  server    %s", fallbackString(h.ServerURL, "-")),
		fmt.Sprintf("  name      %s", fallbackString(h.Name, "-")),
		taggedLine(connTag, fmt.Sprintf("  status    %s %s", connDot, fallbackString(h.Status, "unknown"))),
		taggedLine(connTag, fmt.Sprintf("  connected %s", boolWord(h.Connected, "yes", "no"))),
		"",
		taggedLine(dashboardTagSection, arrowR+" Statistics"),
		fmt.Sprintf("  reconnects       %d", h.Reconnects),
		fmt.Sprintf("  disconnects      %d", h.Disconnects),
		fmt.Sprintf("  connectedAt      %s", formatUnixMilli(h.ConnectedAt)),
		fmt.Sprintf("  lastReconnectAt  %s", formatUnixMilli(h.LastReconnectAt)),
		fmt.Sprintf("  lastDisconnectAt %s", formatUnixMilli(h.LastDisconnectAt)),
	}
	if strings.TrimSpace(h.LastError) != "" {
		lines = append(lines, "", taggedLine(dashboardTagBad, arrowR+" Last Error"), taggedLine(dashboardTagBad, "  "+h.LastError))
	}
	return lines
}

func (m dashboardModel) overviewActivityLines() []string {
	lines := []string{
		taggedLine(dashboardTagSection, arrowR+" Peer State"),
		taggedLine(dashboardTagGood, fmt.Sprintf("  %s authenticated  %d", dotFull, countPeersByAuth(m.snapshot.Peers, types.AuthAuthenticated))),
		taggedLine(dashboardTagWarn, fmt.Sprintf("  %s auth pending   %d", dotHalf, countPeersByAuth(m.snapshot.Peers, types.AuthPending))),
		taggedLine(dashboardTagGood, fmt.Sprintf("  %s trusted        %d", dotFull, countPeersByTrust(m.snapshot.Peers, types.TrustTrusted))),
		taggedLine(dashboardTagWarn, fmt.Sprintf("  %s trust pending  %d", dotHalf, countPeersByTrust(m.snapshot.Peers, types.TrustPending))),
		"",
		taggedLine(dashboardTagSection, arrowR+" Recent Messages"),
	}

	if len(m.snapshot.Messages) == 0 {
		lines = append(lines, taggedLine(dashboardTagMuted, "  no recent messages"))
		return lines
	}

	for i, item := range m.snapshot.Messages {
		if i >= 6 {
			lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("  ... %d more", len(m.snapshot.Messages)-i)))
			break
		}
		lines = append(lines, taggedLine(dashboardTagAccent, fmt.Sprintf("  %s %s %s %s", arrowR, compactType(item.Type), fallbackString(item.From, "-"), fallbackString(item.To, "-"))))
		lines = append(lines, taggedLine(dashboardTagMuted, "    "+truncateRight(strings.TrimSpace(item.Content), 48)))
	}
	return lines
}

// ---------------------------------------------------------------------------
// Peers tab
// ---------------------------------------------------------------------------

func (m dashboardModel) peersView(width, height int) string {
	lo := m.layout

	if lo.narrow {
		listH := maxInt(8, height*3/5)
		detailH := maxInt(8, height-listH-1)
		list := renderPanel("Peers", width, listH, m.peerListLines(maxInt(1, listH-3)))
		detail := renderPanel("Peer Detail", width, detailH, m.selectedPeerLines())
		return strings.Join([]string{list, detail}, "\n")
	}

	list := renderPanel("Peers", lo.splitLeftW, height, m.peerListLines(maxInt(1, height-3)))
	detail := renderPanel("Peer Detail", lo.splitRightW, height, m.selectedPeerLines())
	return joinHorizontal([]string{list, detail}, 1)
}

func (m dashboardModel) peerListLines(contentHeight int) []string {
	lines := []string{
		taggedLine(dashboardTagDim, formatPeerListHeader()),
	}
	if len(m.snapshot.Peers) == 0 {
		return append(lines, taggedLine(dashboardTagMuted, "  no peers discovered"))
	}

	visibleRows := maxInt(1, contentHeight-1)
	offset := ensureVisible(m.offsets[1], m.cursors[1], len(m.snapshot.Peers), visibleRows)
	for row := offset; row < len(m.snapshot.Peers) && len(lines) < contentHeight; row++ {
		peer := m.snapshot.Peers[row]
		prefix := " "
		tag := ""
		if row == m.cursors[1] {
			prefix = arrowR
			tag = dashboardTagSelected
		}
		lines = append(lines, taggedLine(tag, prefix+formatPeerRow(peer)))
	}
	if offset+visibleRows < len(m.snapshot.Peers) {
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("  ... %d more peers", len(m.snapshot.Peers)-(offset+visibleRows))))
	}
	return lines
}

func (m dashboardModel) selectedPeerLines() []string {
	if len(m.snapshot.Peers) == 0 {
		return []string{taggedLine(dashboardTagMuted, "No peer selected."), "", "Use tab to move between views."}
	}
	peer := m.snapshot.Peers[m.cursors[1]]
	authDot := peerAuthDot(peer.AuthStatus)
	trustDot := peerTrustDot(peer.TrustStatus)
	return []string{
		taggedLine(dashboardTagAccent, fmt.Sprintf("  nodeId  %s", peer.NodeID)),
		taggedLine(dashboardPeerAuthTag(peer.AuthStatus), fmt.Sprintf("  auth    %s %s", authDot, fallbackString(peer.AuthStatus, "unknown"))),
		taggedLine(dashboardPeerTrustTag(peer.TrustStatus), fmt.Sprintf("  trust   %s %s", trustDot, fallbackString(peer.TrustStatus, "none"))),
		fmt.Sprintf("  product %s", fallbackString(peer.AgentProduct, "-")),
		fmt.Sprintf("  version %s", fallbackString(peer.Version, "-")),
		fmt.Sprintf("  inbox   %s", fallbackString(peer.Inbox, "-")),
		"",
		taggedLine(dashboardTagSection, arrowR+" Fleet Summary"),
		taggedLine(dashboardTagGood, fmt.Sprintf("  %s trusted peers   %d", dotFull, countPeersByTrust(m.snapshot.Peers, types.TrustTrusted))),
		taggedLine(dashboardTagWarn, fmt.Sprintf("  %s auth pending    %d", dotHalf, countPeersByAuth(m.snapshot.Peers, types.AuthPending))),
		taggedLine(dashboardTagMuted, fmt.Sprintf("  %s seen only      %d", dotEmpty, countPeersByAuth(m.snapshot.Peers, types.AuthSeen))),
	}
}

// ---------------------------------------------------------------------------
// Messages tab
// ---------------------------------------------------------------------------

func (m dashboardModel) messagesView(width, height int) string {
	lo := m.layout

	if lo.narrow {
		listH := maxInt(8, height/2)
		detailH := maxInt(8, height-listH-1)
		list := renderPanel("Messages", width, listH, m.messageListLines(maxInt(1, listH-3), maxInt(24, width-2)))
		detail := renderPanel("Message Detail", width, detailH, m.selectedMessageLines())
		return strings.Join([]string{list, detail}, "\n")
	}

	list := renderPanel("Messages", lo.splitLeftW, height, m.messageListLines(maxInt(1, height-3), maxInt(24, lo.splitLeftW-2)))
	detail := renderPanel("Message Detail", lo.splitRightW, height, m.selectedMessageLines())
	return joinHorizontal([]string{list, detail}, 1)
}

func (m dashboardModel) messageListLines(contentHeight, contentWidth int) []string {
	cols := messageColumnsForWidth(contentWidth)
	lines := []string{
		taggedLine(dashboardTagDim, formatMessageListHeader(cols)),
	}
	if len(m.snapshot.Messages) == 0 {
		return append(lines, taggedLine(dashboardTagMuted, "  no recent messages"))
	}

	visibleRows := maxInt(1, contentHeight-1)
	offset := ensureVisible(m.offsets[2], m.cursors[2], len(m.snapshot.Messages), visibleRows)
	for row := offset; row < len(m.snapshot.Messages) && len(lines) < contentHeight; row++ {
		item := m.snapshot.Messages[row]
		prefix := " "
		tag := ""
		if row == m.cursors[2] {
			prefix = arrowR
			tag = dashboardTagSelected
		}
		lines = append(lines, taggedLine(tag, formatMessageRow(prefix, item, cols)))
	}
	if offset+visibleRows < len(m.snapshot.Messages) {
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("  ... %d more messages", len(m.snapshot.Messages)-(offset+visibleRows))))
	}
	return lines
}

func (m dashboardModel) selectedMessageLines() []string {
	if len(m.snapshot.Messages) == 0 {
		return []string{taggedLine(dashboardTagMuted, "No message selected."), "", "Use j/k or arrow keys to inspect entries."}
	}
	item := m.snapshot.Messages[m.cursors[2]]
	lines := []string{
		taggedLine(dashboardTagMuted, fmt.Sprintf("  id   %s", fallbackString(item.ID, "-"))),
		taggedLine(dashboardTagAccent, fmt.Sprintf("  type %s", fallbackString(item.Type, "unknown"))),
		fmt.Sprintf("  from %s", fallbackString(item.From, "-")),
		fmt.Sprintf("  to   %s", fallbackString(item.To, "-")),
		fmt.Sprintf("  key  %s", fallbackString(item.SessionKey, "-")),
		fmt.Sprintf("  ts   %s", formatUnixMilli(item.Ts)),
		"",
		taggedLine(dashboardTagSection, arrowR+" Content"),
	}
	if strings.TrimSpace(item.Content) == "" {
		lines = append(lines, taggedLine(dashboardTagMuted, "  -"))
	} else {
		lines = append(lines, "  "+item.Content)
	}
	if len(item.Metadata) > 0 {
		lines = append(lines, "", taggedLine(dashboardTagSection, arrowR+" Metadata"))
		lines = append(lines, formatMetadata(item.Metadata)...)
	}
	return lines
}

// ---------------------------------------------------------------------------
// Logs tab
// ---------------------------------------------------------------------------

func (m dashboardModel) logsView(width, height int) string {
	lo := m.layout
	leftWidth := lo.logsLeftW
	rightWidth := lo.logsRightW

	logLines := splitLogLines(m.snapshot.Logs)
	listContentHeight := maxInt(1, height-3)

	// If following tail, snap offset to show newest lines at bottom.
	offset := m.offsets[3]
	if m.logsFollowTail && len(logLines) > listContentHeight {
		offset = len(logLines) - listContentHeight
	}
	offset = clampInt(offset, 0, maxInt(0, len(logLines)-1))
	maxVisible := maxInt(1, listContentHeight)
	if offset > maxInt(0, len(logLines)-maxVisible) {
		offset = maxInt(0, len(logLines)-maxVisible)
	}

	// Column widths for structured log fields.
	timeW := 8
	levelW := 5
	compW := 10
	innerW := maxInt(20, leftWidth-2)

	lines := make([]string, 0, listContentHeight)
	if len(logLines) == 0 {
		lines = append(lines, taggedLine(dashboardTagMuted, "  no logs available"))
	} else {
		// Header row.
		hdr := fmt.Sprintf(" %-*s %s %-*s %s %-*s %s %s",
			timeW, "TIME", boxV, levelW, "LEVEL", boxV, compW, "COMPONENT", boxV, "MESSAGE / FIELDS")
		lines = append(lines, taggedLine(dashboardTagDim, truncateRight(hdr, innerW)))

		for i := offset; i < len(logLines) && len(lines) < listContentHeight; i++ {
			entry := parseLogEntry(logLines[i])
			if !entry.IsJSON {
				lines = append(lines, taggedLine(dashboardTagMuted,
					fmt.Sprintf(" %s", truncateRight(logLines[i], maxInt(8, innerW-2)))))
				continue
			}

			levelTag := logLevelTag(entry.Level)

			// Fixed columns: time | level | component |
			timeStr := padRight(truncateRight(entry.Time, timeW), timeW)
			levelStr := padRight(strings.ToUpper(truncateRight(entry.Level, levelW)), levelW)
			compStr := padRight(truncateRight(entry.Comp, compW), compW)
			prefix := fmt.Sprintf(" %s %s %s %s %s %s ",
				timeStr, boxV, levelStr, boxV, compStr, boxV)
			prefixW := displayWidth(prefix)

			// Remaining width for message + context fields.
			msgAreaW := maxInt(10, innerW-prefixW)

			// Build context tags (key=value pairs) from known fields.
			var ctx []string
			if entry.Event != "" {
				ctx = append(ctx, "event="+entry.Event)
			}
			if entry.Peer != "" {
				ctx = append(ctx, "peer="+entry.Peer)
			}
			if entry.NodeID != "" {
				ctx = append(ctx, "nodeId="+entry.NodeID)
			}
			if entry.From != "" {
				ctx = append(ctx, "from="+entry.From)
			}
			if entry.To != "" {
				ctx = append(ctx, "to="+entry.To)
			}
			if entry.MsgID != "" {
				ctx = append(ctx, "msgId="+entry.MsgID)
			}
			if entry.ReqID != "" {
				ctx = append(ctx, "reqId="+entry.ReqID)
			}
			if entry.SessKey != "" {
				ctx = append(ctx, "sess="+entry.SessKey)
			}
			if entry.Err != "" {
				ctx = append(ctx, "err="+entry.Err)
			}
			// Append remaining extra fields.
			ctx = append(ctx, entry.Extra...)

			// Format: "msg  field1=v field2=v ..."
			msgPart := truncateRight(entry.Msg, maxInt(6, msgAreaW/2))
			ctxStr := strings.Join(ctx, " ")
			if ctxStr != "" {
				ctxAvail := maxInt(4, msgAreaW-displayWidth(msgPart)-2)
				ctxStr = truncateRight(ctxStr, ctxAvail)
				msgPart = msgPart + "  " + ctxStr
			}
			msgPart = truncateRight(msgPart, msgAreaW)

			lines = append(lines, taggedLine(levelTag, prefix+msgPart))
		}
		if offset+maxVisible < len(logLines) {
			remaining := len(logLines) - (offset + maxVisible)
			lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("  ... %d more lines", remaining)))
		}
	}

	tailLabel := ""
	if m.logsFollowTail {
		tailLabel = " " + dotFull + " TAIL"
	}

	summary := []string{
		taggedLine(dashboardTagSection, arrowR+" Log Source"),
		fmt.Sprintf("  source: %s", dashboardLogSource()),
		taggedLine(dashboardTagAccent, fmt.Sprintf("  total lines: %d", len(logLines))),
		fmt.Sprintf("  window: %d-%d", minInt(len(logLines), offset+1), minInt(len(logLines), offset+maxVisible)),
		taggedLine(dashboardTagGood, fmt.Sprintf("  follow:%s", tailLabel)),
		"",
		taggedLine(dashboardTagSection, arrowR+" Keys"),
		taggedLine(dashboardTagMuted, "  G  jump to end (follow)"),
		taggedLine(dashboardTagMuted, "  g  jump to start"),
		taggedLine(dashboardTagMuted, "  j/k  scroll line"),
		taggedLine(dashboardTagMuted, "  d/u  scroll page"),
		"",
		taggedLine(dashboardTagSection, arrowR+" Recent Message Types"),
	}
	summary = append(summary, recentMessageTypeLines(m.snapshot.Messages)...)

	if lo.narrow {
		summaryH := maxInt(8, height/3)
		logsH := maxInt(8, height-summaryH-1)
		left := renderPanel("Logs", leftWidth, logsH, lines)
		right := renderPanel("Runtime Summary", rightWidth, summaryH, summary)
		return strings.Join([]string{left, right}, "\n")
	}

	left := renderPanel("Logs", leftWidth, height, lines)
	right := renderPanel("Runtime Summary", rightWidth, height, summary)
	return joinHorizontal([]string{left, right}, 1)
}

// ---------------------------------------------------------------------------
// Footer
// ---------------------------------------------------------------------------

func (m dashboardModel) footerView(width int) string {
	updated := "never"
	if !m.lastUpdated.IsZero() {
		updated = m.lastUpdated.Format("15:04:05")
	}

	var left string
	if m.layout.narrow {
		left = fmt.Sprintf(" q quit %s tab %s j/k %s r refresh", boxV, boxV, boxV)
	} else {
		left = fmt.Sprintf(" Keys: q quit %s tab switch %s 1-4 select %s j/k scroll %s d/u page %s r refresh",
			boxV, boxV, boxV, boxV, boxV)
	}
	right := fmt.Sprintf("Updated %s ", updated)
	gap := maxInt(0, width-displayWidth(left)-displayWidth(right))
	line := left + strings.Repeat(" ", gap) + right
	return dashboardStyleFooter(truncateRight(line, width))
}

// ---------------------------------------------------------------------------
// Commands & selection
// ---------------------------------------------------------------------------

func (m dashboardModel) refreshCmd() tea.Cmd {
	client := m.client
	logs := m.logs
	timeout := m.timeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		snapshot, err := loadDashboardSnapshot(ctx, client, logs)
		return dashboardRefreshMsg{snapshot: snapshot, err: err}
	}
}

func (m *dashboardModel) moveSelection(delta int) {
	switch m.activeTab {
	case 1:
		m.cursors[1] = clampInt(m.cursors[1]+delta, 0, maxInt(0, len(m.snapshot.Peers)-1))
	case 2:
		m.cursors[2] = clampInt(m.cursors[2]+delta, 0, maxInt(0, len(m.snapshot.Messages)-1))
	case 3:
		lines := splitLogLines(m.snapshot.Logs)
		m.offsets[3] = clampInt(m.offsets[3]+delta, 0, maxInt(0, len(lines)-1))
		m.logsFollowTail = false
	}
}

func (m *dashboardModel) clampSelections() {
	m.cursors[1] = clampInt(m.cursors[1], 0, maxInt(0, len(m.snapshot.Peers)-1))
	m.cursors[2] = clampInt(m.cursors[2], 0, maxInt(0, len(m.snapshot.Messages)-1))
	logLines := splitLogLines(m.snapshot.Logs)
	if m.logsFollowTail {
		m.offsets[3] = maxInt(0, len(logLines)-1)
	} else {
		m.offsets[3] = clampInt(m.offsets[3], 0, maxInt(0, len(logLines)-1))
	}
}

func (m dashboardModel) natsStatusLabel() string {
	if m.snapshot.Health.NATS.Connected {
		return strings.ToUpper(fallbackString(m.snapshot.Health.NATS.Status, "connected"))
	}
	if strings.TrimSpace(m.snapshot.Health.NATS.Status) != "" {
		return strings.ToUpper(m.snapshot.Health.NATS.Status)
	}
	return "OFFLINE"
}

func dashboardTickCmd() tea.Cmd {
	return tea.Tick(dashboardRefreshInterval, func(t time.Time) tea.Msg {
		return t
	})
}

func spinnerTickCmd() tea.Cmd {
	return tea.Tick(dashboardSpinnerInterval, func(_ time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// ---------------------------------------------------------------------------
// Data loading
// ---------------------------------------------------------------------------

func loadDashboardSnapshot(ctx context.Context, client dashboardClient, logs logProvider) (dashboardSnapshot, error) {
	var snapshot dashboardSnapshot

	healthResult, err := client.Get(ctx, "/v1/health")
	if err != nil {
		return dashboardSnapshot{}, err
	}
	if err := decodeAPIData(healthResult.Data, &snapshot.Health); err != nil {
		return dashboardSnapshot{}, fmt.Errorf("decode health: %w", err)
	}

	peersResult, err := client.Get(ctx, "/v1/peers")
	if err != nil {
		return dashboardSnapshot{}, err
	}
	if err := decodeAPIItems(peersResult.Data, &snapshot.Peers); err != nil {
		return dashboardSnapshot{}, fmt.Errorf("decode peers: %w", err)
	}

	messagesResult, err := client.Get(ctx, "/v1/messages")
	if err != nil {
		return dashboardSnapshot{}, err
	}
	if err := decodeAPIItems(messagesResult.Data, &snapshot.Messages); err != nil {
		return dashboardSnapshot{}, fmt.Errorf("decode messages: %w", err)
	}
	if logs != nil {
		logText, err := logs.ReadLogs(ctx, defaultLogLines)
		if err != nil {
			snapshot.Logs = "log error: " + err.Error()
		} else {
			snapshot.Logs = logText
		}
	}

	snapshot.Updated = time.Now()
	return snapshot, nil
}

func decodeAPIItems(data map[string]any, dst any) error {
	rawItems, ok := data["items"]
	if !ok {
		return errors.New("missing items field")
	}
	return decodeInto(rawItems, dst)
}

func decodeAPIData(data map[string]any, dst any) error {
	return decodeInto(data, dst)
}

func decodeInto(src any, dst any) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// ---------------------------------------------------------------------------
// Log parsing
// ---------------------------------------------------------------------------

// logKnownFields lists the JSON keys that are extracted into dedicated
// parsedLogEntry fields and should NOT appear in Extra.
var logKnownFields = map[string]bool{
	"time": true, "level": true, "msg": true, "source": true,
	"nodeId": true, "service": true, "component": true,
	"event": true, "peer": true, "from": true, "to": true,
	"messageId": true, "requestId": true, "sessionKey": true,
	"error": true,
}

func parseLogEntry(line string) parsedLogEntry {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return parsedLogEntry{Raw: line}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return parsedLogEntry{Raw: line}
	}
	entry := parsedLogEntry{IsJSON: true}

	// time
	if t, ok := obj["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			entry.Time = parsed.Format("15:04:05")
		} else if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			entry.Time = parsed.Format("15:04:05")
		} else {
			entry.Time = t
		}
	}

	// Dedicated string fields.
	strField := func(key string) string {
		if v, ok := obj[key].(string); ok {
			return v
		}
		return ""
	}
	entry.Level = strField("level")
	entry.Msg = strField("msg")
	entry.NodeID = strField("nodeId")
	entry.Service = strField("service")
	entry.Comp = strField("component")
	entry.Event = strField("event")
	entry.Peer = strField("peer")
	entry.From = strField("from")
	entry.To = strField("to")
	entry.MsgID = strField("messageId")
	entry.ReqID = strField("requestId")
	entry.SessKey = strField("sessionKey")
	entry.Err = strField("error")

	// Collect remaining fields into Extra.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !logKnownFields[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry.Extra = append(entry.Extra, fmt.Sprintf("%s=%v", k, obj[k]))
	}
	return entry
}

func logLevelTag(level string) string {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "INFO":
		return dashboardTagLogInfo
	case "WARN", "WARNING":
		return dashboardTagLogWarn
	case "ERROR", "FATAL":
		return dashboardTagLogError
	case "DEBUG":
		return dashboardTagLogDebug
	default:
		return dashboardTagMuted
	}
}

// ---------------------------------------------------------------------------
// Panel rendering (Unicode box drawing)
// ---------------------------------------------------------------------------

func renderMetricPanel(title, value, detail string, width int) string {
	detailTag := dashboardTagMuted
	valueTag := dashboardTagAccent
	if title == "NATS" {
		if strings.EqualFold(detail, "connected") || strings.EqualFold(value, "connected") {
			valueTag = dashboardTagGood
		} else {
			valueTag = dashboardTagBad
		}
		detailTag = dashboardTagMuted
	}
	lines := []string{
		taggedLine(valueTag, "  "+value),
		taggedLine(detailTag, "  "+detail),
	}
	return renderPanelWithTheme(title, width, 5, lines, dashboardMetricTheme())
}

func renderPanel(title string, width, height int, lines []string) string {
	return renderPanelWithTheme(title, width, height, lines, dashboardDefaultTheme())
}

func panelHeightForLines(width int, lines []string) int {
	width = maxInt(width, 8)
	innerWidth := width - 2
	return maxInt(3, measureWrappedLineCount(innerWidth, lines)+2)
}

func renderPanelWithTheme(title string, width, height int, lines []string, theme dashboardPanelTheme) string {
	width = maxInt(width, 8)
	height = maxInt(height, 3)

	innerWidth := width - 2
	contentHeight := height - 2
	title = truncateRight(title, maxInt(1, innerWidth-4))

	// Top border: ╭─ Title ──────────╮
	titleStr := " " + title + " "
	titleVisLen := displayWidth(titleStr)
	remainH := maxInt(0, innerWidth-titleVisLen-1) // -1 for the dash before title
	top := theme.border(boxTL+boxH) + theme.title(titleStr) + theme.border(strings.Repeat(boxH, remainH)+boxTR)

	// Bottom border: ╰──────────────────╯
	bottom := theme.border(boxBL + strings.Repeat(boxH, innerWidth) + boxBR)

	expanded := make([]dashboardStyledLine, 0, len(lines))
	for _, rawLine := range lines {
		expanded = append(expanded, expandTaggedLine(rawLine, innerWidth)...)
	}

	if len(expanded) > contentHeight {
		expanded = expanded[:contentHeight]
		if contentHeight > 0 {
			expanded[contentHeight-1].text = truncateRight(expanded[contentHeight-1].text, maxInt(1, innerWidth-3)) + "..."
		}
	}

	body := make([]string, 0, height)
	body = append(body, top)
	for i := 0; i < contentHeight; i++ {
		line := dashboardStyledLine{}
		if i < len(expanded) {
			line = expanded[i]
		}
		content := padRightVisible(truncateRight(line.text, innerWidth), innerWidth)
		content = dashboardApplyLineStyle(line.tag, content)
		body = append(body, theme.border(boxV)+content+theme.border(boxV))
	}
	body = append(body, bottom)
	return strings.Join(body, "\n")
}

// ---------------------------------------------------------------------------
// Horizontal join & layout
// ---------------------------------------------------------------------------

func joinHorizontal(blocks []string, gap int) string {
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		return blocks[0]
	}

	parsed := make([][]string, len(blocks))
	maxLines := 0
	widths := make([]int, len(blocks))
	for i, block := range blocks {
		parsed[i] = strings.Split(block, "\n")
		if len(parsed[i]) > maxLines {
			maxLines = len(parsed[i])
		}
		for _, line := range parsed[i] {
			if visibleLen(line) > widths[i] {
				widths[i] = visibleLen(line)
			}
		}
	}

	var out []string
	sep := strings.Repeat(" ", gap)
	for row := 0; row < maxLines; row++ {
		parts := make([]string, 0, len(parsed))
		for col := range parsed {
			line := ""
			if row < len(parsed[col]) {
				line = parsed[col][row]
			}
			parts = append(parts, padRightVisible(line, widths[col]))
		}
		out = append(out, strings.Join(parts, sep))
	}
	return strings.Join(out, "\n")
}

func measureWrappedLineCount(width int, lines []string) int {
	width = maxInt(width, 1)
	total := 0
	for _, rawLine := range lines {
		total += len(expandTaggedLine(rawLine, width))
	}
	return total
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func ensureVisible(offset, cursor, total, visible int) int {
	if total <= 0 || visible <= 0 {
		return 0
	}
	maxOffset := maxInt(0, total-visible)
	offset = clampInt(offset, 0, maxOffset)
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+visible {
		offset = cursor - visible + 1
	}
	return clampInt(offset, 0, maxOffset)
}

// ---------------------------------------------------------------------------
// Peer formatting
// ---------------------------------------------------------------------------

func formatPeerListHeader() string {
	return " nodeId             auth           trust      adapter/version"
}

func formatPeerRow(peer types.Peer) string {
	authDot := peerAuthDot(peer.AuthStatus)
	trustDot := peerTrustDot(peer.TrustStatus)
	adapter := fallbackString(peer.AgentProduct, "-")
	if peer.Version != "" {
		adapter = adapter + " " + peer.Version
	}
	return fmt.Sprintf("%-18s %s %-12s %s %-8s %s",
		truncateRight(peer.NodeID, 18),
		authDot,
		truncateRight(fallbackString(peer.AuthStatus, "unknown"), 12),
		trustDot,
		truncateRight(fallbackString(peer.TrustStatus, "none"), 8),
		truncateRight(adapter, 22),
	)
}

func peerAuthDot(status string) string {
	switch status {
	case types.AuthAuthenticated:
		return dotFull
	case types.AuthPending:
		return dotHalf
	case types.AuthRejected, types.AuthExpired:
		return dotEmpty
	default:
		return dotEmpty
	}
}

func peerTrustDot(status string) string {
	switch status {
	case types.TrustTrusted:
		return dotFull
	case types.TrustPending:
		return dotHalf
	case types.TrustRejected, types.TrustRevoked:
		return dotEmpty
	default:
		return dotEmpty
	}
}

// ---------------------------------------------------------------------------
// Message formatting
// ---------------------------------------------------------------------------

type messageColumns struct {
	typeWidth    int
	fromWidth    int
	toWidth      int
	tsWidth      int
	previewWidth int
}

func messageColumnsForWidth(width int) messageColumns {
	width = maxInt(width, 32)
	gapCount := 5
	prefixWidth := 1
	tsWidth := 8
	usable := width - prefixWidth - gapCount - tsWidth
	if usable < 24 {
		usable = 24
	}

	typeWidth := clampInt(usable/4, 10, 18)
	fromWidth := clampInt(usable/6, 8, 14)
	toWidth := clampInt(usable/6, 8, 14)
	previewWidth := usable - typeWidth - fromWidth - toWidth

	if previewWidth < 12 {
		deficit := 12 - previewWidth
		typeWidth = maxInt(8, typeWidth-deficit)
		previewWidth = usable - typeWidth - fromWidth - toWidth
	}
	if previewWidth < 12 {
		deficit := 12 - previewWidth
		fromWidth = maxInt(6, fromWidth-deficit)
		previewWidth = usable - typeWidth - fromWidth - toWidth
	}
	if previewWidth < 12 {
		deficit := 12 - previewWidth
		toWidth = maxInt(6, toWidth-deficit)
		previewWidth = usable - typeWidth - fromWidth - toWidth
	}
	if previewWidth < 8 {
		previewWidth = 8
	}

	return messageColumns{
		typeWidth:    typeWidth,
		fromWidth:    fromWidth,
		toWidth:      toWidth,
		tsWidth:      tsWidth,
		previewWidth: previewWidth,
	}
}

func formatMessageListHeader(cols messageColumns) string {
	return formatMessageColumns(" ", "type", "from", "to", "ts", "preview", cols)
}

func formatMessageRow(prefix string, item protocol.MessageEnvelope, cols messageColumns) string {
	preview := normalizeSingleLine(item.Content)
	if preview == "" {
		preview = "-"
	}
	return formatMessageColumns(
		prefix,
		compactType(item.Type),
		fallbackString(item.From, "-"),
		fallbackString(item.To, "-"),
		formatShortTime(item.Ts),
		preview,
		cols,
	)
}

func formatMessageColumns(prefix, msgType, from, to, ts, preview string, cols messageColumns) string {
	return strings.Join([]string{
		padRight(truncateRight(prefix, 1), 1),
		padRight(truncateRight(msgType, cols.typeWidth), cols.typeWidth),
		padRight(truncateRight(from, cols.fromWidth), cols.fromWidth),
		padRight(truncateRight(to, cols.toWidth), cols.toWidth),
		padRight(truncateRight(ts, cols.tsWidth), cols.tsWidth),
		truncateRight(preview, cols.previewWidth),
	}, " ")
}

func compactType(v string) string {
	v = fallbackString(v, "unknown")
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[len(parts)-1]
	}
	return v
}

func formatShortTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.UnixMilli(ts).Format("15:04:05")
}

func formatUnixMilli(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.UnixMilli(ts).Format("2006-01-02 15:04:05")
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// ---------------------------------------------------------------------------
// Log & activity helpers
// ---------------------------------------------------------------------------

func splitLogLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func dashboardLogSource() string {
	if serviceGOOS == "darwin" {
		return "~/.clawsynapse/log/*.log"
	}
	return "journalctl -u clawsynapsed.service"
}

func recentMessageTypeLines(items []protocol.MessageEnvelope) []string {
	if len(items) == 0 {
		return []string{taggedLine(dashboardTagMuted, "  no recent messages")}
	}
	counts := map[string]int{}
	for _, item := range items {
		counts[compactType(item.Type)]++
	}
	type kv struct {
		key   string
		value int
	}
	pairs := make([]kv, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, kv{key: k, value: v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].value == pairs[j].value {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value > pairs[j].value
	})
	lines := make([]string, 0, minInt(5, len(pairs)))
	for i, item := range pairs {
		if i >= 5 {
			break
		}
		lines = append(lines, taggedLine(dashboardTagAccent, fmt.Sprintf("  %s: %d", item.key, item.value)))
	}
	return lines
}

func formatMetadata(meta map[string]any) []string {
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("  %s = %v", key, meta[key])))
	}
	return lines
}

func countPeersByAuth(peers []types.Peer, want string) int {
	count := 0
	for _, peer := range peers {
		if peer.AuthStatus == want {
			count++
		}
	}
	return count
}

func countPeersByTrust(peers []types.Peer, want string) int {
	count := 0
	for _, peer := range peers {
		if peer.TrustStatus == want {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// Text wrapping
// ---------------------------------------------------------------------------

func wrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if line == "" {
		return []string{""}
	}
	var lines []string
	for displayWidth(line) > width {
		part := runewidth.Truncate(line, width, "")
		if part == "" {
			break
		}
		lines = append(lines, part)
		line = strings.TrimPrefix(line, part)
	}
	lines = append(lines, line)
	return lines
}

func expandTaggedLine(rawLine string, width int) []dashboardStyledLine {
	tag, line := parseTaggedLine(rawLine)
	parts := splitPreservingEmptyLines(line)
	expanded := make([]dashboardStyledLine, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			expanded = append(expanded, dashboardStyledLine{text: "", tag: tag})
			continue
		}
		for _, wrapped := range wrapLine(part, width) {
			expanded = append(expanded, dashboardStyledLine{text: wrapped, tag: tag})
		}
	}
	return expanded
}

func splitPreservingEmptyLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(text, "\n")
}

func normalizeSingleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// ---------------------------------------------------------------------------
// Panel themes
// ---------------------------------------------------------------------------

func dashboardDefaultTheme() dashboardPanelTheme {
	return dashboardPanelTheme{
		border: dashboardStyleBorder,
		title:  dashboardStylePanelTitle,
	}
}

func dashboardMetricTheme() dashboardPanelTheme {
	return dashboardPanelTheme{
		border: dashboardStyleMetricBorder,
		title:  dashboardStyleMetricTitle,
	}
}

func dashboardHeaderTheme() dashboardPanelTheme {
	return dashboardPanelTheme{
		border: dashboardStyleHeaderBorder,
		title:  dashboardStyleHeaderTitle,
	}
}

// ---------------------------------------------------------------------------
// Tagged-line system
// ---------------------------------------------------------------------------

func taggedLine(tag, text string) string {
	if tag == "" {
		return text
	}
	return "[[" + tag + "]]" + text
}

func parseTaggedLine(line string) (string, string) {
	if !strings.HasPrefix(line, "[[") {
		return "", line
	}
	end := strings.Index(line, "]]")
	if end <= 2 {
		return "", line
	}
	return line[2:end], line[end+2:]
}

func dashboardApplyLineStyle(tag, text string) string {
	switch tag {
	case dashboardTagAccent:
		return dashboardStyleAccent(text)
	case dashboardTagMuted:
		return dashboardStyleMuted(text)
	case dashboardTagSection:
		return dashboardStyleSection(text)
	case dashboardTagGood:
		return dashboardStyleGood(text)
	case dashboardTagWarn:
		return dashboardStyleWarn(text)
	case dashboardTagBad:
		return dashboardStyleBad(text)
	case dashboardTagSelected:
		return dashboardStyleSelected(text)
	case dashboardTagDim:
		return dashboardStyleDim(text)
	case dashboardTagLogInfo:
		return dashboardStyleLogInfo(text)
	case dashboardTagLogWarn:
		return dashboardStyleLogWarn(text)
	case dashboardTagLogError:
		return dashboardStyleLogError(text)
	case dashboardTagLogDebug:
		return dashboardStyleLogDebug(text)
	default:
		return text
	}
}

// ---------------------------------------------------------------------------
// Status tag helpers
// ---------------------------------------------------------------------------

func dashboardPeerAuthTag(status string) string {
	switch status {
	case types.AuthAuthenticated:
		return dashboardTagGood
	case types.AuthPending:
		return dashboardTagWarn
	case types.AuthRejected, types.AuthExpired:
		return dashboardTagBad
	default:
		return dashboardTagMuted
	}
}

func dashboardPeerTrustTag(status string) string {
	switch status {
	case types.TrustTrusted:
		return dashboardTagGood
	case types.TrustPending:
		return dashboardTagWarn
	case types.TrustRejected, types.TrustRevoked:
		return dashboardTagBad
	default:
		return dashboardTagMuted
	}
}

// ---------------------------------------------------------------------------
// ANSI style functions — Cyberpunk / Neon palette
// ---------------------------------------------------------------------------

func dashboardStyle(code, text string) string {
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Panel borders — dim cyan.
func dashboardStyleBorder(text string) string { return dashboardStyle("38;5;37", text) }

// Metric panel borders — bright cyan.
func dashboardStyleMetricBorder(text string) string { return dashboardStyle("38;5;45", text) }

// Header borders — neon magenta.
func dashboardStyleHeaderBorder(text string) string { return dashboardStyle("38;5;135", text) }

// Panel titles — black on cyan.
func dashboardStylePanelTitle(text string) string {
	return dashboardStyle("1;38;5;16;48;5;37", text)
}

// Metric titles — black on neon green.
func dashboardStyleMetricTitle(text string) string {
	return dashboardStyle("1;38;5;16;48;5;48", text)
}

// Header title — bold white on magenta.
func dashboardStyleHeaderTitle(text string) string {
	return dashboardStyle("1;38;5;255;48;5;55", text)
}

// Accent — bright cyan (for key values, highlights).
func dashboardStyleAccent(text string) string { return dashboardStyle("1;38;5;45", text) }

// Muted — gray (for secondary info).
func dashboardStyleMuted(text string) string { return dashboardStyle("38;5;243", text) }

// Dim — darker gray.
func dashboardStyleDim(text string) string { return dashboardStyle("38;5;239", text) }

// Section headers — bold magenta.
func dashboardStyleSection(text string) string { return dashboardStyle("1;38;5;177", text) }

// Good status — neon green text.
func dashboardStyleGood(text string) string { return dashboardStyle("38;5;48", text) }

// Warning — amber text.
func dashboardStyleWarn(text string) string { return dashboardStyle("38;5;214", text) }

// Bad/error — neon red-pink text.
func dashboardStyleBad(text string) string { return dashboardStyle("1;38;5;197", text) }

// Selected row — white on dark blue.
func dashboardStyleSelected(text string) string {
	return dashboardStyle("1;38;5;255;48;5;24", text)
}

// Footer — muted gray.
func dashboardStyleFooter(text string) string { return dashboardStyle("38;5;243", text) }

// Active tab — bold white on dark teal.
func dashboardStyleTabActive(text string) string {
	return dashboardStyle("1;38;5;255;48;5;30", text)
}

// Inactive tab — dim gray.
func dashboardStyleTabInactive(text string) string { return dashboardStyle("38;5;243", text) }

// Log level styles.
func dashboardStyleLogInfo(text string) string  { return dashboardStyle("38;5;45", text) }
func dashboardStyleLogWarn(text string) string  { return dashboardStyle("38;5;214", text) }
func dashboardStyleLogError(text string) string { return dashboardStyle("1;38;5;197", text) }
func dashboardStyleLogDebug(text string) string { return dashboardStyle("38;5;243", text) }

// ---------------------------------------------------------------------------
// String utilities
// ---------------------------------------------------------------------------

func truncateRight(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return runewidth.Truncate(s, width, "")
	}
	return runewidth.Truncate(s, width, "...")
}

func truncateRightVisible(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if visibleLen(s) <= width {
		return s
	}
	return runewidth.Truncate(stripANSI(s), width, "...")
}

func padRight(s string, width int) string {
	if displayWidth(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-displayWidth(s))
}

func padRightVisible(s string, width int) string {
	if visibleLen(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visibleLen(s))
}

func stripANSI(s string) string {
	return dashboardANSIRegexp.ReplaceAllString(s, "")
}

func visibleLen(s string) int {
	return displayWidth(stripANSI(s))
}

func displayWidth(s string) int {
	return runewidth.StringWidth(s)
}

func clampInt(v, low, high int) int {
	if high < low {
		return low
	}
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func boolWord(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func fallbackString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
