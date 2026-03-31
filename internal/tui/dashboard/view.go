package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"clawsynapse/pkg/types"
)

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), dashboardTickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case refreshMsg:
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

func (m model) View() tea.View {
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

func (m model) headerView(width int) string {
	statusIcon := dotFull
	statusLabel := "READY"
	if m.errText != "" {
		statusIcon = dotEmpty
		statusLabel = "ERROR"
	}

	updated := "never"
	if !m.lastUpdated.IsZero() {
		updated = m.lastUpdated.Format("15:04:05")
	}

	natsIcon := dotFull
	natsTag := dashboardTagGood
	if !m.snapshot.Health.NATS.Connected {
		natsIcon = dotEmpty
		natsTag = dashboardTagBad
	}

	versionText := fallbackString(m.version, "dev")

	var lines []string
	if m.layout.narrow {
		lines = []string{
			taggedLine(dashboardTagAccent, fmt.Sprintf("%s %s %s %s %s",
				diamond, versionText, boxV, statusIcon, statusLabel)),
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
				diamond, versionText, boxV, m.apiAddr, boxV, statusIcon, statusLabel, boxV, updated,
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

func (m model) tabsView(width int) string {
	fullLabels := []string{"Overview", "Peers", "Messages", "Logs"}
	shortLabels := []string{"Ovw", "Prs", "Msg", "Log"}
	nums := []string{"1", "2", "3", "4"}

	labels := fullLabels
	sep := "  "
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

func (m model) bodyView(width, height int) string {
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

func (m model) overviewView(width, height int) string {
	lo := m.layout
	narrow := lo.narrow

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

func (m model) overviewNATSLines() []string {
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

func (m model) overviewActivityLines() []string {
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

func (m model) peersView(width, height int) string {
	lo := m.layout

	if lo.narrow {
		listH := maxInt(8, height*3/5)
		detailH := maxInt(8, height-listH-1)
		list := renderPanel("Peers", width, listH, m.peerListLines(maxInt(1, listH-3), maxInt(1, width-2)))
		detail := renderPanel("Peer Detail", width, detailH, m.selectedPeerLines())
		return strings.Join([]string{list, detail}, "\n")
	}

	listWidth := width * 2 / 3
	detailWidth := width - listWidth - 1
	list := renderPanel("Peers", listWidth, height, m.peerListLines(maxInt(1, height-3), maxInt(1, listWidth-2)))
	detail := renderPanel("Peer Detail", detailWidth, height, m.selectedPeerLines())
	return joinHorizontal([]string{list, detail}, 1)
}

func (m model) peerListLines(contentHeight, contentWidth int) []string {
	cols := peerColumnsForWidth(contentWidth, m.snapshot.Peers)
	lines := []string{
		taggedLine(dashboardTagDim, formatPeerListHeader(cols)),
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
		lines = append(lines, taggedLine(tag, formatPeerRow(prefix, peer, cols)))
	}
	if offset+visibleRows < len(m.snapshot.Peers) {
		lines = append(lines, taggedLine(dashboardTagMuted, fmt.Sprintf("  ... %d more peers", len(m.snapshot.Peers)-(offset+visibleRows))))
	}
	return lines
}

func (m model) selectedPeerLines() []string {
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

func (m model) messagesView(width, height int) string {
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

func (m model) messageListLines(contentHeight, contentWidth int) []string {
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

func (m model) selectedMessageLines() []string {
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

func (m model) logsView(width, height int) string {
	lo := m.layout
	leftWidth := lo.logsLeftW
	rightWidth := lo.logsRightW

	logLines := splitLogLines(m.snapshot.Logs)
	listContentHeight := maxInt(1, height-3)

	offset := m.offsets[3]
	if m.logsFollowTail && len(logLines) > listContentHeight {
		offset = len(logLines) - listContentHeight
	}
	offset = clampInt(offset, 0, maxInt(0, len(logLines)-1))
	maxVisible := maxInt(1, listContentHeight)
	if offset > maxInt(0, len(logLines)-maxVisible) {
		offset = maxInt(0, len(logLines)-maxVisible)
	}

	timeW := 8
	levelW := 5
	compW := 10
	innerW := maxInt(20, leftWidth-2)

	lines := make([]string, 0, listContentHeight)
	if len(logLines) == 0 {
		lines = append(lines, taggedLine(dashboardTagMuted, "  no logs available"))
	} else {
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
			timeStr := padRight(truncateRight(entry.Time, timeW), timeW)
			levelStr := padRight(strings.ToUpper(truncateRight(entry.Level, levelW)), levelW)
			compStr := padRight(truncateRight(entry.Comp, compW), compW)
			prefix := fmt.Sprintf(" %s %s %s %s %s %s ",
				timeStr, boxV, levelStr, boxV, compStr, boxV)
			prefixW := displayWidth(prefix)
			msgAreaW := maxInt(10, innerW-prefixW)

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
			ctx = append(ctx, entry.Extra...)

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
		fmt.Sprintf("  source: %s", fallbackString(m.logSource, "-")),
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

func (m model) footerView(width int) string {
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

func (m model) refreshCmd() tea.Cmd {
	client := m.client
	logs := m.logs
	timeout := m.timeout
	logLines := m.logLines
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		snapshot, err := loadSnapshot(ctx, client, logs, logLines)
		return refreshMsg{snapshot: snapshot, err: err}
	}
}

func (m *model) moveSelection(delta int) {
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

func (m *model) clampSelections() {
	m.cursors[1] = clampInt(m.cursors[1], 0, maxInt(0, len(m.snapshot.Peers)-1))
	m.cursors[2] = clampInt(m.cursors[2], 0, maxInt(0, len(m.snapshot.Messages)-1))
	logLines := splitLogLines(m.snapshot.Logs)
	if m.logsFollowTail {
		m.offsets[3] = maxInt(0, len(logLines)-1)
	} else {
		m.offsets[3] = clampInt(m.offsets[3], 0, maxInt(0, len(logLines)-1))
	}
}

func (m model) natsStatusLabel() string {
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
