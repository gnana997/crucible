package mcpserver

import (
	"context"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect wires a client to a freshly built server over an in-memory
// transport and returns the live client session.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := New(Config{})

	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestServerAdvertisesFullCatalog(t *testing.T) {
	cs := connect(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	got := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
		if tl.InputSchema == nil {
			t.Errorf("tool %q advertises no input schema", tl.Name)
		}
		if tl.Description == "" {
			t.Errorf("tool %q advertises no description", tl.Name)
		}
	}
	sort.Strings(got)

	want := []string{
		"create_sandbox", "delete_sandbox", "delete_snapshot", "exec", "fork",
		"inspect_sandbox", "list_profiles", "list_sandboxes", "list_snapshots",
		"run", "snapshot",
	}
	if len(got) != len(want) {
		t.Fatalf("tools = %v (%d), want %d", got, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStubHandlerReportsNotImplemented(t *testing.T) {
	cs := connect(t)

	// A stub handler returns an error, which the SDK packs into the tool
	// result as IsError rather than a transport-level failure.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_profiles",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError {
		t.Fatal("stub tool result: IsError = false, want true")
	}
}
