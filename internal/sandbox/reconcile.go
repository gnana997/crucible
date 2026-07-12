package sandbox

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
)

// Reconcile re-establishes durable authority over on-disk state after a
// (re)start. Call it exactly once, before serving, and after the network
// and jailer orphan-reaps have run: those kill any leftover VM processes,
// netns, veths, nft state, and jailer chroots from the previous run.
// Reconcile handles the two pieces those reaps don't cover — the
// per-sandbox workdirs under WorkBase and the in-memory registries.
//
// Snapshots are pure on-disk artifacts, so every persisted snapshot whose
// files survived is re-adopted into the registry — forks keep working
// across a restart. Sandboxes cannot be re-attached (their runner handles
// died with the old daemon and the reaps already killed the VMs), so each
// persisted sandbox is treated as an orphan: its workdir is removed and
// its record dropped. Finally the journal is compacted to just the
// surviving snapshots so it can't grow across restarts.
//
// A nil store (persistence disabled, e.g. in unit tests that don't set
// StatePath) makes Reconcile a no-op.
func (m *Manager) Reconcile(ctx context.Context) error {
	_ = ctx // reserved: reaping is local + fast; no cancellation today
	if m.store == nil {
		return nil
	}
	log := slog.Default().With("component", "sandbox", "phase", "reconcile")

	// Re-adopt snapshots whose files still exist; drop (and clean up) the
	// rest. The adopted set doubles as the keep-set for the WorkBase
	// sweep below (a snapshot's dir is named by its ID).
	adopted := make([]snapshotRecord, 0, len(m.loadedSnapshots))
	keep := make(map[string]bool, len(m.loadedSnapshots))
	m.mu.Lock()
	for _, rec := range m.loadedSnapshots {
		if !snapshotFilesPresent(rec) {
			log.Warn("dropping snapshot with missing files", "id", rec.ID, "dir", rec.Dir)
			if rec.Dir != "" {
				_ = os.RemoveAll(rec.Dir)
			}
			continue
		}
		m.snapshots[rec.ID] = m.snapshotFromRecord(rec)
		adopted = append(adopted, rec)
		keep[rec.ID] = true
		log.Info("re-adopted snapshot", "id", rec.ID, "source_id", rec.SourceID)
	}
	m.mu.Unlock()

	for _, rec := range m.loadedSandboxes {
		log.Info("reaping orphaned sandbox from previous run", "id", rec.ID, "workdir", rec.Workdir)
	}

	// Reap orphaned on-disk state under WorkBase. Process/chroot/netns/
	// nft teardown already happened via jailer.ReapOrphans +
	// network.ReapOrphans; the per-entry dirs under WorkBase are the
	// piece those don't reach. We sweep the whole directory rather than
	// only the journaled sandbox workdirs so a crash that left a workdir
	// or snapshot dir on disk *before* its journal record was written is
	// still cleaned up. Everything under WorkBase is crucible-owned and
	// is exactly one of: an adopted snapshot dir (keep), the journal
	// itself (keep), or an orphan (reap) — no live sandbox survives a
	// restart to be re-adopted.
	journalName := filepath.Base(m.cfg.StatePath)
	if entries, err := os.ReadDir(m.cfg.WorkBase); err != nil {
		log.Warn("scan work base for orphans failed", "work_base", m.cfg.WorkBase, "err", err)
	} else {
		for _, e := range entries {
			name := e.Name()
			if keep[name] || name == journalName || name == journalName+".tmp" {
				continue
			}
			if err := os.RemoveAll(filepath.Join(m.cfg.WorkBase, name)); err != nil {
				log.Warn("remove orphan work entry failed", "name", name, "err", err)
				continue
			}
			log.Info("reaped orphan work entry", "name", name)
		}
	}

	m.loadedSandboxes = nil
	m.loadedSnapshots = nil

	if err := m.store.compact(adopted); err != nil {
		log.Warn("compact state journal failed", "err", err)
	}
	log.Info("reconcile complete", "snapshots_adopted", len(adopted))
	return nil
}

// snapshotFilesPresent reports whether every file a snapshot needs to be
// forkable still exists on disk.
func snapshotFilesPresent(r snapshotRecord) bool {
	for _, p := range []string{r.StatePath, r.MemPath, r.RootfsPath} {
		if p == "" {
			return false
		}
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// snapshotFromRecord rebuilds a live Snapshot from its persisted record.
// If the snapshot carried a network allowlist and the Manager was
// configured with a ReloadAllowlist hook, the policy is rebuilt so forks
// taken after a restart re-register the same egress rules; otherwise the
// snapshot is adopted without network intent and networked forks from it
// would need the allowlist re-specified.
func (m *Manager) snapshotFromRecord(r snapshotRecord) *Snapshot {
	snap := &Snapshot{
		ID:            r.ID,
		SourceID:      r.SourceID,
		VCPUs:         r.VCPUs,
		MemoryMiB:     r.MemoryMiB,
		Dir:           r.Dir,
		StatePath:     r.StatePath,
		MemPath:       r.MemPath,
		RootfsPath:    r.RootfsPath,
		StaticNetwork: r.StaticNetwork,
		CreatedAt:     r.CreatedAt,
	}
	// Reconstruct the network intent whenever the source was networked — a
	// default-deny sandbox is networked with ZERO patterns, so keying on the
	// Networked flag (not pattern count) is what lets a slept, proxy-fronted app
	// (empty allowlist) wake from a restart-adopted snapshot with a working NIC.
	if r.Networked && m.cfg.ReloadAllowlist != nil {
		al, err := m.cfg.ReloadAllowlist(r.NetworkPatterns)
		if err != nil {
			slog.Default().Warn("rebuild snapshot allowlist failed; networked forks from it will need the allowlist re-specified",
				"component", "sandbox", "id", r.ID, "err", err)
		} else {
			snap.Network = &NetworkConfig{Allowlist: al, FullEgress: r.FullEgress, CIDRs: parseCIDRs(r.NetworkCIDRs)}
		}
	}
	return snap
}

// parseCIDRs turns persisted CIDR strings back into prefixes, dropping any that
// no longer parse (forward-compatible; a bad entry shouldn't fail re-adoption).
func parseCIDRs(ss []string) []netip.Prefix {
	if len(ss) == 0 {
		return nil
	}
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}
