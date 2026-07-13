package main

import (
	"errors"
	"io"
	"os"

	client "github.com/gnana997/crucible/sdk"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// captureFlags are shared by `sandbox capture` and `app capture`.
type captureFlags struct {
	filter     string
	out        string
	snaplen    int
	maxBytes   int
	maxSeconds int
}

func (cf *captureFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&cf.filter, "filter", "", "BPF filter expression, e.g. 'tcp port 80 and host 10.0.0.5'")
	f.StringVarP(&cf.out, "write", "w", "", "write pcap to this file instead of stdout")
	f.IntVar(&cf.snaplen, "snaplen", 0, "bytes captured per packet (0 = whole packet)")
	f.IntVar(&cf.maxBytes, "max-bytes", 0, "stop after this many bytes (0 = daemon default, 50 MiB)")
	f.IntVar(&cf.maxSeconds, "max-seconds", 0, "stop after this many seconds (0 = daemon default, 60 s)")
}

func (cf *captureFlags) run(cmd *cobra.Command, o *globalOpts, instanceID string) error {
	var w io.Writer
	if cf.out != "" {
		f, err := os.Create(cf.out)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
	} else {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return errors.New("refusing to write binary pcap to a terminal; use -w <file> or pipe it (e.g. | wireshark -k -i -)")
		}
		w = cmd.OutOrStdout()
	}
	return o.client().Capture(cmd.Context(), instanceID, client.CaptureOptions{
		Filter:     cf.filter,
		Snaplen:    cf.snaplen,
		MaxBytes:   int64(cf.maxBytes),
		MaxSeconds: cf.maxSeconds,
	}, w)
}

func newSandboxCaptureCmd(o *globalOpts) *cobra.Command {
	cf := &captureFlags{}
	cmd := &cobra.Command{
		Use:   "capture <sandbox-id>",
		Short: "Capture a sandbox's network traffic as pcap (needs the 'capture' scoped op)",
		Long: "Stream a live packet capture of a sandbox's traffic in pcap format. Captured\n" +
			"host-side on the sandbox's veth — no in-guest tcpdump needed, so it works for\n" +
			"distroless/scratch images. Requires a token granted the 'capture' operation.\n" +
			"Bounded by --max-bytes / --max-seconds; write with -w or pipe to Wireshark.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cf.run(cmd, o, args[0])
		},
	}
	cf.bind(cmd)
	return cmd
}

func newAppCaptureCmd(o *globalOpts) *cobra.Command {
	cf := &captureFlags{}
	cmd := &cobra.Command{
		Use:   "capture <app>",
		Short: "Capture the app's current instance network traffic as pcap",
		Long: "Like `sandbox capture`, but addresses the app by name — the daemon resolves\n" +
			"its current instance. Requires the 'capture' scoped op.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := o.appInstanceID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return cf.run(cmd, o, inst)
		},
	}
	cf.bind(cmd)
	return cmd
}
