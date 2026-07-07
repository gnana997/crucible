package tui

import (
	"sort"
	"strings"

	"github.com/gnana997/crucible/internal/api"
)

// renderTree builds the fork genealogy from the sandbox + snapshot lists and
// returns it as indented text. Roots are created (non-forked) sandboxes; the
// hierarchy alternates sandbox → snapshot → forked sandbox → snapshot → …,
// using snapshot.source_id and sandbox.source_snapshot_id as the two edges.
func renderTree(sandboxes []api.SandboxResponse, snapshots []api.SnapshotResponse) string {
	if len(sandboxes) == 0 && len(snapshots) == 0 {
		return metaStyle.Render("no sandboxes yet — create or fork one to grow the tree")
	}

	snapsBySource := map[string][]api.SnapshotResponse{} // sandbox id → snapshots taken from it
	for _, s := range snapshots {
		snapsBySource[s.SourceID] = append(snapsBySource[s.SourceID], s)
	}
	forksBySnap := map[string][]api.SandboxResponse{} // snapshot id → sandboxes forked from it
	var roots []api.SandboxResponse
	for _, sb := range sandboxes {
		if sb.SourceSnapshotID == "" {
			roots = append(roots, sb)
		} else {
			forksBySnap[sb.SourceSnapshotID] = append(forksBySnap[sb.SourceSnapshotID], sb)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })

	var b strings.Builder
	var sandboxChildren func(sb api.SandboxResponse, prefix string)
	var snapshotChildren func(s api.SnapshotResponse, prefix string)

	sandboxChildren = func(sb api.SandboxResponse, prefix string) {
		snaps := snapsBySource[sb.ID]
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
		for i, s := range snaps {
			cp := writeNode(&b, prefix, snapshotLabel(s), i == len(snaps)-1)
			snapshotChildren(s, cp)
		}
	}
	snapshotChildren = func(s api.SnapshotResponse, prefix string) {
		forks := forksBySnap[s.ID]
		sort.Slice(forks, func(i, j int) bool { return forks[i].ID < forks[j].ID })
		for i, f := range forks {
			cp := writeNode(&b, prefix, sandboxLabel(f), i == len(forks)-1)
			sandboxChildren(f, cp)
		}
	}

	for i, r := range roots {
		b.WriteString(sandboxLabel(r) + "\n")
		sandboxChildren(r, "")
		if i < len(roots)-1 {
			b.WriteString("\n")
		}
	}

	// Orphan snapshots — whose source sandbox is already deleted — still belong
	// on the tree; surface them so nothing hides.
	var orphans []api.SnapshotResponse
	for _, s := range snapshots {
		if _, ok := indexOfSandbox(sandboxes, s.SourceID); !ok {
			orphans = append(orphans, s)
		}
	}
	if len(orphans) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(metaStyle.Render("orphan snapshots (source sandbox gone):") + "\n")
		sort.Slice(orphans, func(i, j int) bool { return orphans[i].ID < orphans[j].ID })
		for i, s := range orphans {
			cp := writeNode(&b, "", snapshotLabel(s), i == len(orphans)-1)
			snapshotChildren(s, cp)
		}
	}

	return b.String()
}

// writeNode writes "prefix + connector + label" and returns the prefix children
// should use (the connector's continuation).
func writeNode(b *strings.Builder, prefix, label string, last bool) string {
	connector, cont := "├─ ", "│  "
	if last {
		connector, cont = "└─ ", "   "
	}
	b.WriteString(treeStyle.Render(prefix+connector) + label + "\n")
	return prefix + cont
}

func sandboxLabel(sb api.SandboxResponse) string {
	label := sbxNodeStyle.Render("● " + sb.ID)
	var extra []string
	if sb.Profile != "" {
		extra = append(extra, sb.Profile)
	}
	if sb.Network != nil && sb.Network.Enabled && sb.Network.GuestIP != "" {
		extra = append(extra, sb.Network.GuestIP)
	}
	if len(extra) > 0 {
		label += "  " + metaStyle.Render(strings.Join(extra, " · "))
	}
	return label
}

func snapshotLabel(s api.SnapshotResponse) string {
	return snapNodeStyle.Render("◆ " + s.ID)
}

func indexOfSandbox(sbs []api.SandboxResponse, id string) (int, bool) {
	for i, sb := range sbs {
		if sb.ID == id {
			return i, true
		}
	}
	return 0, false
}
