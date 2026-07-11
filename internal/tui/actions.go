package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/sdk/api"
)

// actionTimeout bounds a mutating action. Create/fork boot a microVM, so it's
// generous — well above the read requestTimeout.
const actionTimeout = 90 * time.Second

// actionMsg reports the outcome of a mutating action. The model turns it into a
// status-line notice and refreshes the list so the change shows immediately.
type actionMsg struct {
	verb   string // create | snapshot | fork | delete
	detail string // e.g. the new/removed id, for the notice
	err    error
}

func (m model) createCmd() tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		sb, err := cl.CreateSandbox(ctx, api.CreateSandboxRequest{})
		return actionMsg{verb: "create", detail: sb.ID, err: err}
	}
}

func (m model) snapshotCmd(id string) tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		snap, err := cl.Snapshot(ctx, id)
		return actionMsg{verb: "snapshot", detail: snap.ID, err: err}
	}
}

func (m model) forkCmd(snapID string) tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		sbs, err := cl.Fork(ctx, snapID, 1)
		detail := ""
		if len(sbs) > 0 {
			detail = sbs[0].ID
		}
		return actionMsg{verb: "fork", detail: detail, err: err}
	}
}

func (m model) deleteCmd(id string) tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		err := cl.DeleteSandbox(ctx, id)
		return actionMsg{verb: "delete", detail: id, err: err}
	}
}

// can reports whether the presenting token is permitted to perform op. An
// unscoped (full-access) token allows everything; before whoami loads we're
// optimistic (the daemon enforces the real ceiling regardless).
func (m model) can(op policy.Operation) bool {
	if !m.whoami.Scoped || m.whoami.Policy == nil {
		return true
	}
	return m.whoami.Policy.Allows(op)
}

// latestSnapshotOf returns the newest snapshot taken from sandboxID, if any —
// the natural thing to fork from when the user presses [f] on a sandbox.
func latestSnapshotOf(snaps []api.SnapshotResponse, sandboxID string) (api.SnapshotResponse, bool) {
	var best api.SnapshotResponse
	found := false
	for _, s := range snaps {
		if s.SourceID != sandboxID {
			continue
		}
		if !found || s.CreatedAt.After(best.CreatedAt) {
			best, found = s, true
		}
	}
	return best, found
}

// actionOK renders the success notice for a completed action.
func actionOK(msg actionMsg) string {
	switch msg.verb {
	case "create":
		return "created " + msg.detail
	case "snapshot":
		return "snapshot " + msg.detail
	case "fork":
		return "forked " + msg.detail
	case "delete":
		return "deleted " + msg.detail
	default:
		return msg.verb + " ok"
	}
}
