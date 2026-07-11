// crucible-bench is a reproducible benchmark harness for a crucible daemon. It
// drives the daemon through the crucible SDK (the same typed client the CLI, TUI,
// and MCP server use) and reports latency distributions, fork fan-out scaling,
// the lazy-memory efficiency of fork, and sandbox density.
//
// It creates and deletes real microVMs, so point it at a daemon you own:
//
//	crucible-bench --addr 127.0.0.1:7878 --profile python-3.12
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	client "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

var (
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2913D")).Bold(true)
	head   = lipgloss.NewStyle().Foreground(lipgloss.Color("#86A9B4")).Bold(true)
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("#8792A1"))
	okc    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6FD08A")).Bold(true)
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:7878", "daemon address")
		token    = flag.String("token", "", "API token")
		skipTLS  = flag.Bool("tls-skip-verify", false, "skip TLS verification")
		profile  = flag.String("profile", "", "rootfs profile (empty = daemon default)")
		vcpus    = flag.Int("vcpus", 1, "vCPUs per sandbox")
		memMiB   = flag.Int("memory", 512, "memory (MiB) per sandbox")
		samples  = flag.Int("samples", 30, "latency samples per operation")
		warmup   = flag.Int("warmup", 3, "warmup iterations (discarded)")
		fanoutS  = flag.String("fanout", "1,4,16,64,128", "fork fan-out sizes")
		memForks = flag.Int("mem-forks", 64, "forks for the memory-efficiency test")
		density  = flag.Int("density", 0, "density target (0 = skip); forks until this many live or a failure")
		phases   = flag.String("phases", "latency,fanout,memory,density", "phases to run")
		jsonOut  = flag.String("json", "", "write machine-readable results to this path")
	)
	flag.Parse()

	var opts []client.Option
	if *token != "" {
		opts = append(opts, client.WithToken(*token))
	}
	if *skipTLS {
		opts = append(opts, client.WithInsecureSkipVerify())
	}
	cl := client.New(*addr, opts...)

	b := &bench{
		cl: cl, profile: *profile, vcpus: *vcpus, memMiB: *memMiB,
		samples: *samples, warmup: *warmup,
		results: map[string]any{},
	}
	run := func(p string) bool { return strings.Contains(","+*phases+",", ","+p+",") }

	fmt.Println(accent.Render("crucible-bench") + dim.Render("  →  "+*addr))
	printEnv(b)

	ctx := context.Background()
	if err := cl.Health(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "daemon unreachable:", err)
		os.Exit(1)
	}

	if run("latency") {
		b.latency(ctx)
	}
	if run("fanout") {
		b.fanout(ctx, parseInts(*fanoutS))
	}
	if run("memory") {
		b.memory(ctx, *memForks)
	}
	if run("density") && *density > 0 {
		b.density(ctx, *density)
	}

	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err == nil {
			enc := json.NewEncoder(f)
			enc.SetIndent("", "  ")
			_ = enc.Encode(b.results)
			_ = f.Close()
			fmt.Println(dim.Render("\nwrote " + *jsonOut))
		}
	}
}

type bench struct {
	cl              *client.Client
	profile         string
	vcpus, memMiB   int
	samples, warmup int
	results         map[string]any
}

func (b *bench) createReq() api.CreateSandboxRequest {
	return api.CreateSandboxRequest{VCPUs: b.vcpus, MemoryMiB: b.memMiB, Profile: b.profile}
}

// --- phases -----------------------------------------------------------------

func (b *bench) latency(ctx context.Context) {
	fmt.Println("\n" + head.Render("① latency") + dim.Render(fmt.Sprintf("  (%d samples, %d warmup discarded)", b.samples, b.warmup)))

	// cold create: create → agent ready, then delete
	create := b.measure("create (cold → ready)", func() time.Duration {
		t0 := time.Now()
		sb, err := b.cl.CreateSandbox(ctx, b.createReq())
		d := time.Since(t0)
		if err != nil {
			fatal("create", err)
		}
		_ = b.cl.DeleteSandbox(ctx, sb.ID)
		return d
	})

	// exec roundtrip on one warm sandbox
	warm, err := b.cl.CreateSandbox(ctx, b.createReq())
	if err != nil {
		fatal("create warm", err)
	}
	exec := b.measure("exec roundtrip (true)", func() time.Duration {
		t0 := time.Now()
		_, err := b.cl.Exec(ctx, warm.ID, wire.ExecRequest{Cmd: []string{"true"}}, io.Discard, io.Discard)
		if err != nil {
			fatal("exec", err)
		}
		return time.Since(t0)
	})

	// snapshot the warm sandbox repeatedly
	snap := b.measure("snapshot", func() time.Duration {
		t0 := time.Now()
		s, err := b.cl.Snapshot(ctx, warm.ID)
		d := time.Since(t0)
		if err != nil {
			fatal("snapshot", err)
		}
		_ = b.cl.DeleteSnapshot(ctx, s.ID)
		return d
	})

	// fork one child from a stable snapshot
	base, err := b.cl.Snapshot(ctx, warm.ID)
	if err != nil {
		fatal("snapshot base", err)
	}
	fork := b.measure("fork (warm → child)", func() time.Duration {
		t0 := time.Now()
		kids, err := b.cl.Fork(ctx, base.ID, 1)
		d := time.Since(t0)
		if err != nil {
			fatal("fork", err)
		}
		for _, k := range kids {
			_ = b.cl.DeleteSandbox(ctx, k.ID)
		}
		return d
	})

	_ = b.cl.DeleteSnapshot(ctx, base.ID)
	_ = b.cl.DeleteSandbox(ctx, warm.ID)

	b.results["latency"] = map[string]any{
		"create_ms": create, "exec_ms": exec, "snapshot_ms": snap, "fork_ms": fork,
	}
}

func (b *bench) fanout(ctx context.Context, sizes []int) {
	fmt.Println("\n" + head.Render("② fork fan-out") + dim.Render("  (fork N children from one snapshot, in a single call)"))
	parent, err := b.cl.CreateSandbox(ctx, b.createReq())
	if err != nil {
		fatal("fanout parent", err)
	}
	snap, err := b.cl.Snapshot(ctx, parent.ID)
	if err != nil {
		fatal("fanout snapshot", err)
	}

	fmt.Printf("  %-10s %-14s %-16s %s\n", head.Render("children"), head.Render("total"), head.Render("per-child"), head.Render("throughput"))
	var rows []map[string]any
	for _, n := range sizes {
		t0 := time.Now()
		kids, err := b.cl.Fork(ctx, snap.ID, n)
		total := time.Since(t0)
		if err != nil {
			fmt.Printf("  %-10d %s\n", n, dim.Render("failed: "+err.Error()))
			continue
		}
		perChild := total / time.Duration(len(kids))
		tput := float64(len(kids)) / total.Seconds()
		fmt.Printf("  %-10d %-14s %-16s %s\n",
			n, ms(total), ms(perChild), accent.Render(fmt.Sprintf("%.1f/s", tput)))
		rows = append(rows, map[string]any{
			"children": len(kids), "total_ms": round(total), "per_child_ms": round(perChild), "per_sec": tput,
		})
		deleteAll(ctx, b.cl, kids)
	}
	_ = b.cl.DeleteSnapshot(ctx, snap.ID)
	_ = b.cl.DeleteSandbox(ctx, parent.ID)
	b.results["fanout"] = rows
}

func (b *bench) memory(ctx context.Context, n int) {
	fmt.Println("\n" + head.Render("③ memory efficiency") + dim.Render(fmt.Sprintf("  (fork %d children, measure host RAM vs naïve %d × %d MiB)", n, n, b.memMiB)))
	parent, err := b.cl.CreateSandbox(ctx, b.createReq())
	if err != nil {
		fatal("memory parent", err)
	}
	snap, err := b.cl.Snapshot(ctx, parent.ID)
	if err != nil {
		fatal("memory snapshot", err)
	}

	before := memAvailableMiB()
	t0 := time.Now()
	kids, err := b.cl.Fork(ctx, snap.ID, n)
	forkWall := time.Since(t0)
	if err != nil {
		fmt.Printf("  %s\n", dim.Render("fork failed: "+err.Error()))
		_ = b.cl.DeleteSnapshot(ctx, snap.ID)
		_ = b.cl.DeleteSandbox(ctx, parent.ID)
		return
	}
	time.Sleep(1500 * time.Millisecond) // let pages settle
	after := memAvailableMiB()
	used := before - after
	if used < 1 {
		used = 1
	}
	naive := int64(len(kids) * b.memMiB)
	perFork := float64(used) / float64(len(kids))
	ratio := float64(naive) / float64(used)

	fmt.Printf("  %s %s in %s\n", accent.Render(strconv.Itoa(len(kids))), dim.Render("live forks"), ms(forkWall))
	fmt.Printf("  host RAM used:   %s\n", accent.Render(fmt.Sprintf("%d MiB", used))+dim.Render(fmt.Sprintf("  (~%.1f MiB/fork)", perFork)))
	fmt.Printf("  naïve N×mem:     %s\n", dim.Render(fmt.Sprintf("%d MiB", naive)))
	fmt.Printf("  efficiency:      %s %s\n", okc.Render(fmt.Sprintf("%.1f×", ratio)), dim.Render("less memory than copying per fork"))

	b.results["memory"] = map[string]any{
		"forks": len(kids), "host_ram_used_mib": used, "naive_mib": naive,
		"per_fork_mib": perFork, "efficiency_x": ratio, "fork_wall_ms": round(forkWall),
	}
	deleteAll(ctx, b.cl, kids)
	_ = b.cl.DeleteSnapshot(ctx, snap.ID)
	_ = b.cl.DeleteSandbox(ctx, parent.ID)
}

func (b *bench) density(ctx context.Context, target int) {
	fmt.Println("\n" + head.Render("④ density") + dim.Render(fmt.Sprintf("  (fork toward %d live sandboxes or the first failure)", target)))
	parent, err := b.cl.CreateSandbox(ctx, b.createReq())
	if err != nil {
		fatal("density parent", err)
	}
	snap, err := b.cl.Snapshot(ctx, parent.ID)
	if err != nil {
		fatal("density snapshot", err)
	}
	var live []api.SandboxResponse
	batch := 64
	for len(live) < target {
		n := batch
		if rem := target - len(live); rem < n {
			n = rem
		}
		kids, err := b.cl.Fork(ctx, snap.ID, n)
		live = append(live, kids...)
		fmt.Printf("\r  %s live  (%s free)   ", accent.Render(strconv.Itoa(len(live))), dim.Render(fmt.Sprintf("%d MiB", memAvailableMiB())))
		if err != nil {
			fmt.Printf("\n  %s\n", dim.Render("stopped at first failure: "+err.Error()))
			break
		}
	}
	fmt.Printf("\n  peak: %s live sandboxes\n", okc.Render(strconv.Itoa(len(live))))
	b.results["density"] = map[string]any{"peak_live": len(live), "mem_free_mib": memAvailableMiB()}
	deleteAll(ctx, b.cl, live)
	_ = b.cl.DeleteSnapshot(ctx, snap.ID)
	_ = b.cl.DeleteSandbox(ctx, parent.ID)
}

// --- helpers ----------------------------------------------------------------

// measure runs op warmup+samples times, discards warmup, prints a distribution,
// and returns the {p50,p90,p99,min,max} in ms.
func (b *bench) measure(name string, op func() time.Duration) map[string]float64 {
	for i := 0; i < b.warmup; i++ {
		op()
	}
	ds := make([]time.Duration, 0, b.samples)
	for i := 0; i < b.samples; i++ {
		ds = append(ds, op())
		fmt.Printf("\r  %-24s %s", name, dim.Render(fmt.Sprintf("%d/%d", i+1, b.samples)))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p := func(q float64) float64 { return round(ds[int(q*float64(len(ds)-1))]) }
	m := map[string]float64{"p50": p(0.50), "p90": p(0.90), "p99": p(0.99), "min": p(0), "max": p(1)}
	fmt.Printf("\r  %-24s p50 %s   p90 %s   p99 %s   %s\n",
		name, accent.Render(fmtms(m["p50"])), accent.Render(fmtms(m["p90"])), accent.Render(fmtms(m["p99"])),
		dim.Render(fmt.Sprintf("(min %s, max %s, n=%d)", fmtms(m["min"]), fmtms(m["max"]), len(ds))))
	return m
}

func deleteAll(ctx context.Context, cl *client.Client, sbs []api.SandboxResponse) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, s := range sbs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			_ = cl.DeleteSandbox(ctx, id)
		}(s.ID)
	}
	wg.Wait()
}

func memAvailableMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "MemAvailable:") {
			var kb int64
			_, _ = fmt.Sscanf(sc.Text(), "MemAvailable: %d kB", &kb)
			return kb / 1024
		}
	}
	return 0
}

func printEnv(b *bench) {
	host, _ := os.Hostname()
	cpu := firstMatch("/proc/cpuinfo", "model name")
	fmt.Println(dim.Render(fmt.Sprintf("  host %s · %s · %d MiB free · sandbox %dc/%dMiB · profile %q",
		host, cpu, memAvailableMiB(), b.vcpus, b.memMiB, orDefault(b.profile))))
}

func firstMatch(path, prefix string) string {
	f, err := os.Open(path)
	if err != nil {
		return "?"
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), prefix) {
			if i := strings.Index(sc.Text(), ":"); i >= 0 {
				return strings.TrimSpace(sc.Text()[i+1:])
			}
		}
	}
	return "?"
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func parseInts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func round(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
func ms(d time.Duration) string     { return accent.Render(fmtms(round(d))) }
func fmtms(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.2fs", v/1000)
	}
	return fmt.Sprintf("%.1fms", v)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "\n%s failed: %v\n", what, err)
	os.Exit(1)
}
