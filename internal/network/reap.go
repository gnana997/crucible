package network

import (
	"context"
	"log/slog"
)

// ReapOrphans removes any sandbox network state left behind by a
// previous daemon run that didn't exit cleanly. Safe and idempotent
// to call at every daemon startup, regardless of whether networking
// is enabled this run — it only touches objects carrying our prefix
// or comment tag.
//
// Order matters:
//
//  1. Delete each `crucible-*` netns. Deleting a netns also destroys
//     every interface that lived inside it, and the *root-netns* side
//     of each veth pair is destroyed with its in-netns peer — so
//     this single step cleans up every vh-*, vg-*, br-*, and tap0
//     left over.
//  2. Tear down the base nft table (`inet crucible`). This wipes the
//     forward chain, postrouting chain, sandbox dispatch map, and
//     every per-sandbox chain/set inside. TeardownBaseTable also
//     removes the iptables FORWARD ACCEPT rules carrying our comment
//     tag, so the host's FORWARD chain is left as we found it.
//
// All failures are logged and swallowed — a partially reaped host is
// still better than refusing to start. The next EnsureBaseTable call
// will re-create whatever remains, and leftover netns are harmless
// (just pool capacity we didn't reclaim).
func ReapOrphans(ctx context.Context, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "network", "phase", "reap")

	names, err := ListCrucibleNetns(ctx)
	if err != nil {
		log.Warn("list orphan netns failed", "err", err)
	}
	for _, name := range names {
		if err := DeleteNetns(ctx, name); err != nil {
			log.Warn("delete orphan netns failed", "netns", name, "err", err)
			continue
		}
		log.Info("reaped orphan netns", "netns", name)
	}

	if err := TeardownBaseTable(ctx); err != nil {
		log.Warn("teardown base table failed", "err", err)
	}
}
