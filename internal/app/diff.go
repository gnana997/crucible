package app

import (
	"reflect"

	"github.com/gnana997/crucible/internal/ingress"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// updateClass is how a spec change must be applied.
type updateClass int

const (
	// updateNoop: the specs are identical — nothing to do.
	updateNoop updateClass = iota
	// updateInPlace: only host-side fields changed — state the daemon re-reads
	// from the record (idle evaluator, per-connection call authorization, health
	// prober, restart policy, guest-scrape targets) or re-asserts from spec on
	// every reconcile pass (waking forwarders, internal VIPs). Persisting the
	// record applies it; the running instance is untouched.
	updateInPlace
	// updateRedeploy: an instance-defining field changed (fixed at VM boot or
	// read by the guest only at startup), or the change flips how the instance
	// was built — the reconciler must rebuild it.
	updateRedeploy
)

// bootSpec is the subset of AppSpec fixed at instance boot. A change to any of
// these genuinely requires destroy-then-boot (or a rolling redeploy): rootfs,
// vCPU/RAM (Firecracker has no hot-plug), block devices, guest env/entrypoint
// (read only at startup), the egress nftables policy and per-instance published
// ports (both programmed at sandbox create).
type bootSpec struct {
	Image         *api.ImageRef
	Pull          string
	VCPUs         int
	MemoryMiB     int
	DiskBytes     int64
	Volumes       []api.VolumeMount
	Env           map[string]string
	PublishAll    bool
	Network       *api.NetworkRequest
	Service       *wire.ServiceSpec
	SecretEnvFrom []string
}

func bootSpecOf(s api.AppSpec) bootSpec {
	return bootSpec{
		Image: s.Image, Pull: s.Pull,
		VCPUs: s.VCPUs, MemoryMiB: s.MemoryMiB, DiskBytes: s.DiskBytes,
		Volumes: s.Volumes, Env: s.Env,
		PublishAll: s.PublishAll, Network: s.Network,
		Service: s.Service, SecretEnvFrom: s.SecretEnvFrom,
	}
}

// needsNIC mirrors the instantiator's NIC decision: with no egress policy, a
// NIC is synthesized only when the proxy, the waking forwarders, or the
// internal-L4 zone must reach the guest over its veth. A change that flips this
// cannot be applied to a running instance — there is no NIC to (un)use.
func needsNIC(s api.AppSpec) bool {
	return s.Network != nil || s.Port > 0 || ingress.WakesOnTCP(s) || len(s.InternalPorts) > 0
}

// classifyUpdate decides how a spec change is applied: not at all (identical),
// in place (no rebuild — see updateInPlace), or by redeploying the instance.
//
// Classification is not purely per-field: WakesOnTCP decides at boot whether
// the instance binds its own published ports (suppressed when the app-scoped
// waking forwarder owns them), and needsNIC whether it got a NIC at all. A
// change that flips either predicate needs the rebuild even when every changed
// field is otherwise host-side.
func classifyUpdate(old, next api.AppSpec) updateClass {
	if reflect.DeepEqual(old, next) {
		return updateNoop
	}
	if !reflect.DeepEqual(bootSpecOf(old), bootSpecOf(next)) {
		return updateRedeploy
	}
	if ingress.WakesOnTCP(old) != ingress.WakesOnTCP(next) {
		return updateRedeploy
	}
	if needsNIC(old) != needsNIC(next) {
		return updateRedeploy
	}
	// Published ports are bound per instance at sandbox create — except for a
	// wake-on-TCP app (checked same-mode above), whose app-scoped forwarders
	// are diffed and rebound from the record each reconcile pass.
	if !reflect.DeepEqual(old.Publish, next.Publish) && !ingress.WakesOnTCP(next) {
		return updateRedeploy
	}
	return updateInPlace
}

// changedHostFields names the host-side fields that differ, for the update
// event's attrs (a lifecycle-event reader can tell what moved without a spec
// diff of its own).
func changedHostFields(old, next api.AppSpec) []string {
	var fields []string
	diff := func(name string, changed bool) {
		if changed {
			fields = append(fields, name)
		}
	}
	diff("sleep", !reflect.DeepEqual(old.Sleep, next.Sleep))
	diff("can_call", !reflect.DeepEqual(old.CanCall, next.CanCall))
	diff("publish", !reflect.DeepEqual(old.Publish, next.Publish))
	diff("internal_ports", !reflect.DeepEqual(old.InternalPorts, next.InternalPorts))
	diff("health", !reflect.DeepEqual(old.Health, next.Health))
	diff("restart", !reflect.DeepEqual(old.Restart, next.Restart))
	diff("metrics_port", old.MetricsPort != next.MetricsPort || old.MetricsPath != next.MetricsPath)
	diff("port", old.Port != next.Port)
	diff("tls_mode", old.TLSMode != next.TLSMode)
	diff("http_redirect", !reflect.DeepEqual(old.HTTPRedirect, next.HTTPRedirect))
	return fields
}
