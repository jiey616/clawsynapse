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
	dashboardRefreshInterval = 3 * time.Second
	dashboardMessagesLimit   = 12
	dashboardTabCount        = 4
	dashboardMinWidth        = 84
	dashboardMinHeight       = 26
	dashboardPageStep        = 6
)

const (
	dashboardTagAccent   = "accent"
	dashboardTagMuted    = "muted"
	dashboardTagSection  = "section"
	dashboardTagGood     = "good"
	dashboardTagWarn     = "warn"
	dashboardTagBad      = "bad"
	dashboardTagSelected = "selected"
)

var dashboardANSIRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

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

type dashboardPanelTheme struct {
	border func(string) string
	title  func(string) string
}

type dashboardStyledLine struct {
	text string
	tag  string
}

type dashboardModel struct {
	client      dashboardClient
	logs        logProvider
	apiAddr     string
	timeout     time.Duration
	width       int
	height      int
	activeTab   int
	lastUpdated time.Time
	snapshot    dashboardSnapshot
	loading     bool
	errText     string
	cursors     [dashboardTabCount]int
	offsets     [dashboardTabCount]int
}

func runDashboard(args []string, stdout, stderr *os.File) error {
	cfg, err := parseDashboardArgs(args, stderr)
	if err != nil {
		return err
	}

	model := dashboardModel{
		client:  api.NewClient(cfg.APIAddr, cfg.Timeout),
		logs:    defaultLogProvider{runner: execServiceRunner{}},
		apiAddr: cfg.APIAddr,
		timeout: cfg.Timeout,
		loading: true,
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

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), dashboardTickCmd())
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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
	width := maxInt(m.width, dashboardMinWidth)
	height := maxInt(m.height, dashboardMinHeight)

	header := m.headerView(width)
	tabs := m.tabsView(width)
	footer := m.footerView(width)
	bodyHeight := maxInt(12, height-countLines(header)-countLines(tabs)-countLines(footer))

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

func (m dashboardModel) headerView(width int) string {
	status := "READY"
	statusTag := dashboardTagGood
	if m.loading {
		status = "REFRESHING"
		statusTag = dashboardTagWarn
	}
	if m.errText != "" {
		status = "ERROR"
		statusTag = dashboardTagBad
	}
	updated := "never"
	if !m.lastUpdated.IsZero() {
		updated = m.lastUpdated.Format("15:04:05")
	}

	lines := []string{
		taggedLine(dashboardTagAccent, fmt.Sprintf("CLI v%s | API %s | Last update %s | Status %s", version, m.apiAddr, updated, status)),
		taggedLine(statusTag, fmt.Sprintf("NATS %s | Peers %d | Traffic in/out %d/%d msgs | Press r to refresh immediately", m.natsStatusLabel(), len(m.snapshot.Peers), m.snapshot.Health.NATS.InMsgs, m.snapshot.Health.NATS.OutMsgs)),
	}
	if m.errText != "" {
		lines = append(lines, taggedLine(dashboardTagBad, "Last refresh failed: "+truncateRight(m.errText, maxInt(12, width-18))))
	}
	return renderPanelWithTheme("ClawSynapse Dashboard", width, 5, lines, dashboardHeaderTheme())
}

func (m dashboardModel) tabsView(width int) string {
	tabs := []string{"1 Overview", "2 Peers", "3 Messages", "4 Logs"}
	out := make([]string, 0, len(tabs))
	for i, label := range tabs {
		if i == m.activeTab {
			out = append(out, dashboardStyleTabActive(" "+label+" "))
		} else {
			out = append(out, dashboardStyleTabInactive(" "+label+" "))
		}
	}
	line := strings.Join(out, "  ")
	return truncateRightVisible(line, width)
}

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

func (m dashboardModel) overviewView(width, height int) string {
	cardWidth := maxInt(18, (width-3)/4)
	cards := []string{
		renderMetricPanel("Peers", fmt.Sprintf("%d", len(m.snapshot.Peers)), "discovered nodes", cardWidth),
		renderMetricPanel("NATS", boolWord(m.snapshot.Health.NATS.Connected, "connected", "offline"), fallbackString(m.snapshot.Health.NATS.Status, "unknown"), cardWidth),
		renderMetricPanel("Inbound", fmt.Sprintf("%d msgs", m.snapshot.Health.NATS.InMsgs), fmt.Sprintf("%d bytes", m.snapshot.Health.NATS.InBytes), cardWidth),
		renderMetricPanel("Outbound", fmt.Sprintf("%d msgs", m.snapshot.Health.NATS.OutMsgs), fmt.Sprintf("%d bytes", m.snapshot.Health.NATS.OutBytes), cardWidth),
	}
	top := joinHorizontal(cards, 1)

	remaining := maxInt(8, height-countLines(top)-1)
	if width < 110 {
		left := renderPanel("NATS", width, remaining/2, m.overviewNATSLines())
		right := renderPanel("Activity", width, height-countLines(top)-countLines(left)-1, m.overviewActivityLines())
		return strings.Join([]string{top, left, right}, "\n")
	}

	leftWidth := width * 3 / 5
	rightWidth := width - leftWidth - 1
	left := renderPanel("NATS", leftWidth, remaining, m.overviewNATSLines())
	right := renderPanel("Activity", rightWidth, remaining, m.overviewActivityLines())
	return strings.Join([]string{top, joinHorizontal([]string{left, right}, 1)}, "\n")
}

func (m dashboardModel) overviewNATSLines() []string {
	h := m.snapshot.Health.NATS
	lines := []string{
		taggedLine(dashboardTagSection, "Connection details"),
		fmt.Sprintf("server: %s", fallbackString(h.ServerURL, "-")),
		fmt.Sprintf("name: %s", fallbackString(h.Name, "-")),
		taggedLine(dashboardHealthTag(h.Connected), fmt.Sprintf("status: %s", fallbackString(h.Status, "unknown"))),
		taggedLine(dashboardHealthTag(h.Connected), fmt.Sprintf("connected: %s", boolWord(h.Connected, "yes", "no"))),
		fmt.Sprintf("reconnects: %d", h.Reconnects),
		fmt.Sprintf("disconnects: %d", h.Disconnects),
		fmt.Sprintf("connectedAt: %s", formatUnixMilli(h.ConnectedAt)),
		fmt.Sprintf("lastReconnectAt: %s", formatUnixMilli(h.LastReconnectAt)),
		fmt.Sprintf("lastDisconnectAt: %s", formatUnixMilli(h.LastDisconnectAt)),
	}
	if strings.TrimSpace(h.LastError) != "" {
		lines = append(lines, "", taggedLine(dashboardTagBad, "lastError:"), taggedLine(dashboardTagBad, h.LastError))
	}
	return lines
}

func (m dashboardModel) overviewActivityLines() []string {
	lines := []string{
		taggedLine(dashboardTagSection, "Peer state"),
		taggedLine(dashboardTagGood, fmt.Sprintf("authenticated: %d", countPeersByAuth(m.snapshot.Peers, types.AuthAuthenticated))),
		taggedLine(dashboardTagWarn, fmt.Sprintf("pending auth: %d", countPeersByAuth(m.snapshot.Peers, types.AuthPending))),
		taggedLine(dashboardTagGood, fmt.Sprintf("trusted: %d", countPeersByTrust(m.snapshot.Peers, types.TrustTrusted))),
		taggedLine(dashboardTagWarn, fmt.Sprintf("pending trust: %d", countPeersByTrust(m.snapshot.Peers, types.TrustPending))),
		"",
		taggedLine(dashboardTagSection, "Recent messages"),
	}

	if len(m.snapshot.Messages) == 0 {
		lines = append(lines, taggedLine(dashboardTagMuted, "no recent messages"))
		return lines
	}

	for i, item := range m.snapshot.Messages {
		if i >= 6 {
			lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("... %d more", len(m.snapshot.Messages)-i)))
			break
		}
		lines = append(lines, taggedLine(dashboardTagAccent, fmt.Sprintf("%s %s -> %s", compactType(item.Type), fallbackString(item.From, "-"), fallbackString(item.To, "-"))))
		lines = append(lines, taggedLine(dashboardTagMuted, "  "+truncateRight(strings.TrimSpace(item.Content), 48)))
	}
	return lines
}

func (m dashboardModel) peersView(width, height int) string {
	leftWidth := width * 3 / 5
	rightWidth := width - leftWidth - 1
	if width < 100 {
		leftWidth = width
		rightWidth = width
	}

	list := m.peerListPanel(leftWidth, height)
	detail := m.peerDetailPanel(rightWidth, height)
	if width < 100 {
		detailHeight := maxInt(8, height-countLines(list)-1)
		return strings.Join([]string{
			renderPanel("Peers", leftWidth, maxInt(8, height-detailHeight-1), m.peerListLines(maxInt(1, height-detailHeight-3))),
			renderPanel("Peer Detail", rightWidth, detailHeight, m.selectedPeerLines()),
		}, "\n")
	}
	return joinHorizontal([]string{list, detail}, 1)
}

func (m dashboardModel) peerListPanel(width, height int) string {
	return renderPanel("Peers", width, height, m.peerListLines(maxInt(1, height-3)))
}

func (m dashboardModel) peerDetailPanel(width, height int) string {
	return renderPanel("Peer Detail", width, height, m.selectedPeerLines())
}

func (m dashboardModel) peerListLines(contentHeight int) []string {
	lines := []string{
		taggedLine(dashboardTagMuted, formatPeerListHeader()),
	}
	if len(m.snapshot.Peers) == 0 {
		return append(lines, taggedLine(dashboardTagMuted, "no peers discovered"))
	}

	visibleRows := maxInt(1, contentHeight-1)
	offset := ensureVisible(m.offsets[1], m.cursors[1], len(m.snapshot.Peers), visibleRows)
	for row := offset; row < len(m.snapshot.Peers) && len(lines) < contentHeight; row++ {
		peer := m.snapshot.Peers[row]
		prefix := " "
		tag := ""
		if row == m.cursors[1] {
			prefix = ">"
			tag = dashboardTagSelected
		}
		lines = append(lines, taggedLine(tag, prefix+formatPeerRow(peer)))
	}
	if offset+visibleRows < len(m.snapshot.Peers) {
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("... %d more peers", len(m.snapshot.Peers)-(offset+visibleRows))))
	}
	return lines
}

func (m dashboardModel) selectedPeerLines() []string {
	if len(m.snapshot.Peers) == 0 {
		return []string{"No peer selected.", "", "Use tab to move between views."}
	}
	peer := m.snapshot.Peers[m.cursors[1]]
	return []string{
		taggedLine(dashboardTagAccent, fmt.Sprintf("nodeId: %s", peer.NodeID)),
		taggedLine(dashboardPeerAuthTag(peer.AuthStatus), fmt.Sprintf("auth: %s", fallbackString(peer.AuthStatus, "unknown"))),
		taggedLine(dashboardPeerTrustTag(peer.TrustStatus), fmt.Sprintf("trust: %s", fallbackString(peer.TrustStatus, "none"))),
		fmt.Sprintf("agentProduct: %s", fallbackString(peer.AgentProduct, "-")),
		fmt.Sprintf("version: %s", fallbackString(peer.Version, "-")),
		fmt.Sprintf("inbox: %s", fallbackString(peer.Inbox, "-")),
		"",
		taggedLine(dashboardTagSection, "Fleet summary"),
		taggedLine(dashboardTagGood, fmt.Sprintf("trusted peers: %d", countPeersByTrust(m.snapshot.Peers, types.TrustTrusted))),
		taggedLine(dashboardTagWarn, fmt.Sprintf("auth pending: %d", countPeersByAuth(m.snapshot.Peers, types.AuthPending))),
		taggedLine(dashboardTagMuted, fmt.Sprintf("seen only: %d", countPeersByAuth(m.snapshot.Peers, types.AuthSeen))),
	}
}

func (m dashboardModel) messagesView(width, height int) string {
	leftWidth := width * 3 / 5
	rightWidth := width - leftWidth - 1
	if width < 100 {
		leftWidth = width
		rightWidth = width
	}

	list := renderPanel("Messages", leftWidth, height, m.messageListLines(maxInt(1, height-3), maxInt(24, leftWidth-2)))
	detail := renderPanel("Message Detail", rightWidth, height, m.selectedMessageLines())
	if width < 100 {
		detailHeight := maxInt(8, height/2)
		return strings.Join([]string{
			renderPanel("Messages", leftWidth, maxInt(8, height-detailHeight-1), m.messageListLines(maxInt(1, height-detailHeight-3), maxInt(24, leftWidth-2))),
			renderPanel("Message Detail", rightWidth, detailHeight, m.selectedMessageLines()),
		}, "\n")
	}
	return joinHorizontal([]string{list, detail}, 1)
}

func (m dashboardModel) messageListLines(contentHeight, contentWidth int) []string {
	cols := messageColumnsForWidth(contentWidth)
	lines := []string{
		taggedLine(dashboardTagMuted, formatMessageListHeader(cols)),
	}
	if len(m.snapshot.Messages) == 0 {
		return append(lines, taggedLine(dashboardTagMuted, "no recent messages"))
	}

	visibleRows := maxInt(1, contentHeight-1)
	offset := ensureVisible(m.offsets[2], m.cursors[2], len(m.snapshot.Messages), visibleRows)
	for row := offset; row < len(m.snapshot.Messages) && len(lines) < contentHeight; row++ {
		item := m.snapshot.Messages[row]
		prefix := " "
		tag := ""
		if row == m.cursors[2] {
			prefix = ">"
			tag = dashboardTagSelected
		}
		lines = append(lines, taggedLine(tag, formatMessageRow(prefix, item, cols)))
	}
	if offset+visibleRows < len(m.snapshot.Messages) {
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("... %d more messages", len(m.snapshot.Messages)-(offset+visibleRows))))
	}
	return lines
}

func (m dashboardModel) selectedMessageLines() []string {
	if len(m.snapshot.Messages) == 0 {
		return []string{"No message selected.", "", "Use j/k or arrow keys to inspect entries."}
	}
	item := m.snapshot.Messages[m.cursors[2]]
	lines := []string{
		taggedLine(dashboardTagMuted, fmt.Sprintf("id: %s", fallbackString(item.ID, "-"))),
		taggedLine(dashboardTagAccent, fmt.Sprintf("type: %s", fallbackString(item.Type, "unknown"))),
		fmt.Sprintf("from: %s", fallbackString(item.From, "-")),
		fmt.Sprintf("to: %s", fallbackString(item.To, "-")),
		fmt.Sprintf("sessionKey: %s", fallbackString(item.SessionKey, "-")),
		fmt.Sprintf("ts: %s", formatUnixMilli(item.Ts)),
		"",
		taggedLine(dashboardTagSection, "content:"),
	}
	if strings.TrimSpace(item.Content) == "" {
		lines = append(lines, taggedLine(dashboardTagMuted, "-"))
	} else {
		lines = append(lines, item.Content)
	}
	if len(item.Metadata) > 0 {
		lines = append(lines, "", taggedLine(dashboardTagSection, "metadata:"))
		lines = append(lines, formatMetadata(item.Metadata)...)
	}
	return lines
}

func (m dashboardModel) logsView(width, height int) string {
	leftWidth := width * 7 / 10
	rightWidth := width - leftWidth - 1
	if width < 100 {
		leftWidth = width
		rightWidth = width
	}

	logLines := splitLogLines(m.snapshot.Logs)
	listContentHeight := maxInt(1, height-3)
	offset := clampInt(m.offsets[3], 0, maxInt(0, len(logLines)-1))
	maxVisible := maxInt(1, listContentHeight)
	if offset > maxInt(0, len(logLines)-maxVisible) {
		offset = maxInt(0, len(logLines)-maxVisible)
	}

	lines := make([]string, 0, listContentHeight)
	if len(logLines) == 0 {
		lines = append(lines, taggedLine(dashboardTagMuted, "no logs available"))
	} else {
		for i := offset; i < len(logLines) && len(lines) < listContentHeight; i++ {
			lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("%4d  %s", i+1, logLines[i])))
		}
		if offset+maxVisible < len(logLines) {
			lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("... %d more lines", len(logLines)-(offset+maxVisible))))
		}
	}

	summary := []string{
		taggedLine(dashboardTagSection, "Log source"),
		fmt.Sprintf("source: %s", dashboardLogSource()),
		taggedLine(dashboardTagAccent, fmt.Sprintf("total lines: %d", len(logLines))),
		fmt.Sprintf("window start: %d", minInt(len(logLines), offset+1)),
		fmt.Sprintf("window end: %d", minInt(len(logLines), offset+maxVisible)),
		"",
		taggedLine(dashboardTagSection, "Recent message types"),
	}
	summary = append(summary, recentMessageTypeLines(m.snapshot.Messages)...)

	left := renderPanel("Logs", leftWidth, height, lines)
	right := renderPanel("Runtime Summary", rightWidth, height, summary)
	if width < 100 {
		summaryHeight := maxInt(8, height/3)
		return strings.Join([]string{
			renderPanel("Logs", leftWidth, maxInt(8, height-summaryHeight-1), lines),
			renderPanel("Runtime Summary", rightWidth, summaryHeight, summary),
		}, "\n")
	}
	return joinHorizontal([]string{left, right}, 1)
}

func (m dashboardModel) footerView(width int) string {
	updated := "never"
	if !m.lastUpdated.IsZero() {
		updated = m.lastUpdated.Format("15:04:05")
	}
	line := fmt.Sprintf("Keys: q quit | tab switch | 1-4 select | j/k scroll | d/u page | r refresh | Last updated %s", updated)
	return dashboardStyleFooter(truncateRight(line, width))
}

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
	}
}

func (m *dashboardModel) clampSelections() {
	m.cursors[1] = clampInt(m.cursors[1], 0, maxInt(0, len(m.snapshot.Peers)-1))
	m.cursors[2] = clampInt(m.cursors[2], 0, maxInt(0, len(m.snapshot.Messages)-1))
	m.offsets[3] = clampInt(m.offsets[3], 0, maxInt(0, len(splitLogLines(m.snapshot.Logs))-1))
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
		taggedLine(valueTag, value),
		taggedLine(detailTag, detail),
	}
	return renderPanelWithTheme(title, width, 5, lines, dashboardMetricTheme())
}

func renderPanel(title string, width, height int, lines []string) string {
	return renderPanelWithTheme(title, width, height, lines, dashboardDefaultTheme())
}

func renderPanelWithTheme(title string, width, height int, lines []string, theme dashboardPanelTheme) string {
	width = maxInt(width, 8)
	height = maxInt(height, 3)

	innerWidth := width - 2
	contentHeight := height - 2
	title = truncateRight(title, maxInt(1, innerWidth-2))
	top := theme.border("+") + theme.title(" "+padRightVisible(title, maxInt(1, innerWidth-1))) + theme.border("+")
	bottom := theme.border("+" + strings.Repeat("-", innerWidth) + "+")

	expanded := make([]dashboardStyledLine, 0, len(lines))
	for _, rawLine := range lines {
		tag, line := parseTaggedLine(rawLine)
		if strings.TrimSpace(line) == "" {
			expanded = append(expanded, dashboardStyledLine{text: "", tag: tag})
			continue
		}
		for _, part := range wrapLine(line, innerWidth) {
			expanded = append(expanded, dashboardStyledLine{text: part, tag: tag})
		}
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
		body = append(body, theme.border("|")+content+theme.border("|"))
	}
	body = append(body, bottom)
	return strings.Join(body, "\n")
}

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

func formatPeerListHeader() string {
	return " nodeId             auth           trust      adapter/version"
}

func formatPeerRow(peer types.Peer) string {
	adapter := fallbackString(peer.AgentProduct, "-")
	if peer.Version != "" {
		adapter = adapter + " " + peer.Version
	}
	return fmt.Sprintf("%-18s %-14s %-10s %s",
		truncateRight(peer.NodeID, 18),
		truncateRight(fallbackString(peer.AuthStatus, "unknown"), 14),
		truncateRight(fallbackString(peer.TrustStatus, "none"), 10),
		truncateRight(adapter, 24),
	)
}

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
	preview := strings.TrimSpace(item.Content)
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
		return []string{taggedLine(dashboardTagMuted, "no recent messages")}
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
		lines = append(lines, taggedLine(dashboardTagAccent, fmt.Sprintf("%s: %d", item.key, item.value)))
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
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("%s=%v", key, meta[key])))
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
	default:
		return text
	}
}

func dashboardHealthTag(connected bool) string {
	if connected {
		return dashboardTagGood
	}
	return dashboardTagBad
}

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

func dashboardStyle(code, text string) string {
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func dashboardStyleBorder(text string) string       { return dashboardStyle("38;5;67", text) }
func dashboardStyleMetricBorder(text string) string { return dashboardStyle("38;5;44", text) }
func dashboardStyleHeaderBorder(text string) string { return dashboardStyle("38;5;81", text) }
func dashboardStylePanelTitle(text string) string   { return dashboardStyle("1;38;5;255;48;5;24", text) }
func dashboardStyleMetricTitle(text string) string  { return dashboardStyle("1;38;5;232;48;5;79", text) }
func dashboardStyleHeaderTitle(text string) string {
	return dashboardStyle("1;38;5;232;48;5;117", text)
}
func dashboardStyleAccent(text string) string      { return dashboardStyle("1;38;5;117", text) }
func dashboardStyleMuted(text string) string       { return dashboardStyle("38;5;246", text) }
func dashboardStyleSection(text string) string     { return dashboardStyle("1;38;5;81", text) }
func dashboardStyleGood(text string) string        { return dashboardStyle("1;38;5;255;48;5;29", text) }
func dashboardStyleWarn(text string) string        { return dashboardStyle("1;38;5;16;48;5;214", text) }
func dashboardStyleBad(text string) string         { return dashboardStyle("1;38;5;255;48;5;160", text) }
func dashboardStyleSelected(text string) string    { return dashboardStyle("1;38;5;255;48;5;60", text) }
func dashboardStyleFooter(text string) string      { return dashboardStyle("38;5;246", text) }
func dashboardStyleTabActive(text string) string   { return dashboardStyle("1;38;5;255;48;5;24", text) }
func dashboardStyleTabInactive(text string) string { return dashboardStyle("38;5;110", text) }

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
