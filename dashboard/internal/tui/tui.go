// Package tui renders core state; deployment discovery and execution live in adapters.
package tui

import (
	"context"
	"fmt"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"maat/dashboard/internal/core"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

type UI struct {
	core                             *core.App
	app                              *tview.Application
	table                            *tview.Table
	details, header, footer, message *tview.TextView
	pages                            *tview.Pages
	mode, label, sessionError        string
	ctx                              context.Context
	request                          chan struct{}
	suspended                        atomic.Bool
}

func New(a *core.App, label string, screen tcell.Screen) *UI {
	u := &UI{core: a, app: tview.NewApplication(), table: tview.NewTable().SetSelectable(true, false).SetFixed(1, 0), details: tview.NewTextView().SetDynamicColors(true), header: tview.NewTextView().SetDynamicColors(true), footer: tview.NewTextView().SetDynamicColors(true), message: tview.NewTextView().SetDynamicColors(true), pages: tview.NewPages(), mode: "nodes", label: label, ctx: context.Background(), request: make(chan struct{}, 1)}
	u.table.SetBorder(true).SetTitle(" Nodes ")
	u.details.SetBorder(true)
	u.pages.AddPage("nodes", u.table, true, true).AddPage("details", u.details, true, false)
	root := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.header, 4, 0, false).AddItem(u.pages, 0, 1, true).AddItem(u.footer, 1, 0, false).AddItem(u.message, 2, 0, false)
	u.app.SetRoot(root, true).SetFocus(u.table).SetInputCapture(u.key)
	if screen != nil {
		u.app.SetScreen(screen)
	}
	u.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		w, h := screen.Size()
		if w < 80 || h < 24 {
			screen.Clear()
			tview.Print(screen, "Resize terminal to at least 80 x 24. Press q to exit.", 0, 0, w, tview.AlignLeft, tcell.ColorYellow)
			return true
		}
		u.render(w, h)
		return false
	})
	return u
}
func (u *UI) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u.ctx = ctx
	var wg sync.WaitGroup
	wg.Go(func() { u.core.Poll(ctx, u.request) })
	wg.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				u.app.QueueEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, 0))
				return
			case <-ticker.C:
				if !u.suspended.Load() {
					u.app.QueueEvent(tcell.NewEventResize(0, 0))
				}
			}
		}
	})
	err := u.app.Run()
	cancel()
	wg.Wait()
	return err
}
func safe(s string) string {
	return tview.Escape(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}
func bytesText(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
func ageText(age *float64) string {
	if age == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1fs", *age)
}
func lagText(n core.Node, now time.Time) (string, string) {
	m := n.Lag(now)
	lag := "unknown"
	if m.State == "n/a" {
		return "n/a", "n/a"
	}
	if m.Bytes != nil {
		lag = bytesText(*m.Bytes)
	}
	age := ageText(m.Age)
	if m.State == "stale" {
		lag += " *"
		age += " STALE"
	} else if m.State == "unknown" && m.Bytes != nil {
		lag += " ?"
	}
	return lag, age
}
func role(n core.Node) string {
	if n.Status == nil || !n.Status.Healthy {
		return "unknown"
	}
	value := "primary"
	if n.Status.Recovery {
		value = "replica"
	}
	if n.Error != "" {
		value += " (last)"
	}
	return value
}
func (u *UI) render(width, _ int) {
	v := u.core.Snapshot()
	now := time.Now()
	state, count, agree := v.Consensus()
	summary := "conflicting or unknown views"
	if agree {
		summary = fmt.Sprintf("generation %d | authorized primary %s | reported leader %s", state.Generation, safe(state.Primary), safe(state.Leader))
	}
	u.header.SetText(fmt.Sprintf("[dodgerblue]MAAT[-]  %s\n%s\n%d/%d current status responses | refresh 2s | quorum not verified", safe(u.label), summary, count, len(v.Nodes)))
	headers := []string{"Node", "Agent", "PG health", "PG role", "Replication", "Lag", "Sample age"}
	if width < 100 {
		headers = append(headers[:4], headers[5:]...)
	}
	u.table.Clear()
	for col, text := range headers {
		u.table.SetCell(0, col, tview.NewTableCell(text).SetTextColor(tcell.ColorDodgerBlue).SetSelectable(false))
	}
	var selected *core.Node
	for i, n := range v.Nodes {
		agent, health, receiver := "unreachable", "unknown", "unknown"
		if n.Status != nil && n.Error == "" {
			agent = "reachable"
			health = "unhealthy"
			if n.Status.Healthy {
				health = "healthy"
			}
			if n.Status.Healthy {
				receiver = "not streaming"
				if !n.Status.Recovery {
					receiver = "n/a"
				} else if n.Status.ReceiverStreaming {
					receiver = "streaming"
				}
			}
		}
		lag, age := lagText(n, now)
		cells := []string{safe(n.Target.ID), agent, health, role(n), receiver, lag, age}
		if width < 100 {
			cells = append(cells[:4], cells[5:]...)
		}
		for col, text := range cells {
			cell := tview.NewTableCell(text).SetExpansion(1)
			if n.Error != "" {
				cell.SetTextColor(tcell.ColorYellow)
			}
			u.table.SetCell(i+1, col, cell)
		}
		if n.Target.ID == v.Selected {
			copyNode := n
			selected = &copyNode
			u.table.Select(i+1, 0)
		}
	}
	u.table.SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorDarkSlateBlue))
	if u.mode == "help" {
		u.details.SetTitle(" Help ")
		u.details.SetText("[dodgerblue]Keys[-]\nArrows / j / k: select nodes; scroll details/help\nEnter: node details   Esc: node overview\np: native psql on selected node   ?: help   q: quit\n\n[dodgerblue]Interpretation[-]\nAgent reachability, database observation health, and replication differ.\nHistorical quorum confirmation does not prove current quorum.\nLag is sampled bytes; * means stale, ? means unknown freshness.\nThe 30s display limit is not candidate eligibility.\nAsynchronous replication can lose transactions; actual final loss is unknown.\n\n[dodgerblue]SQL session[-]\nRuns as postgres and allows administrative commands and SQL writes.\nUse \\q to return. Ctrl-C belongs to psql during the session.\nDocker detach keys can leave a session running; abrupt-loss cleanup is not guaranteed.\nMonitoring never starts/stops instances or performs HA/recovery operations.")
	} else if selected != nil {
		u.detail(*selected, now)
	}
	keys := "↑/↓ j/k select"
	if u.mode != "nodes" {
		keys = "↑/↓ j/k scroll"
	}
	u.footer.SetText("[dodgerblue]" + keys + "  Enter details  Esc back  p psql  ? help  q quit[-]")
	message := "Selected " + safe(v.Selected)
	if selected != nil && selected.Error != "" {
		message += " | " + safe(selected.Error)
	}
	if v.Error != "" {
		message += "\n" + safe(v.Error)
	}
	if u.sessionError != "" {
		message += "\n" + safe(u.sessionError)
	}
	u.message.SetText(message)
}
func when(t time.Time) string {
	if t.IsZero() {
		return "not reported"
	}
	return t.Format(time.RFC3339)
}
func reported(s string) string {
	if s == "" {
		return "not reported"
	}
	return safe(s)
}
func databaseValue(healthy bool, value any) string {
	if !healthy {
		return "unknown"
	}
	return fmt.Sprint(value)
}
func lsn(n uint64) string { return fmt.Sprintf("%X/%X", n>>32, n&0xffffffff) }
func (u *UI) detail(n core.Node, now time.Time) {
	title := " Node: " + safe(n.Target.ID) + " "
	if n.Error != "" {
		title += "LAST KNOWN "
	}
	u.details.SetTitle(title)
	text := fmt.Sprintf("[dodgerblue]Instance[-]\nNode: %s\nCluster: %s\nInstance identity: %s\nRuntime: %s\nObserved role: %s\n", safe(n.Target.ID), safe(n.Target.ClusterID), safe(n.Target.InstanceID), safe(n.Target.Runtime), role(n))
	if n.Status == nil {
		text += "Status: not yet validated\n"
	} else {
		s := n.Status
		health := fmt.Sprint(s.Healthy)
		if n.Error != "" {
			health = "unknown; last observation: " + health
		}
		lag, age := lagText(n, now)
		exact := "unavailable"
		if s.LagBytes != nil {
			exact = fmt.Sprintf("%d bytes", *s.LagBytes)
		}
		authorized := "replica"
		if s.Primary == n.Target.ID {
			authorized = "primary"
		}
		if s.Primary == "" {
			authorized = "unknown"
		}
		text += fmt.Sprintf("Database observation healthy: %s\nStatus age: %.1fs\nAuthorized role: %s\n\n[dodgerblue]Replication[-]\nSystem identity: %s\nTimeline: %s\nFlush LSN: %s\nReplay LSN: %s\nReceiver streaming: %s\nUpstream receiver host: %s\nReceived timeline: %s\nReplay paused: %s\nObserved lag: %s (%s)\nPrimary sample age: %s\n\n[dodgerblue]Control plane[-]\nHA generation: %d\nAuthorized primary: %s\nReported Raft leader: %s\nLocal agent is leader: %t\nRaft term: %d\nLast quorum confirmation (historical): %s\n\n[dodgerblue]Reconciliation / recovery[-]\nTransition: %s\nRecovery state: %s\nObservation error: %s\nReconciliation error: %s\nLast successful reconcile: %s\n", health, now.Sub(n.Received).Seconds(), authorized, databaseValue(s.Healthy, reported(s.SystemID)), databaseValue(s.Healthy, s.Timeline), databaseValue(s.Healthy, lsn(s.FlushLSN)), databaseValue(s.Healthy, lsn(s.ReplayLSN)), databaseValue(s.Healthy, s.ReceiverStreaming), databaseValue(s.Healthy, reported(s.ReceiverHost)), databaseValue(s.Healthy, s.ReceivedTimeline), databaseValue(s.Healthy, s.ReplayPaused), lag, exact, age, s.Generation, reported(s.Primary), reported(s.Leader), s.IsLeader, s.Term, when(s.QuorumAt), reported(s.Transition), reported(s.RecoveryState), reported(s.ObservationError), reported(s.ReconcileError), when(s.ReconcileAt))
		if n.Error != "" {
			text += "\n[yellow]All retained database/control fields are last known, not current.[-]\n"
		}
		if s.Healthy && ((s.Primary == n.Target.ID) == s.Recovery) {
			text += "\n[yellow]Observed database role differs from authorization.[-]\n"
		}
	}
	text += "\nObservation health does not prove replication readiness.\nAsynchronous sampled lag cannot establish actual final transaction loss."
	row, col := u.details.GetScrollOffset()
	u.details.SetText(text).ScrollTo(row, col)
}
func (u *UI) show(mode string) {
	u.mode = mode
	if mode == "nodes" {
		u.pages.SwitchToPage("nodes")
		u.app.SetFocus(u.table)
	} else {
		u.pages.SwitchToPage("details")
		u.details.ScrollToBeginning()
		u.app.SetFocus(u.details)
	}
}
func (u *UI) key(e *tcell.EventKey) *tcell.EventKey {
	if u.suspended.Load() {
		return nil
	}
	if e.Key() == tcell.KeyCtrlC || e.Rune() == 'q' {
		u.app.Stop()
		return nil
	}
	if e.Key() == tcell.KeyEscape {
		u.show("nodes")
		return nil
	}
	if e.Rune() == '?' {
		u.show("help")
		return nil
	}
	if e.Rune() == 'p' {
		u.suspended.Store(true)
		u.core.Pause()
		ok := u.app.Suspend(func() {
			if err := u.core.OpenSession(u.ctx); err != nil {
				u.sessionError = err.Error()
			} else {
				u.sessionError = ""
			}
		})
		if !ok {
			u.sessionError = "terminal suspension failed"
		}
		u.core.Resume()
		u.suspended.Store(false)
		select {
		case u.request <- struct{}{}:
		default:
		}
		return nil
	}
	if u.mode == "nodes" {
		switch {
		case e.Key() == tcell.KeyEnter:
			u.show("details")
			return nil
		case e.Key() == tcell.KeyDown || e.Rune() == 'j':
			u.core.Move(1)
			return nil
		case e.Key() == tcell.KeyUp || e.Rune() == 'k':
			u.core.Move(-1)
			return nil
		}
	}
	return e
}
