package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	client "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

const execTimeout = 5 * time.Minute

// execEvent carries one piece of streaming exec state to the model: an output
// chunk, or the terminal completion.
type execEvent struct {
	out  []byte
	errb []byte
	done bool
	res  wire.ExecResult
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
		wire.ExecRequest{Cmd: []string{"sh", "-c", cmdline}},
		&chanWriter{ch: ch}, &chanWriter{ch: ch, stderr: true})
	ch <- execEvent{done: true, res: res, fail: err}
}

// attachShell opens a long-lived interactive shell in the sandbox via
// client.ExecInteractive and streams its output into ch, exactly like
// runExec but for a persistent session. The caller writes each entered line
// to the returned pipe as stdin; closing the pipe (or the shell exiting)
// ends the session with a done event. No timeout: an interactive session is
// user-paced, and detach/quit/close tears it down.
func attachShell(cl *client.Client, id string, ch chan execEvent) *io.PipeWriter {
	pr, pw := io.Pipe()
	go func() {
		res, err := cl.ExecInteractive(context.Background(), id,
			wire.ExecRequest{Cmd: []string{"/bin/sh"}},
			pr, &chanWriter{ch: ch}, &chanWriter{ch: ch, stderr: true})
		ch <- execEvent{done: true, res: res, fail: err}
	}()
	return pw
}

// sendStdin writes one line (plus newline) to a live shell's stdin. It runs
// as a command so a synchronous io.Pipe write can never block the update
// loop. Output arrives separately via the shell's frame stream on execCh.
func sendStdin(pw *io.PipeWriter, line string) tea.Cmd {
	return func() tea.Msg {
		_, _ = pw.Write([]byte(line + "\n"))
		return nil
	}
}

// waitExec reads the next streaming event. On each event the model re-issues
// this command, so output flows in live until the done event arrives.
func waitExec(ch chan execEvent) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// exitLine renders the summary shown when an exec finishes: a filled exit chip
// (green on success, red otherwise) followed by the duration in dim meta.
func exitLine(res wire.ExecResult, fail error) string {
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
