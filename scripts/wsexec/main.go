// Command wsexec is a smoke-test client for the WebSocket interactive exec
// endpoint (GET /sandboxes/{id}/exec + upgrade). It is the non-Go-SDK
// stand-in: it speaks the exact contract a TS/Python client would — first
// message is the JSON ExecRequest, then binary messages whose concatenated
// payloads are the wire frame stream (stdin/stdin-close up, stdout/stderr/
// exit down).
//
// Usage:
//
//	wsexec -addr 127.0.0.1:7878 -id sbx_ab12cd34 [-token KEY] -- CMD [ARGS…]
//
// Stdin is forwarded as FrameStdin chunks (FrameStdinClose on EOF), stdout/
// stderr frames go to the matching file descriptors, and the process exits
// with the command's exit code (or 125 on transport errors, mirroring the
// docker convention for daemon-side failures).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/gnana997/crucible/sdk/wire"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7878", "daemon host:port")
	id := flag.String("id", "", "sandbox id (required)")
	token := flag.String("token", "", "bearer token (optional)")
	flag.Parse()
	if *id == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: wsexec -addr HOST:PORT -id SBX [-token KEY] -- CMD [ARGS…]")
		os.Exit(125)
	}

	os.Exit(run(*addr, *id, *token, flag.Args()))
}

func run(addr, id, token string, cmd []string) int {
	dialCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &websocket.DialOptions{}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + token}}
	}
	c, _, err := websocket.Dial(dialCtx, "ws://"+addr+"/sandboxes/"+id+"/exec", opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsexec: dial: %v\n", err)
		return 125
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(1 << 20)

	// First message: the ExecRequest.
	body, _ := json.Marshal(wire.ExecRequest{Cmd: cmd})
	if err := c.Write(dialCtx, websocket.MessageText, body); err != nil {
		fmt.Fprintf(os.Stderr, "wsexec: send exec request: %v\n", err)
		return 125
	}

	// Everything after is the frame byte stream over binary messages.
	ctx := context.Background()
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	// Stdin → FrameStdin chunks, FrameStdinClose on EOF.
	go func() {
		fw := wire.NewFrameWriter(nc)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if werr := fw.WriteFrame(wire.FrameStdin, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = fw.WriteFrame(wire.FrameStdinClose, nil)
				return
			}
		}
	}()

	for {
		f, err := wire.ReadFrame(nc)
		if err != nil {
			if err == io.EOF {
				fmt.Fprintln(os.Stderr, "wsexec: stream ended without an exit frame")
			} else {
				fmt.Fprintf(os.Stderr, "wsexec: read frame: %v\n", err)
			}
			return 125
		}
		switch f.Type {
		case wire.FrameStdout:
			_, _ = os.Stdout.Write(f.Payload)
		case wire.FrameStderr:
			_, _ = os.Stderr.Write(f.Payload)
		case wire.FrameExit:
			var res wire.ExecResult
			if err := json.Unmarshal(f.Payload, &res); err != nil {
				fmt.Fprintf(os.Stderr, "wsexec: decode exit frame: %v\n", err)
				return 125
			}
			if res.Error != "" {
				fmt.Fprintf(os.Stderr, "wsexec: agent error: %s\n", res.Error)
			}
			return res.ExitCode
		}
	}
}
