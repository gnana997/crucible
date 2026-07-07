package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/client"
)

const execTimeout = 5 * time.Minute

// execEvent carries one piece of streaming exec state to the model: an output
// chunk, or the terminal completion.
type execEvent struct {
	out  []byte
	errb []byte
	done bool
	res  agentwire.ExecResult
	fail error
}

// chanWriter is the io.Writer we hand to client.Exec; each frame becomes an
// execEvent the Bubble Tea model consumes via waitExec.
type chanWriter struct {
	ch     chan execEvent
	stderr bool
}

func (w *chanWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p)) // copy: the frame buffer is reused
	copy(b, p)
	if w.stderr {
		w.ch <- execEvent{errb: b}
	} else {
		w.ch <- execEvent{out: b}
	}
	return len(p), nil
}

// runExec streams a command's output into ch and sends a final done event. It
// runs in its own goroutine; the model consumes ch one event at a time via
// waitExec, which provides backpressure.
func runExec(cl *client.Client, id, cmdline string, ch chan execEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	res, err := cl.Exec(ctx, id,
		agentwire.ExecRequest{Cmd: []string{"sh", "-c", cmdline}},
		&chanWriter{ch: ch}, &chanWriter{ch: ch, stderr: true})
	ch <- execEvent{done: true, res: res, fail: err}
}

// waitExec reads the next streaming event. On each event the model re-issues
// this command, so output flows in live until the done event arrives.
func waitExec(ch chan execEvent) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// exitLine renders the summary shown when an exec finishes: a filled exit chip
// (green on success, red otherwise) followed by the duration in dim meta.
func exitLine(res agentwire.ExecResult, fail error) string {
	if fail != nil {
		return exitBadChip.Render("error") + " " + metaStyle.Render(fail.Error())
	}
	extra := ""
	switch {
	case res.TimedOut:
		extra = " · timed out"
	case res.OomKilled:
		extra = " · OOM"
	case res.Signal != "":
		extra = " · " + res.Signal
	}
	dur := metaStyle.Render(fmt.Sprintf("%dms%s", res.DurationMs, extra))
	if res.ExitCode == 0 {
		return exitOKChip.Render("exit 0") + " " + dur
	}
	return exitBadChip.Render(fmt.Sprintf("exit %d", res.ExitCode)) + " " + dur
}

// sandboxDetail renders the selected sandbox's metadata block for the detail view.
func sandboxDetail(sb api.SandboxResponse) string {
	lines := []string{sbxNodeStyle.Render(sb.ID)}
	lines = append(lines, metaStyle.Render(fmt.Sprintf(
		"profile %s   cpu/mem %dc/%dM   age %s",
		dash(sb.Profile), sb.VCPUs, sb.MemoryMiB, shortDur(time.Since(sb.CreatedAt)))))
	lines = append(lines, metaStyle.Render("network  "+netLabel(sb.Network)))
	if sb.SourceSnapshotID != "" {
		lines = append(lines, metaStyle.Render("forked from  "+sb.SourceSnapshotID))
	}
	return strings.Join(lines, "\n")
}
