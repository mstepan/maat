package tui

import (
	"context"
	"errors"
	"github.com/gdamore/tcell/v2"
	"maat/dashboard/internal/core"
	"strings"
	"testing"
	"time"
)

type source struct{}

func (source) Discover(context.Context) ([]core.Target, error) {
	return []core.Target{{ID: "a", ClusterID: "lab", InstanceID: "one", Available: true}, {ID: "b", ClusterID: "lab", InstanceID: "two", Available: true}}, nil
}

type reader struct{ fail, unhealthy bool }

func (r *reader) Read(_ context.Context, t core.Target) (core.Status, error) {
	if r.fail {
		return core.Status{}, errors.New("offline")
	}
	return core.Status{Members: map[string]string{"a": "one", "b": "two"}, ID: t.ID, ClusterID: t.ClusterID, InstanceID: t.InstanceID, Healthy: !r.unhealthy, Recovery: !r.unhealthy, Generation: 7, Primary: "a", Leader: "b", Timeline: 3, ReceiverHost: "upstream"}, nil
}

type session struct{ calls int }

func (s *session) Run(context.Context, core.Target) error {
	s.calls++
	return errors.New("launch failed")
}
func TestNavigationAndStaleRendering(t *testing.T) {
	r := &reader{}
	a := core.New(source{}, r, &session{})
	_ = a.Refresh(t.Context())
	screen := tcell.NewSimulationScreen("")
	u := New(a, "lab", screen)
	defer screen.Fini()
	screen.SetSize(80, 24)
	u.render(80, 24)
	u.key(tcell.NewEventKey(tcell.KeyRune, 'j', 0))
	u.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	u.render(80, 24)
	if u.mode != "details" || a.Snapshot().Selected != "b" || !strings.Contains(u.details.GetText(true), "upstream") {
		t.Fatal("details navigation missing")
	}
	r.fail = true
	_ = a.Refresh(t.Context())
	u.key(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
	u.render(80, 24)
	if u.table.GetCell(2, 1).Text != "unreachable" || u.table.GetCell(2, 2).Text != "unknown" {
		t.Fatal("stale node displayed healthy")
	}
	if strings.Contains(safe("\x1b[31m[red]bad\x07"), "\x1b") || safe("[red]") == "[red]" {
		t.Fatal("untrusted terminal markup not escaped")
	}
}

func TestUnhealthyObservationDoesNotFabricateDatabaseFields(t *testing.T) {
	a := core.New(source{}, &reader{}, &session{})
	_ = a.Refresh(t.Context())
	screen := tcell.NewSimulationScreen("")
	u := New(a, "lab", screen)
	defer screen.Fini()
	n := a.Snapshot().Nodes[0]
	n.Status.Healthy = false
	u.detail(n, n.Received)
	text := u.details.GetText(true)
	if !strings.Contains(text, "Timeline: unknown") || !strings.Contains(text, "Replay LSN: unknown") || !strings.Contains(text, "Receiver streaming: unknown") {
		t.Fatal("unhealthy observation defaults presented as database evidence")
	}
}

func TestSessionFailureRestoresScreenAndSelection(t *testing.T) {
	s := &session{}
	a := core.New(source{}, &reader{}, s)
	_ = a.Refresh(t.Context())
	screen := tcell.NewSimulationScreen("")
	u := New(a, "lab", screen)
	defer screen.Fini()
	u.key(tcell.NewEventKey(tcell.KeyRune, 'j', 0))
	u.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	u.key(tcell.NewEventKey(tcell.KeyRune, 'p', 0))
	if s.calls != 1 || u.mode != "details" || a.Snapshot().Selected != "b" || u.sessionError != "launch failed" || u.suspended.Load() {
		t.Fatalf("handoff did not restore state: %s %s", u.mode, u.sessionError)
	}
	select {
	case <-u.request:
	default:
		t.Fatal("missing immediate refresh request")
	}
}

func TestRunHandlesQueuedNavigationAndQuit(t *testing.T) {
	a := core.New(source{}, &reader{}, &session{})
	_ = a.Refresh(t.Context())
	screen := tcell.NewSimulationScreen("")
	u := New(a, "lab", screen)
	ready := make(chan struct{}, 1)
	u.app.SetAfterDrawFunc(func(tcell.Screen) {
		select {
		case ready <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("initial draw hung")
	}
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyRune, 'j', 0))
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyRune, 'q', 0))
	select {
	case err := <-done:
		if err != nil || u.mode != "details" || a.Snapshot().Selected != "b" {
			t.Fatalf("run result: %v, %s", err, u.mode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("navigation or quit hung")
	}
}

func TestUnhealthyReceiverStateIsUnknown(t *testing.T) {
	a := core.New(source{}, &reader{unhealthy: true}, &session{})
	_ = a.Refresh(t.Context())
	screen := tcell.NewSimulationScreen("")
	u := New(a, "lab", screen)
	defer screen.Fini()
	u.render(100, 30)
	if u.table.GetCell(1, 4).Text != "unknown" {
		t.Fatal("unhealthy receiver was represented as known")
	}
}
