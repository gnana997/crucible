package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/appevents"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// Health is an instance's probe result.
type Health int

const (
	// HealthUnknown means no probe yet, still in the start period, or the probe
	// type is unsupported (exec probes are a follow-up).
	HealthUnknown Health = iota
	// HealthPassing means the probe succeeded.
	HealthPassing
	// HealthFailing means the probe failed.
	HealthFailing
)

// Instantiator boots, probes, and tears down an app's instance. The daemon
// implements it (over internal/sandbox.Manager); the reconciler depends
// only on this narrow interface, so its self-heal logic is decoupled from
// the VMM machinery and testable with a fake.
type Instantiator interface {
	// Create boots a new instance from spec and returns its instance
	// (sandbox) id.
	Create(ctx context.Context, appID string, spec api.AppSpec) (instanceID string, err error)

	// Exists reports whether the instance is still registered in the
	// Manager (gone = the VM/registration disappeared).
	Exists(instanceID string) bool

	// Probe runs the app's health check against the instance: http, tcp, or
	// exec (a command in the guest, exit 0 = healthy).
	Probe(ctx context.Context, instanceID string, hc api.HealthCheck) Health

	// Destroy tears the instance down. Absent/already-gone is not an error.
	Destroy(ctx context.Context, instanceID string) error

	// Quiesce flushes the instance's guest filesystems (sync) so a subsequent
	// Destroy doesn't lose un-fsync'd writes to a Writeback volume. Best-effort
	// (a gone instance or an old agent is not an error). Used before stop/start
	// sleep and destroy-then-boot redeploy of a volume app.
	Quiesce(ctx context.Context, instanceID string) error

	// Sleep snapshots the instance and stops its VMM to free RAM while KEEPING
	// its record + network, so Wake can restore it in place (scale-to-zero). It
	// returns the id of the durable snapshot captured, which the caller persists
	// so the slept app survives a daemon restart (re-adopted + woken from it).
	Sleep(ctx context.Context, instanceID string) (snapshotID string, err error)

	// Wake restores a slept instance in place — same id, netns, and IP —
	// reseeding its CRNG and stepping its clock.
	Wake(ctx context.Context, instanceID string) error

	// WakeFromSnapshot restores a slept app's durable snapshot into a FRESH
	// instance (used after a daemon restart, when the original in-place instance
	// is gone), returning the new instance id. Identity is preserved (reseed +
	// clock only); the network is fresh (new IP; the proxy resolves by name).
	WakeFromSnapshot(ctx context.Context, snapshotID string, spec api.AppSpec) (instanceID string, err error)

	// SnapshotExists reports whether the durable snapshot still exists (its files
	// survived a restart and it was re-adopted). Used to decide whether a
	// persisted-asleep app can be re-adopted vs. cold-booted.
	SnapshotExists(snapshotID string) bool

	// SnapshotInstance snapshots a RUNNING instance without stopping it, returning
	// the snapshot id — the "golden" template a horizontally-scaled app forks its
	// warm replicas from (v0.5.2). Distinct from Sleep, which stops the VMM.
	SnapshotInstance(ctx context.Context, instanceID string) (snapshotID string, err error)

	// ForkInstance stamps a fresh WARM instance from a golden snapshot (lazy
	// memory, ~O(working set)), with clone-safe identity rotation — a distinct
	// machine-id/hostname/IP from every other replica. The scale-up path that
	// beats a cold boot.
	ForkInstance(ctx context.Context, snapshotID string) (instanceID string, err error)

	// DeleteSnapshot GCs a golden snapshot (on app delete/stop or a generation
	// change). Absent/already-gone is not an error.
	DeleteSnapshot(ctx context.Context, snapshotID string) error

	// ImageHealth returns the health check derived from the image's Docker
	// HEALTHCHECK, or nil if the image declares none (or NONE). Used to seed an
	// app's health when it declares none of its own.
	ImageHealth(ctx context.Context, spec api.AppSpec) (*api.HealthCheck, error)
}

// ActivitySource reports per-app request activity for the idle monitor: the last
// time a request was seen and how many are in flight. ok is false for an app
// never seen (so it is left alone). The ingress activity tracker satisfies it.
type ActivitySource interface {
	Activity(appName string) (last time.Time, inflight int, ok bool)
}

// PortReconciler keeps app-scoped host-port listeners in sync with the apps that
// need them — the L4 waking forwarders that front a scale-to-zero app's published
// port, wake it on connect, and survive its sleep (unlike per-instance publish,
// which dies with the VM). Called on each reconcile pass with the current
// desired-running specs; the implementation diffs and binds/closes as needed.
type PortReconciler interface {
	ReconcilePorts(specs []api.AppSpec)
}

// observed is an app's runtime (never persisted) state, rebuilt from
// scratch on Start: an empty observed map plus persisted desired state is
// what drives re-creation after a daemon restart.
type observed struct {
	instanceID string
	generation uint64    // spec generation this instance was booted from
	bootedAt   time.Time // when the current/last instance booted
	phase      string    // pending | running | unhealthy | crashlooping | stopped | asleep | waking

	// Scale-to-zero bookkeeping, set by the Sleep/Wake path (not the
	// reconciler, which treats asleep/waking as a steady state it must not
	// disturb). While phase is "asleep" the instance's VMM is stopped but the
	// sandbox record + snapshot are kept.
	sleepCount      int
	lastWakeLatency time.Duration

	// Self-heal bookkeeping.
	restarts            int
	consecutiveFailures int       // windowed; drives backoff + crash-loop
	backoffUntil        time.Time // don't (re)boot before this
	lastErr             string

	// Health tracking.
	health          string // healthy | unhealthy | unknown
	healthyStreak   int
	unhealthyStreak int
	nextProbe       time.Time

	// Zero-downtime rolling update. While a roll is in progress the incoming
	// instance is booted and probed WITHOUT being made current, so the old
	// instance keeps serving (phase stays "running", the proxy keeps routing to
	// it) until the incoming passes its readiness gate. Only the intentional
	// `app update` path rolls; self-heal of a dead instance stays a cold boot.
	incomingInstanceID  string // booted from a newer generation, not yet current
	incomingGeneration  uint64
	incomingReadyStreak int       // consecutive readiness passes for the incoming
	rolloutUntil        time.Time // abort the roll (keep old serving) if not ready by here
	rollFailures        int       // consecutive failed rolls, drives rollBackoff
	rollBackoff         time.Time // don't retry a failed roll before this

	// After a flip, the superseded instance is kept alive for drainWindow so
	// in-flight requests and the ~1s of stale proxy routes (resolver TTL) land
	// on a live instance, then it is destroyed.
	drainInstanceID string
	drainUntil      time.Time

	// This struct tracks only the PRIMARY instance and its single-instance state
	// machines (self-heal, rolling update, sleep/wake) — unchanged by v0.5.2.
	// Extra replicas (2..N) live in Manager.extras.
}

// replica is one non-primary ("extra") instance of a horizontally-scaled app.
// Extras add capacity behind the proxy; they don't own the app's transition
// state (rolling update, sleep/wake) — the primary does. Self-heal is by
// replacement: a vanished or stale-generation extra is dropped and re-booted to
// hold replicaTarget.
type replica struct {
	id         string
	generation uint64
	bootedAt   time.Time
	health     string // healthy | unknown (updated by the balancer's traffic-based checks)
}

// goldenSnap is the snapshot of a healthy primary that an app's warm replicas
// are forked from, tagged with the spec generation it was captured at.
type goldenSnap struct {
	snapshotID string
	generation uint64
}

// scaleState is an app's dual-window autoscaling estimator: a fast EWMA of
// request concurrency drives scale-UP (react to bursts), a slow EWMA drives
// scale-DOWN (resist flapping), and a stabilization window blocks a scale-down
// right after a scale-up.
type scaleState struct {
	shortAvg    float64
	longAvg     float64
	target      int
	lastScaleUp time.Time
}

// Autoscaling tuning.
const (
	autoscaleShortAlpha      = 0.6              // fast EWMA weight (scale up)
	autoscaleLongAlpha       = 0.2              // slow EWMA weight (scale down)
	autoscaleDownStabilize   = 60 * time.Second // don't scale down within this of a scale-up
	defaultTargetConcurrency = 50               // in-flight requests per instance
)

// Tuning. Backoff is exponential between restart attempts; a run that
// survives crashLoopWindow resets the failure count; crashLoopThreshold
// consecutive fast failures flip the phase to crashlooping (still retried,
// at the capped backoff — the k8s CrashLoopBackOff shape).
const (
	defaultReconcileInterval = 3 * time.Second
	defaultIdleInterval      = 5 * time.Second  // idle-monitor scan cadence
	defaultUsageInterval     = 60 * time.Second // persistent-usage-metrics accrual/flush cadence
	baseBackoff              = 1 * time.Second
	maxBackoff               = 60 * time.Second
	crashLoopWindow          = 60 * time.Second
	crashLoopThreshold       = 5

	// Rolling update: how long to wait for the incoming instance to pass
	// its readiness gate before aborting the roll (keeping the old instance),
	// and how long to keep the superseded instance alive after a flip so the
	// cutover drops nothing. drainWindow must exceed the resolver's cache TTL
	// (1s) so stale routes still land on a live instance.
	rolloutTimeout = 60 * time.Second
	drainWindow    = 10 * time.Second
)

// Health-check defaults, applied when a HealthCheck leaves a field zero.
const (
	defaultProbeInterval    = 10 * time.Second
	defaultHealthyThreshold = 1
	defaultUnhealthyCount   = 3
	defaultStartPeriod      = 5 * time.Second
)

// Manager is the control-plane engine: it owns the durable app store and a
// reconcile loop that converges actual instances toward desired state,
// self-healing via health probes, restart backoff, and a crash-loop guard.
// Level-triggered and idempotent — a pass is always safe to repeat.
type Manager struct {
	store *Store
	inst  Instantiator
	log   *slog.Logger

	interval     time.Duration
	idleInterval time.Duration
	now          func() time.Time // injectable clock (tests)

	// activity, when set, feeds the idle monitor (auto-sleep of idle
	// scale-to-zero apps). Nil disables auto-sleep — manual sleep still works.
	activity ActivitySource

	// ports, when set, keeps app-scoped host-port listeners in sync with the set
	// of apps that need them (the L4 waking forwarders for scale-to-zero published
	// apps). Called at the end of each reconcile pass with the current
	// desired-running specs. Nil disables app-scoped publishing.
	ports PortReconciler

	// onDomainAdd, when set, is called (best-effort) with a newly attached custom
	// domain so the TLS layer can pre-warm its certificate. Nil disables pre-warm.
	onDomainAdd func(domain string)

	// usage is the persistent usage-metrics ledger (durable per-app counters that
	// survive a daemon restart). Always non-nil; driven by lifecycle hooks + a
	// periodic tick (usageInterval).
	usage         *usageLedger
	usageInterval time.Duration
	// volSize, when set, returns a volume's allocated backing bytes by name — the
	// per-app storage sampled into the usage ledger. Nil ⇒ storage stays 0.
	volSize func(name string) int64
	// egress, when set, returns per-instance cumulative external egress bytes
	// keyed by sandbox (instance) id, read once per usage tick. Nil ⇒ egress
	// stays 0.
	egress func() map[string]uint64
	// names indexes app name → app id for the request hot path (usage attribution
	// keys by id). Maintained on Create/Delete/adopt.
	namesMu sync.RWMutex
	names   map[string]string

	// events is the app lifecycle event stream (activity feed + exact awake
	// boundaries for billing). Always non-nil. lastPhase de-dups phase_changed
	// events: the hot lifecycle methods emit exact transitions and the reconcile
	// pass emits whatever the state machine converged to, both via emitPhase, so
	// no net phase change is missed and none is emitted twice.
	events    *appevents.Store
	phaseMu   sync.Mutex
	lastPhase map[string]string // app id → last-emitted phase

	reconcileMu sync.Mutex
	obsMu       sync.Mutex
	obs         map[string]*observed
	// extras holds an app's non-primary replicas (instances 2..N) for
	// horizontal scale-out (v0.5.2), keyed by app id and guarded by obsMu. Kept
	// SEPARATE from obs because the primary's state machine replaces its
	// *observed wholesale (re-boot, roll, re-adopt) — nesting extras there would
	// orphan them. Empty for a single-instance app, so the primary path is
	// unchanged.
	extras map[string][]*replica
	// golden holds, per app, the snapshot of a healthy primary that extras are
	// forked warm from (v0.5.2). Keyed by app id, guarded by obsMu; re-captured on
	// a generation change and GC'd on delete/stop.
	golden map[string]*goldenSnap
	// scaleTarget is the autoscaler's current desired replica count per app (the
	// dynamic replicaTarget for an autoscaling app); scalers holds its dual-window
	// concurrency state. Both guarded by obsMu.
	scaleTarget map[string]int
	scalers     map[string]*scaleState

	// transitionMu guards transitions; each per-app mutex serializes that app's
	// sleep/wake lifecycle transitions so Sleep and Wake are mutually exclusive
	// (a wake can't interleave with an in-progress sleep, which would observe a
	// half-slept instance). Held across the whole transition, not just the phase
	// flip. Entries are never removed — a *sync.Mutex per app is negligible, and
	// deleting one a concurrent caller may hold would break the guarantee.
	transitionMu sync.Mutex
	transitions  map[string]*sync.Mutex

	trigger chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// BackupStoreTo streams a consistent copy of the app store's bbolt file; see
// Store.BackupTo. Used by the daemon's daemon backup.
func (m *Manager) BackupStoreTo(frame func(size int64) (io.Writer, error)) error {
	return m.store.BackupTo(frame)
}

// NewManager returns a control-plane manager over store, instantiating
// through inst. Call Start to run the loop.
func NewManager(store *Store, inst Instantiator, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		store:         store,
		inst:          inst,
		log:           log,
		interval:      defaultReconcileInterval,
		idleInterval:  defaultIdleInterval,
		usageInterval: defaultUsageInterval,
		now:           time.Now,
		obs:           make(map[string]*observed),
		extras:        make(map[string][]*replica),
		golden:        make(map[string]*goldenSnap),
		scaleTarget:   make(map[string]int),
		scalers:       make(map[string]*scaleState),
		transitions:   make(map[string]*sync.Mutex),
		names:         make(map[string]string),
		lastPhase:     make(map[string]string),
		events:        appevents.New(appevents.DefaultBuffer),
		trigger:       make(chan struct{}, 1),
	}
	// Late-bind the ledger's clock to m.now so a test that replaces m.now still
	// drives usage accrual.
	m.usage = newUsageLedger(store, func() time.Time { return m.now() }, log)
	return m
}

// SetActivitySource wires the request-activity source (the ingress proxy's
// tracker) so the idle monitor can auto-sleep idle scale-to-zero apps. Call
// before Start; nil leaves auto-sleep disabled.
func (m *Manager) SetActivitySource(a ActivitySource) { m.activity = a }

// SetPortReconciler wires the app-scoped host-port publisher (the L4 waking
// forwarders). Call before Start; nil leaves app-scoped publishing disabled.
func (m *Manager) SetPortReconciler(p PortReconciler) { m.ports = p }

// SetVolumeSizer wires the per-volume backing-size source so the usage ledger
// can sample an app's storage. Call before Start; nil leaves storage at 0.
func (m *Manager) SetVolumeSizer(f func(name string) int64) { m.volSize = f }

// SetUsageInterval overrides the persistent-usage-metrics accrual/flush cadence
// (default 60s). Call before Start; a non-positive value keeps the default.
func (m *Manager) SetUsageInterval(d time.Duration) {
	if d > 0 {
		m.usageInterval = d
	}
}

// SetEgressSource wires the per-instance egress-byte source (sandbox id →
// cumulative external egress bytes), read once per usage tick. Call before
// Start; nil leaves egress at 0.
func (m *Manager) SetEgressSource(f func() map[string]uint64) { m.egress = f }

// RecordRequest attributes one ingress-proxy request (by HTTP status class) to
// an app's durable usage counters. Cheap (in-memory bump); a name that maps to
// no live app is dropped. Wired into the proxy's OnRequest callback.
func (m *Manager) RecordRequest(name, code string) {
	if name == "" {
		return
	}
	m.namesMu.RLock()
	id := m.names[name]
	m.namesMu.RUnlock()
	if id == "" {
		return
	}
	m.usage.AddRequest(id, name, code)
}

// Events returns the app lifecycle event stream (for GET /events, OTLP export).
func (m *Manager) Events() *appevents.Store { return m.events }

// emit records a non-phase lifecycle event (created/updated/deleted/domain).
func (m *Manager) emit(typ, appID, name, reason string, attrs map[string]any) {
	m.events.Emit(appevents.AppEvent{
		Type: typ, AppID: appID, App: name, Reason: reason, Attrs: attrs,
	})
}

// emitPhase emits a phase_changed event iff the app's phase actually changed
// since the last emission (de-dup across the hot-path and reconcile callers).
// attrs are merged into {from,to}; instance/name are looked up by the caller.
func (m *Manager) emitPhase(appID, name, instance, to, reason string, attrs map[string]any) {
	m.phaseMu.Lock()
	from := m.lastPhase[appID]
	if from == to {
		m.phaseMu.Unlock()
		return
	}
	m.lastPhase[appID] = to
	m.phaseMu.Unlock()

	a := map[string]any{"from": from, "to": to}
	for k, v := range attrs {
		a[k] = v
	}
	m.events.Emit(appevents.AppEvent{
		Type: appevents.TypePhaseChanged, AppID: appID, App: name,
		Instance: instance, Reason: reason, Attrs: a,
	})
}

// forgetPhase drops an app's phase-dedup state on delete (bounds the map).
func (m *Manager) forgetPhase(appID string) {
	m.phaseMu.Lock()
	delete(m.lastPhase, appID)
	m.phaseMu.Unlock()
}

// emitPhaseChanges is step 4 of a reconcile pass: emit phase_changed for every
// app whose converged phase differs from what was last emitted. This catches the
// reconciler-driven transitions (booted→running, crash, health) centrally, so no
// net phase change is missed without threading emits through every phase write.
func (m *Manager) emitPhaseChanges(recs []Record) {
	type snap struct{ id, name, inst, phase string }
	snaps := make([]snap, 0, len(recs))
	m.obsMu.Lock()
	for _, r := range recs {
		ob := m.obs[r.ID]
		snaps = append(snaps, snap{r.ID, r.Spec.Name, obInstance(ob), phaseOf(ob)})
	}
	m.obsMu.Unlock()
	for _, s := range snaps {
		m.emitPhase(s.id, s.name, s.inst, s.phase, "reconcile", nil)
	}
}

// obInstance returns an observed's current instance id, or "".
func obInstance(ob *observed) string {
	if ob == nil {
		return ""
	}
	return ob.instanceID
}

// registerName / unregisterName maintain the name→id index for RecordRequest.
func (m *Manager) registerName(name, id string) {
	m.namesMu.Lock()
	m.names[name] = id
	m.namesMu.Unlock()
}

func (m *Manager) unregisterName(name string) {
	m.namesMu.Lock()
	delete(m.names, name)
	m.namesMu.Unlock()
}

// usageDims returns the usage inputs for a record: vCPUs and memory drive the
// awake compute/memory integral; volBytes (sparse-allocated backing size of the
// app's volumes) drives the storage integral. volBytes is 0 without a sizer.
func (m *Manager) usageDims(rec Record) (vcpus, memMiB int, volBytes int64) {
	vcpus, memMiB = rec.Spec.VCPUs, rec.Spec.MemoryMiB
	if m.volSize != nil {
		for _, v := range rec.Spec.Volumes {
			volBytes += m.volSize(v.Name)
		}
	}
	return vcpus, memMiB, volBytes
}

// phaseAwake reports whether a phase burns compute — a live VMM (serving or
// unhealthy). Asleep/stopped/pending/crashlooping do not accrue compute.
func phaseAwake(phase string) bool { return phase == "running" || phase == "unhealthy" }

// observeUsage records an app's current usage state (awake compute/memory +
// storage) at a lifecycle boundary. name may be empty (looked up from the spec).
func (m *Manager) observeUsage(rec Record, awake bool) {
	vcpus, memMiB, volBytes := m.usageDims(rec)
	m.usage.observe(rec.ID, rec.Spec.Name, awake, vcpus, memMiB, volBytes)
}

// AllUsage returns every app's persistent usage metrics, accrued to now: each
// live app's current counters plus any RETAINED (finalized) records for apps
// that were deleted. Cumulative — a reader diffs across reads.
func (m *Manager) AllUsage() []api.AppUsage {
	recs, err := m.store.List()
	if err != nil {
		m.log.Warn("usage: list apps failed", "err", err)
	}
	live := make(map[string]struct{}, len(recs))
	out := make([]api.AppUsage, 0, len(recs))
	for _, r := range recs {
		live[r.ID] = struct{}{}
		out = append(out, usageToAPI(m.usage.Snapshot(r.ID, r.Spec.Name)))
	}
	// Retained records for deleted apps (finalized): present in the store but not
	// in the live set. Emit them straight from storage (already accrued at delete).
	stored, err := m.store.ListUsage()
	if err != nil {
		m.log.Warn("usage: list usage failed", "err", err)
	}
	for id, u := range stored {
		if _, isLive := live[id]; !isLive {
			out = append(out, usageToAPI(u))
		}
	}
	return out
}

// AppUsage returns one live app's persistent usage metrics by name, accrued to
// now. A deleted app's retained usage is only available via AllUsage / GET
// /usage (its id is gone, so it can't be resolved by name).
func (m *Manager) AppUsage(name string) (api.AppUsage, error) {
	rec, found, err := m.store.GetByName(name)
	if err != nil {
		return api.AppUsage{}, err
	}
	if !found {
		return api.AppUsage{}, ErrNotFound
	}
	return usageToAPI(m.usage.Snapshot(rec.ID, rec.Spec.Name)), nil
}

// usageToAPI converts the internal integer-sub-unit ledger record to the public
// seconds-based shape (the caller converts seconds→hours for billing).
func usageToAPI(u Usage) api.AppUsage {
	return api.AppUsage{
		AppID:              u.AppID,
		AppName:            u.AppName,
		ComputeVCPUSeconds: float64(u.ComputeVCPUMillis) / 1000,
		MemoryMiBSeconds:   float64(u.MemoryMiBMillis) / 1000,
		StorageGiBSeconds:  float64(u.StorageMiBMillis) / 1000 / 1024,
		Requests:           u.Requests,
		RequestsByCode:     u.RequestsByCode,
		EgressBytes:        u.EgressBytes,
		UpdatedAt:          u.UpdatedAt,
		FinalizedAt:        u.FinalizedAt,
	}
}

// transitionLock returns the per-app mutex that serializes sleep/wake
// transitions for appID, creating it on first use.
func (m *Manager) transitionLock(appID string) *sync.Mutex {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	mu := m.transitions[appID]
	if mu == nil {
		mu = &sync.Mutex{}
		m.transitions[appID] = mu
	}
	return mu
}

// --- domain operations -------------------------------------------------

// ErrNameTaken is returned by Create when an app with that name exists.
var ErrNameTaken = errors.New("app: name already in use")

// ErrNotFound is returned when no app matches the given id.
var ErrNotFound = errors.New("app: not found")

// Create validates and persists a new app, then triggers a reconcile so
// its instance is booted. desiredRunning=false creates it stopped.
func (m *Manager) Create(spec api.AppSpec, desiredRunning bool) (Record, error) {
	if err := validateSpec(spec); err != nil {
		return Record{}, err
	}
	if _, found, err := m.store.GetByName(spec.Name); err != nil {
		return Record{}, err
	} else if found {
		return Record{}, fmt.Errorf("%w: %q", ErrNameTaken, spec.Name)
	}
	id, err := NewID()
	if err != nil {
		return Record{}, err
	}
	now := m.now().UTC()
	rec := Record{
		ID:             id,
		Spec:           spec,
		DesiredRunning: desiredRunning,
		Generation:     1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := m.store.Put(rec); err != nil {
		return Record{}, err
	}
	m.registerName(spec.Name, id)
	m.emit(appevents.TypeCreated, id, spec.Name, "", map[string]any{"generation": rec.Generation})
	m.Trigger()
	return rec, nil
}

// Delete removes an app and triggers a reconcile that tears down its
// instance. Absent id is ErrNotFound.
func (m *Manager) Delete(id string) error {
	rec, found, err := m.store.Get(id)
	if err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	if err := m.store.Delete(id); err != nil {
		return err
	}
	// Final usage accrual, then retain the (finalized) usage record so a control
	// plane can read the deleted app's total usage.
	m.observeUsage(rec, false)
	m.usage.Finalize(id, rec.Spec.Name)
	m.unregisterName(rec.Spec.Name)
	m.emit(appevents.TypeDeleted, id, rec.Spec.Name, "", nil)
	m.forgetPhase(id)
	m.Trigger()
	return nil
}

// SetDesired flips an app between running and stopped (spec retained).
func (m *Manager) SetDesired(id string, running bool) error {
	rec, found, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	rec.DesiredRunning = running
	rec.UpdatedAt = m.now().UTC()
	if err := m.store.Put(rec); err != nil {
		return err
	}
	m.Trigger()
	return nil
}

// Update replaces an app's spec and bumps its generation, which the reconciler
// observes as a redeploy. For a proxy-fronted app (a Port, no fixed host
// publish) the redeploy is a zero-downtime rolling update: a new instance is
// booted and, once it passes its readiness gate, the route flips to it and the
// old instance is drained then destroyed (a failed update keeps the old
// instance serving). Other apps fall back to destroy-then-boot. The app's name
// is immutable and desired running/stopped is retained (use SetDesired to
// change that). Absent name is ErrNotFound.
func (m *Manager) Update(name string, spec api.AppSpec) (Record, error) {
	if spec.Name != name {
		return Record{}, fmt.Errorf("app: name is immutable (%q cannot become %q)", name, spec.Name)
	}
	if err := validateSpec(spec); err != nil {
		return Record{}, err
	}
	rec, found, err := m.store.GetByName(name)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotFound
	}
	// Domains are managed by AddDomain/RemoveDomain, not this full re-spec, so an
	// `app update` that doesn't know about them can't wipe them.
	spec.Domains = rec.Spec.Domains
	rec.Spec = spec
	rec.Generation++
	rec.UpdatedAt = m.now().UTC()
	if err := m.store.Put(rec); err != nil {
		return Record{}, err
	}
	m.emit(appevents.TypeUpdated, rec.ID, rec.Spec.Name, "", map[string]any{"generation": rec.Generation})
	m.Trigger()
	return rec, nil
}

// Get returns the app's desired state plus observed status.
func (m *Manager) Get(id string) (api.AppResponse, error) {
	rec, found, err := m.store.Get(id)
	if err != nil {
		return api.AppResponse{}, err
	}
	if !found {
		return api.AppResponse{}, ErrNotFound
	}
	return m.toResponse(rec), nil
}

// GetByName returns the app with the given name (the user-facing handle).
func (m *Manager) GetByName(name string) (api.AppResponse, error) {
	rec, found, err := m.store.GetByName(name)
	if err != nil {
		return api.AppResponse{}, err
	}
	if !found {
		return api.AppResponse{}, ErrNotFound
	}
	return m.toResponse(rec), nil
}

// SetOnDomainAdd registers a best-effort hook called with each newly attached
// custom domain (the TLS layer pre-warms its cert). Call once at wiring time.
func (m *Manager) SetOnDomainAdd(fn func(domain string)) { m.onDomainAdd = fn }

// GetByDomain returns the app a custom domain is attached to, and whether one
// is. Linear over the app records (fine at single-host scale; the resolver only
// consults it for hosts that aren't <app>.<proxy-domain>).
func (m *Manager) GetByDomain(domain string) (api.AppResponse, bool) {
	recs, err := m.store.List()
	if err != nil {
		return api.AppResponse{}, false
	}
	for _, rec := range recs {
		for _, d := range rec.Spec.Domains {
			if strings.EqualFold(d, domain) {
				return m.toResponse(rec), true
			}
		}
	}
	return api.AppResponse{}, false
}

// AddDomain attaches a custom domain to an app (idempotent), enforcing global
// uniqueness — a domain may belong to at most one app. Fires the pre-warm hook.
func (m *Manager) AddDomain(appName, domain string) (Record, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := validateDomain(domain); err != nil {
		return Record{}, err
	}
	if resp, ok := m.GetByDomain(domain); ok && resp.Name != appName {
		return Record{}, fmt.Errorf("app: domain %q is already attached to app %q", domain, resp.Name)
	}
	rec, found, err := m.store.GetByName(appName)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotFound
	}
	for _, d := range rec.Spec.Domains {
		if d == domain {
			return rec, nil // already attached
		}
	}
	rec.Spec.Domains = append(rec.Spec.Domains, domain)
	rec.UpdatedAt = m.now().UTC()
	if err := m.store.Put(rec); err != nil {
		return Record{}, err
	}
	if m.onDomainAdd != nil {
		m.onDomainAdd(domain)
	}
	m.emit(appevents.TypeDomainAdded, rec.ID, rec.Spec.Name, "", map[string]any{"domain": domain})
	return rec, nil
}

// RemoveDomain detaches a custom domain from an app (idempotent).
func (m *Manager) RemoveDomain(appName, domain string) (Record, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	rec, found, err := m.store.GetByName(appName)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotFound
	}
	out := rec.Spec.Domains[:0]
	for _, d := range rec.Spec.Domains {
		if !strings.EqualFold(d, domain) {
			out = append(out, d)
		}
	}
	rec.Spec.Domains = out
	rec.UpdatedAt = m.now().UTC()
	if err := m.store.Put(rec); err != nil {
		return Record{}, err
	}
	m.emit(appevents.TypeDomainRemoved, rec.ID, rec.Spec.Name, "", map[string]any{"domain": domain})
	return rec, nil
}

// ListDomains returns the custom domains attached to an app.
func (m *Manager) ListDomains(appName string) ([]string, error) {
	rec, found, err := m.store.GetByName(appName)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return rec.Spec.Domains, nil
}

// AllDomains returns every custom domain attached to any app (for the TLS
// layer's cert-expiry metric). Linear over the app records.
func (m *Manager) AllDomains() []string {
	recs, err := m.store.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, rec := range recs {
		out = append(out, rec.Spec.Domains...)
	}
	return out
}

// domainRe is a permissive FQDN check: dot-separated labels of letters, digits,
// and hyphens (not leading/trailing), at least two labels. Rejects wildcards
// (not supported — see the TLS plan) and the empty string.
var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func validateDomain(domain string) error {
	if domain == "" {
		return fmt.Errorf("app: domain is required")
	}
	if !domainRe.MatchString(domain) {
		return fmt.Errorf("app: %q is not a valid domain (FQDN, no wildcards)", domain)
	}
	return nil
}

// CanCall reports whether caller is authorized to reach target over the internal
// zone (app→app networking, v0.5.1). Default-deny: an unknown caller, or one
// whose spec's can_call does not list target, returns false. A call to self is
// always allowed.
func (m *Manager) CanCall(caller, target string) bool {
	if caller == "" || target == "" {
		return false
	}
	if caller == target {
		return true
	}
	rec, found, err := m.store.GetByName(caller)
	if err != nil || !found {
		return false
	}
	for _, t := range rec.Spec.CanCall {
		if t == target {
			return true
		}
	}
	return false
}

// DeleteByName removes the app with the given name.
func (m *Manager) DeleteByName(name string) error {
	rec, found, err := m.store.GetByName(name)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return m.Delete(rec.ID)
}

// ErrNotRunning is returned by Sleep when the app has no running instance to
// snapshot (it is stopped, pending, crash-looping, or already asleep).
var ErrNotRunning = errors.New("app: no running instance to sleep")

// ErrNotAsleep is returned by Wake when the app is not currently asleep.
var ErrNotAsleep = errors.New("app: not asleep")

// Sleep snapshots the app's current instance and stops its VMM to free RAM
// (scale-to-zero), keeping the instance record + reserved network so Wake can
// restore it in place. The app must have a running instance.
//
// The observed phase is flipped to "asleep" BEFORE the VMM stops, so a
// concurrent reconcile pass leaves the instance alone (the reconcile guard
// skips asleep/waking apps) rather than seeing the gone VMM as a crash and
// cold-booting a replacement. A failed sleep reverts the phase to running.
func (m *Manager) Sleep(ctx context.Context, name string) error {
	rec, found, err := m.store.GetByName(name)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}

	// Serialize against a concurrent wake (and other sleeps) for the whole
	// transition, so a wake can't start against a half-slept instance.
	lk := m.transitionLock(rec.ID)
	lk.Lock()
	defer lk.Unlock()

	m.obsMu.Lock()
	ob := m.obs[rec.ID]
	if ob == nil || ob.instanceID == "" || ob.phase != "running" {
		m.obsMu.Unlock()
		return fmt.Errorf("%w (phase %q)", ErrNotRunning, phaseOf(ob))
	}
	instanceID := ob.instanceID
	ob.phase = "asleep" // claim the state before the VMM stops
	// Reap any roll instances in flight — a draining (post-flip) or incoming
	// (mid-roll) instance. Left alone they'd keep a live VM running while the app
	// is "asleep" (defeating scale-to-zero), and the reap in reconcilePrimary is
	// skipped once phase is asleep. On wake the roll re-derives from the spec.
	rollOrphans := []string{ob.drainInstanceID, ob.incomingInstanceID}
	ob.drainInstanceID, ob.drainUntil = "", time.Time{}
	ob.incomingInstanceID, ob.incomingGeneration, ob.incomingReadyStreak = "", 0, 0
	ob.rolloutUntil = time.Time{}
	m.obsMu.Unlock()
	for _, id := range rollOrphans {
		if id != "" {
			_ = m.inst.Destroy(ctx, id)
		}
	}

	revertRunning := func() {
		m.obsMu.Lock()
		if o := m.obs[rec.ID]; o != nil && o.instanceID == instanceID {
			o.phase = "running" // revert: still awake
		}
		m.obsMu.Unlock()
	}

	var snapID string
	// all apps — volume-backed included — snapshot-sleep. The instance keeps
	// its identity + netns/IP, its single-writer volume guard stays HELD (the
	// snapshot path never Destroys), and its volume backing file is host-fsync'd
	// before the VMM stops, so a slept volume app wakes in place (~125 ms) with
	// data durable. (v0.6.1 stop/start-destroyed a volume app; v0.6.2 replaces it.
	// A restore-failure fallback to stop/start is handled below.)
	var serr error
	snapID, serr = m.inst.Sleep(ctx, instanceID)
	if serr != nil {
		revertRunning()
		return fmt.Errorf("app: sleep %q: %w", name, serr)
	}

	// Persist the asleep marker so a daemon restart re-adopts this app as asleep
	// (and wakes it from snapID) rather than cold-booting. Non-fatal: the
	// in-memory phase is already asleep; the worst case on a persist failure is a
	// cold boot after a restart.
	if cur, found, gerr := m.store.Get(rec.ID); gerr == nil && found {
		cur.AsleepSnapshotID = snapID
		cur.UpdatedAt = m.now().UTC()
		if perr := m.store.Put(cur); perr != nil {
			m.log.Warn("persist asleep state failed", "app", rec.ID, "err", perr)
		}
	}

	m.obsMu.Lock()
	if o := m.obs[rec.ID]; o != nil {
		o.sleepCount++
	}
	m.obsMu.Unlock()
	// Freeze compute/memory accrual at the exact sleep boundary (storage keeps
	// accruing — a slept app still occupies its disk).
	m.observeUsage(rec, false)
	m.emitPhase(rec.ID, name, instanceID, "asleep", "sleep", nil)
	m.log.Info("app slept", "app", rec.ID, "name", name, "instance", instanceID, "snapshot", snapID)
	return nil
}

// Wake restores a slept app's instance in place — same id, netns, and IP —
// reseeding its CRNG and stepping its clock. The app must be asleep. The
// observed phase is "waking" while the restore runs (still skipped by the
// reconciler), then "running" with the measured wake latency recorded. A failed
// wake reverts to "asleep" (still slept, retryable).
func (m *Manager) Wake(ctx context.Context, name string) error {
	rec, found, err := m.store.GetByName(name)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}

	// Serialize against a concurrent sleep (and other wakes) for the whole
	// transition — a wake starting mid-sleep would observe a half-slept instance.
	lk := m.transitionLock(rec.ID)
	lk.Lock()
	defer lk.Unlock()

	m.obsMu.Lock()
	ob := m.obs[rec.ID]
	if ob == nil || ob.phase != "asleep" {
		m.obsMu.Unlock()
		return fmt.Errorf("%w (phase %q)", ErrNotAsleep, phaseOf(ob))
	}
	instanceID := ob.instanceID
	ob.phase = "waking"
	m.obsMu.Unlock()

	start := m.now()
	newInstanceID := instanceID
	var werr error
	switch {
	case instanceID != "" && m.inst.Exists(instanceID):
		// Same daemon lifetime (volume-backed or not): the slept instance is still
		// live — restore in place, a ~170 ms snapshot-wake for volume
		// apps too: the instance survived sleep with its single-writer volume guard
		// still held, so WakeInPlace re-attaches the same volume.
		werr = m.inst.Wake(ctx, instanceID)
	case len(rec.Spec.Volumes) > 0 && rec.AsleepSnapshotID != "" && m.inst.SnapshotExists(rec.AsleepSnapshotID):
		// volume app after a daemon restart — fast-wake from the durable
		// snapshot, re-attaching the volume into a fresh instance (~125 ms, no cold
		// boot / recovery). The restored guest resumes with the volume already
		// mounted; WakeFromSnapshot re-acquires the single-writer guard the restart
		// dropped.
		newInstanceID, werr = m.inst.WakeFromSnapshot(ctx, rec.AsleepSnapshotID, rec.Spec)
	case len(rec.Spec.Volumes) > 0:
		// Volume app with no usable durable snapshot (files gone): cold-create a
		// fresh instance and re-attach + mount the volume — the v0.6.1 behavior,
		// correct if not instant.
		newInstanceID, werr = m.inst.Create(ctx, rec.ID, rec.Spec)
	case rec.AsleepSnapshotID != "":
		// Non-volume app after a restart: fork a fresh one from the durable
		// snapshot (new IP; the proxy resolves by name).
		newInstanceID, werr = m.inst.WakeFromSnapshot(ctx, rec.AsleepSnapshotID, rec.Spec)
	default:
		werr = fmt.Errorf("no live instance and no durable snapshot to wake from")
	}

	// a volume app's snapshot-wake (in-place or from-snapshot) must never
	// leave it stuck asleep — a persistently-bad snapshot would otherwise loop.
	// On any restore failure, fall back to the v0.6.1 stop/start cold-create: tear
	// down the slept instance if it's still around (releasing its single-writer
	// volume guard so the fresh Create can re-attach), then cold-create. Slower
	// (a real boot + recovery) but always recovers. Non-volume apps keep the
	// retry-on-failure behavior unchanged.
	if werr != nil && len(rec.Spec.Volumes) > 0 {
		m.log.Warn("app volume snapshot-wake failed; falling back to stop/start cold-create",
			"app", rec.ID, "name", name, "instance", instanceID, "err", werr)
		if instanceID != "" && m.inst.Exists(instanceID) {
			if derr := m.inst.Destroy(ctx, instanceID); derr != nil {
				m.log.Warn("wake fallback: destroy slept instance failed", "app", rec.ID, "err", derr)
			}
		}
		newInstanceID, werr = m.inst.Create(ctx, rec.ID, rec.Spec)
	}

	m.obsMu.Lock()
	if o := m.obs[rec.ID]; o != nil {
		if werr != nil {
			o.phase = "asleep" // revert: still slept, retryable
			o.lastErr = werr.Error()
		} else {
			o.phase = "running"
			o.instanceID = newInstanceID
			o.generation = rec.Generation
			o.health = "healthy" // fresh restore; a configured probe re-evaluates
			o.lastWakeLatency = m.now().Sub(start)
			o.lastErr = ""
		}
	}
	m.obsMu.Unlock()

	if werr != nil {
		m.log.Warn("app wake failed", "app", rec.ID, "name", name, "err", werr)
		return fmt.Errorf("app: wake %q: %w", name, werr)
	}
	instanceID = newInstanceID
	// Clear the persisted asleep marker now that the app is running again.
	if cur, found, gerr := m.store.Get(rec.ID); gerr == nil && found && cur.AsleepSnapshotID != "" {
		cur.AsleepSnapshotID = ""
		cur.UpdatedAt = m.now().UTC()
		if perr := m.store.Put(cur); perr != nil {
			m.log.Warn("clear asleep state failed", "app", rec.ID, "err", perr)
		}
	}
	// Resume compute/memory accrual from the wake boundary (the slept gap accrued
	// no compute — the prior observe left it not-awake).
	m.observeUsage(rec, true)
	m.emitPhase(rec.ID, name, instanceID, "running", "wake",
		map[string]any{"wake_latency_ms": m.now().Sub(start).Milliseconds()})
	m.log.Info("app woke", "app", rec.ID, "name", name, "instance", instanceID)
	return nil
}

// phaseOf returns an observed's phase, or "stopped" when there is no observed
// state yet (the reconciler hasn't acted).
func phaseOf(ob *observed) string {
	if ob == nil || ob.phase == "" {
		return "stopped"
	}
	return ob.phase
}

// List returns every app with observed status.
func (m *Manager) List() ([]api.AppResponse, error) {
	recs, err := m.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]api.AppResponse, 0, len(recs))
	for _, r := range recs {
		out = append(out, m.toResponse(r))
	}
	return out, nil
}

// --- reconcile loop ----------------------------------------------------

// Start runs an initial convergence (the survive-restart step) then the
// background loop. Returns after the first pass.
func (m *Manager) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.loadNames() // seed name→id so requests attribute for adopted apps from t0
	m.reconcile(ctx)
	m.wg.Add(1)
	go m.loop(loopCtx)
	if m.activity != nil {
		m.wg.Add(1)
		go m.idleLoop(loopCtx)
	}
	m.wg.Add(1)
	go m.usageLoop(loopCtx)
	return nil
}

// loadNames seeds the name→id index from the store (adoption after a restart).
func (m *Manager) loadNames() {
	recs, err := m.store.List()
	if err != nil {
		return
	}
	for _, r := range recs {
		m.registerName(r.Spec.Name, r.ID)
	}
}

// usageLoop periodically accrues persistent usage metrics for every app and
// flushes them to the store (the tick that keeps long-running apps' counters
// fresh and bounds crash loss to one interval).
func (m *Manager) usageLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.usageInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.accrueAllUsage() // final flush on shutdown
			return
		case <-t.C:
			m.accrueAllUsage()
		}
	}
}

// accrueAllUsage re-asserts every app's current usage state (awake compute/
// memory + storage), which accrues the elapsed interval and flushes. Awake state
// is snapshotted under obsMu, then the store writes happen without the lock.
func (m *Manager) accrueAllUsage() {
	recs, err := m.store.List()
	if err != nil {
		m.log.Warn("usage tick: list apps failed", "err", err)
		return
	}
	awake := make([]bool, len(recs))
	instID := make([]string, len(recs))
	m.obsMu.Lock()
	for i, r := range recs {
		if ob := m.obs[r.ID]; ob != nil {
			awake[i] = phaseAwake(ob.phase)
			instID[i] = ob.instanceID
		}
	}
	m.obsMu.Unlock()
	// Egress: one snapshot per tick (sandbox id → cumulative bytes), folded in as
	// a per-instance delta. A missing counter is a no-op (preserves the delta).
	var egress map[string]uint64
	if m.egress != nil {
		egress = m.egress()
	}
	for i, r := range recs {
		m.observeUsage(r, awake[i])
		if instID[i] != "" {
			if b, ok := egress[instID[i]]; ok {
				m.usage.AddEgress(r.ID, r.Spec.Name, instID[i], b)
			}
		}
	}
}

// Stop halts the reconcile loop (does not tear down instances; desired
// state stays in the store for the next Start).
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// Trigger requests a reconcile pass (coalesced; never blocks).
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Manager) loop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcile(ctx)
		case <-m.trigger:
			m.reconcile(ctx)
		}
	}
}

// idleLoop scans for idle scale-to-zero apps to auto-sleep. Runs only when an
// ActivitySource is wired.
func (m *Manager) idleLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.idleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.idleCheck(m.now())
			m.autoscaleCheck(m.now())
		}
	}
}

// idleCheck sleeps every scale-to-zero app (sleep.min_scale==0, idle_timeout>0)
// that is running, not unhealthy, has zero in-flight requests/connections, and
// has been idle at least its idle_timeout. Activity comes from whichever front
// serves the app — the L7 ingress proxy (per request) or the L4 waking forwarder
// (per TCP connection); an app fronted by neither has no activity record and is
// left alone.
//
// Known v1 limitation: a request arriving between the zero-in-flight check and
// the snapshot is not drained (it may see a brief 502/reset and retry, which
// wakes the app). Request draining before sleep is a deliberate follow-up.
func (m *Manager) idleCheck(now time.Time) {
	if m.activity == nil {
		return
	}
	recs, err := m.store.List()
	if err != nil {
		return
	}
	for _, rec := range recs {
		sp := rec.Spec.Sleep
		if !rec.DesiredRunning || sp == nil || sp.MinScale != 0 || sp.IdleTimeoutSec <= 0 {
			continue
		}
		m.obsMu.Lock()
		ob := m.obs[rec.ID]
		var phase, health string
		if ob != nil {
			phase, health = ob.phase, ob.health
		}
		m.obsMu.Unlock()
		if phase != "running" || health == "unhealthy" {
			continue
		}
		last, inflight, ok := m.activity.Activity(rec.Spec.Name)
		if !ok || inflight > 0 {
			continue
		}
		if now.Sub(last) < time.Duration(sp.IdleTimeoutSec)*time.Second {
			continue
		}
		if err := m.Sleep(context.Background(), rec.Spec.Name); err != nil {
			m.log.Warn("idle sleep failed", "app", rec.Spec.Name, "err", err)
		} else {
			m.log.Info("app slept (idle)", "app", rec.Spec.Name, "idle_s", sp.IdleTimeoutSec)
		}
	}
}

// reconcile converges actual instances toward desired state. One pass at a
// time (reconcileMu); observed reads elsewhere take obsMu.
func (m *Manager) reconcile(ctx context.Context) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	recs, err := m.store.List()
	if err != nil {
		m.log.Error("app reconcile: list store", "err", err)
		return
	}
	desired := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.DesiredRunning {
			desired[r.ID] = true
		}
	}

	// 1. Tear down instances whose app was deleted or stopped.
	m.obsMu.Lock()
	tracked := make([]string, 0, len(m.obs))
	for id := range m.obs {
		tracked = append(tracked, id)
	}
	m.obsMu.Unlock()
	for _, appID := range tracked {
		if desired[appID] {
			continue
		}
		m.obsMu.Lock()
		ob := m.obs[appID]
		_, stillExists := desiredRec(recs, appID)
		m.obsMu.Unlock()
		// Destroy EVERY instance the app owns — not just the current one. A rolling
		// update leaves a draining (superseded) instance and, mid-roll, an incoming
		// one; both live in single best-effort slots reaped only by reconcilePrimary,
		// which never runs again once the app isn't desired. Missing them here (then
		// dropping `ob` below) orphans those VMs.
		if ob != nil {
			for _, id := range []string{ob.instanceID, ob.drainInstanceID, ob.incomingInstanceID} {
				if id == "" {
					continue
				}
				if err := m.inst.Destroy(ctx, id); err != nil {
					m.log.Warn("app reconcile: destroy", "app", appID, "instance", id, "err", err)
				}
			}
		}
		m.destroyExtras(ctx, appID) // tear down horizontally-scaled replicas too
		m.obsMu.Lock()
		if !stillExists {
			delete(m.obs, appID) // app deleted → forget it
		} else if ob != nil {
			// Stopped (still desired, just not running): clear the roll/drain slots
			// too so nothing dangles or gets re-reaped after the instances are gone.
			ob.phase, ob.instanceID, ob.health = "stopped", "", "unknown"
			ob.drainInstanceID, ob.drainUntil = "", time.Time{}
			ob.incomingInstanceID, ob.incomingGeneration, ob.incomingReadyStreak = "", 0, 0
			ob.rolloutUntil = time.Time{}
		}
		m.obsMu.Unlock()
	}

	// 2. Converge every desired-running app.
	now := m.now()
	for _, rec := range recs {
		if rec.DesiredRunning {
			m.reconcileApp(ctx, rec, now)
		}
	}

	// 3. Keep app-scoped host-port listeners in sync with desired state: the L4
	// waking forwarders front a scale-to-zero app's published port and must exist
	// while it is asleep (DesiredRunning stays true when slept) and vanish when it
	// is deleted or stopped. Diff-based, so an unchanged set is a cheap no-op.
	if m.ports != nil {
		specs := make([]api.AppSpec, 0, len(recs))
		for _, r := range recs {
			if r.DesiredRunning {
				specs = append(specs, r.Spec)
			}
		}
		m.ports.ReconcilePorts(specs)
	}

	// 4. Emit a lifecycle event for any app whose phase converged to something new
	// this pass (boot→running, crash, health flips). Sleep/wake emit their exact
	// transitions directly; this is the catch-all for reconciler-driven ones.
	m.emitPhaseChanges(recs)
}

func desiredRec(recs []Record, id string) (Record, bool) {
	for _, r := range recs {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// reconcileApp converges one desired-running app: the primary instance (its full
// single-instance state machine) plus, for a horizontally-scaled app, the extra
// replicas that hold replicaTarget.
func (m *Manager) reconcileApp(ctx context.Context, rec Record, now time.Time) {
	m.reconcilePrimary(ctx, rec, now)

	// A slept, waking, or stopped app has no extras — a sleep frees the whole
	// app's RAM, so extras are torn down and re-forked on wake (as scale-out
	// arrives). Otherwise converge extras alongside the running primary.
	m.obsMu.Lock()
	phase := ""
	if ob := m.obs[rec.ID]; ob != nil {
		phase = ob.phase
	}
	m.obsMu.Unlock()
	if phase == "asleep" || phase == "waking" || phase == "stopped" {
		m.destroyExtras(ctx, rec.ID)
		return
	}
	m.reconcileExtras(ctx, rec, now)
}

// replicaTarget is the desired number of instances for an app (primary + extras).
// For an autoscaling app it's the autoscaler's current target (falling back to
// the running floor before the first tick); otherwise the static floor from
// min_scale.
func (m *Manager) replicaTarget(rec Record) int {
	if !rec.DesiredRunning {
		return 0
	}
	sp := rec.Spec.Sleep
	if autoscaleEnabled(sp) {
		m.obsMu.Lock()
		t, ok := m.scaleTarget[rec.ID]
		m.obsMu.Unlock()
		if ok && t > 0 {
			return t
		}
		return effectiveMin(sp)
	}
	if sp != nil && sp.MinScale > 1 {
		return sp.MinScale
	}
	return 1
}

// autoscaleEnabled reports whether an app autoscales horizontally: max_scale
// must leave room above the running floor.
func autoscaleEnabled(sp *api.SleepPolicy) bool {
	return sp != nil && sp.MaxScale > effectiveMin(sp)
}

// effectiveMin is the floor a RUNNING app holds: at least 1 (min_scale 0 drops
// to 0 via the sleep path, not the replica floor).
func effectiveMin(sp *api.SleepPolicy) int {
	if sp != nil && sp.MinScale > 1 {
		return sp.MinScale
	}
	return 1
}

func targetConcurrency(sp *api.SleepPolicy) int {
	if sp != nil && sp.TargetConcurrency > 0 {
		return sp.TargetConcurrency
	}
	return defaultTargetConcurrency
}

func ewma(prev, sample, alpha float64) float64 { return alpha*sample + (1-alpha)*prev }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// autoscaleCheck recomputes each autoscaling app's desired replica count from
// observed concurrency — a fast EWMA scales up on bursts, a slow EWMA scales
// down when calm (after a stabilization window) — and nudges the reconciler on a
// change. Runs on the idle-monitor tick.
func (m *Manager) autoscaleCheck(now time.Time) {
	if m.activity == nil {
		return
	}
	recs, err := m.store.List()
	if err != nil {
		return
	}
	for _, rec := range recs {
		sp := rec.Spec.Sleep
		if !rec.DesiredRunning || !autoscaleEnabled(sp) {
			continue
		}
		m.obsMu.Lock()
		phase := ""
		if ob := m.obs[rec.ID]; ob != nil {
			phase = ob.phase
		}
		st := m.scalers[rec.ID]
		if st == nil {
			st = &scaleState{target: effectiveMin(sp)}
			m.scalers[rec.ID] = st
			m.scaleTarget[rec.ID] = st.target
		}
		prev := m.scaleTarget[rec.ID]
		m.obsMu.Unlock()

		// Only steer a running app; asleep/waking/pending don't autoscale (the
		// wake path handles 0→1).
		if phase != "running" {
			continue
		}
		_, inflight, ok := m.activity.Activity(rec.Spec.Name)
		if !ok {
			inflight = 0
		}
		c := float64(inflight)
		st.shortAvg = ewma(st.shortAvg, c, autoscaleShortAlpha)
		st.longAvg = ewma(st.longAvg, c, autoscaleLongAlpha)

		tc := float64(targetConcurrency(sp))
		lo, hi := effectiveMin(sp), sp.MaxScale
		up := clampInt(int(math.Ceil(st.shortAvg/tc)), lo, hi)
		down := clampInt(int(math.Ceil(st.longAvg/tc)), lo, hi)

		target := st.target
		if target < lo {
			target = lo
		}
		switch {
		case up > target:
			target, st.lastScaleUp = up, now // burst → scale up now
		case down < target && now.Sub(st.lastScaleUp) >= autoscaleDownStabilize:
			target = down // calm → scale down after stabilization
		}
		st.target = target

		if target != prev {
			m.obsMu.Lock()
			m.scaleTarget[rec.ID] = target
			m.obsMu.Unlock()
			m.log.Info("app autoscaled", "app", rec.ID, "name", rec.Spec.Name, "replicas", target)
			m.Trigger()
		}
	}
}

// reconcileExtras converges an app's extra replicas (instances 2..N) to
// replicaTarget-1: it drops vanished or stale-generation extras, destroys any
// surplus, and cold-boots the deficit. Warm forking from a golden snapshot and
// per-replica health-based replacement land with later slices; here an extra
// self-heals by replacement when its instance disappears.
func (m *Manager) reconcileExtras(ctx context.Context, rec Record, now time.Time) {
	m.obsMu.Lock()
	ob := m.obs[rec.ID]
	extras := m.extras[rec.ID]
	m.obsMu.Unlock()
	if ob == nil || ob.instanceID == "" {
		return // primary not up yet; extras converge on a later pass
	}
	target := m.replicaTarget(rec) - 1
	if target < 0 {
		target = 0
	}

	// Keep the extras still alive at the current generation; destroy the rest.
	alive := make([]*replica, 0, len(extras))
	for _, r := range extras {
		if r.generation != rec.Generation {
			_ = m.inst.Destroy(ctx, r.id) // stale spec → replace at current generation
			continue
		}
		if !m.inst.Exists(r.id) {
			continue // vanished → drop; re-booted below if under target
		}
		alive = append(alive, r)
	}
	// Destroy surplus above target (a scale-down or a lowered min_scale).
	for len(alive) > target {
		last := alive[len(alive)-1]
		_ = m.inst.Destroy(ctx, last.id)
		alive = alive[:len(alive)-1]
	}
	// Boot the deficit up to target — warm-forked from the golden snapshot when
	// available, else a cold boot.
	for len(alive) < target {
		id, err := m.bootExtra(ctx, rec)
		if err != nil {
			m.log.Error("app: boot extra replica", "app", rec.ID, "name", rec.Spec.Name, "err", err)
			break // retry next pass
		}
		health := "healthy"
		if hc := rec.Spec.Health; hc != nil && hc.Type != "" {
			health = "unknown"
		}
		alive = append(alive, &replica{id: id, generation: rec.Generation, bootedAt: now, health: health})
	}

	m.obsMu.Lock()
	m.extras[rec.ID] = alive
	m.obsMu.Unlock()
}

// bootExtra brings up one extra replica: fork it WARM from the app's golden
// snapshot (~O(working set), clone-safe identity) when one is available for the
// current generation, otherwise cold-boot. A warm-fork error falls back to a
// cold boot rather than blocking the scale-up.
func (m *Manager) bootExtra(ctx context.Context, rec Record) (string, error) {
	if snapID, ok := m.goldenFor(ctx, rec); ok {
		id, err := m.inst.ForkInstance(ctx, snapID)
		if err == nil {
			m.log.Info("app extra replica forked (warm)", "app", rec.ID, "name", rec.Spec.Name, "instance", id, "snapshot", snapID)
			return id, nil
		}
		m.log.Warn("app: warm fork failed, cold-booting extra", "app", rec.ID, "name", rec.Spec.Name, "err", err)
	}
	id, err := m.inst.Create(ctx, rec.ID, rec.Spec)
	if err == nil {
		m.log.Info("app extra replica booted (cold)", "app", rec.ID, "name", rec.Spec.Name, "instance", id)
	}
	return id, err
}

// goldenFor returns the app's golden snapshot for the current generation,
// capturing a fresh one from the healthy primary (and GC'ing a stale one) when
// needed. Returns false when there's no healthy primary to snapshot yet, or the
// capture failed — the caller then cold-boots.
func (m *Manager) goldenFor(ctx context.Context, rec Record) (string, bool) {
	m.obsMu.Lock()
	g := m.golden[rec.ID]
	var primary, health string
	if ob := m.obs[rec.ID]; ob != nil {
		primary, health = ob.instanceID, ob.health
	}
	m.obsMu.Unlock()

	if g != nil && g.generation == rec.Generation {
		return g.snapshotID, true
	}
	// Stale (superseded generation) or missing: only capture from a healthy
	// primary, so replicas fork from good state.
	if primary == "" || health == "unhealthy" {
		return "", false
	}
	snapID, err := m.inst.SnapshotInstance(ctx, primary)
	if err != nil {
		m.log.Warn("app: capture golden snapshot failed", "app", rec.ID, "name", rec.Spec.Name, "err", err)
		return "", false
	}
	if g != nil {
		_ = m.inst.DeleteSnapshot(ctx, g.snapshotID) // GC the superseded template
	}
	m.obsMu.Lock()
	m.golden[rec.ID] = &goldenSnap{snapshotID: snapID, generation: rec.Generation}
	m.obsMu.Unlock()
	m.log.Info("app: captured golden snapshot for scale-out", "app", rec.ID, "name", rec.Spec.Name, "snapshot", snapID, "generation", rec.Generation)
	return snapID, true
}

// destroyExtras tears down and forgets an app's extra replicas (on sleep, stop,
// or delete).
func (m *Manager) destroyExtras(ctx context.Context, appID string) {
	m.obsMu.Lock()
	extras := m.extras[appID]
	delete(m.extras, appID)
	g := m.golden[appID]
	delete(m.golden, appID)
	delete(m.scaleTarget, appID)
	delete(m.scalers, appID)
	m.obsMu.Unlock()
	for _, r := range extras {
		_ = m.inst.Destroy(ctx, r.id)
	}
	if g != nil {
		_ = m.inst.DeleteSnapshot(ctx, g.snapshotID) // GC the golden template
	}
}

// reconcilePrimary converges an app's primary instance: (re)boot subject to
// backoff, detect death via Exists, and — when a health check is
// configured — probe and restart on sustained failure.
func (m *Manager) reconcilePrimary(ctx context.Context, rec Record, now time.Time) {
	m.obsMu.Lock()
	ob := m.obs[rec.ID]
	m.obsMu.Unlock()

	// Re-adopt a persisted-asleep app after a daemon restart (obs was rebuilt
	// empty): if its durable snapshot survived, register it as asleep so the next
	// request wakes it (fork-from-snapshot) instead of cold-booting — the free
	// durability that beats today's "no running VM survives a restart". If the
	// snapshot is gone, clear the stale marker and fall through to a cold boot.
	if ob == nil && rec.AsleepSnapshotID != "" {
		if m.inst.SnapshotExists(rec.AsleepSnapshotID) {
			m.obsMu.Lock()
			m.obs[rec.ID] = &observed{phase: "asleep", generation: rec.Generation, health: "healthy"}
			m.obsMu.Unlock()
			m.log.Info("re-adopted asleep app", "app", rec.ID, "name", rec.Spec.Name, "snapshot", rec.AsleepSnapshotID)
			return
		}
		if cur, found, gerr := m.store.Get(rec.ID); gerr == nil && found {
			cur.AsleepSnapshotID = ""
			cur.UpdatedAt = m.now().UTC()
			_ = m.store.Put(cur)
		}
		m.log.Warn("asleep app snapshot missing; cold-booting", "app", rec.ID, "name", rec.Spec.Name)
	}

	// A slept (or mid-wake) app is a steady desired state: its instance is
	// intentionally gone (snapshotted, VMM stopped) or being restored. The
	// reconciler must not boot over it, probe it, or count it as a failure —
	// only an explicit Wake transitions it back to running. This guard has to
	// come before the boot/redeploy logic below, which would otherwise treat
	// the missing instance as a crash and cold-boot a replacement.
	if ob != nil && (ob.phase == "asleep" || ob.phase == "waking") {
		return
	}

	// Reap a superseded (draining) instance once its drain window has elapsed.
	if ob != nil && ob.drainInstanceID != "" && !now.Before(ob.drainUntil) {
		_ = m.inst.Destroy(ctx, ob.drainInstanceID)
		m.obsMu.Lock()
		ob.drainInstanceID, ob.drainUntil = "", time.Time{}
		m.obsMu.Unlock()
	}

	// A rolling update in progress: advance it (probe the incoming, then flip or
	// abort) and stop — the old instance keeps serving until the roll resolves.
	if ob != nil && ob.incomingInstanceID != "" {
		m.advanceRoll(ctx, rec, ob, now)
		return
	}

	// Spec change → redeploy.
	if ob != nil && ob.instanceID != "" && ob.generation != rec.Generation {
		if canRoll(rec) {
			if now.Before(ob.rollBackoff) {
				return // a recent roll failed; keep the old instance serving
			}
			m.startRoll(ctx, rec, ob, now)
			return
		}
		// Not a roll-eligible app (nothing to keep warm, a host publish a second
		// instance can't co-bind, or a single-writer volume): classic
		// destroy-then-boot. For a volume app, sync first so un-fsync'd writes
		// reach the Writeback volume before the stop.
		if len(rec.Spec.Volumes) > 0 {
			_ = m.inst.Quiesce(ctx, ob.instanceID)
		}
		_ = m.inst.Destroy(ctx, ob.instanceID)
		ob = nil
	}

	// No live instance: boot when backoff has elapsed.
	if ob == nil || ob.instanceID == "" {
		if ob != nil && now.Before(ob.backoffUntil) {
			return
		}
		m.bootInstance(ctx, rec, ob, now)
		return
	}

	// Instance vanished (VM/registration gone).
	if !m.inst.Exists(ob.instanceID) {
		if rec.Spec.Restart.Policy == wire.RestartNever {
			m.setStopped(rec.ID)
			return
		}
		m.recordFailure(rec.ID, "instance exited", now)
		return
	}

	// Instance is registered. Health, if configured, is the liveness signal.
	if hc := rec.Spec.Health; hc != nil && hc.Type != "" {
		if now.Before(ob.nextProbe) {
			m.maybeResetStable(rec.ID, now)
			return
		}
		res := m.inst.Probe(ctx, ob.instanceID, *hc)
		m.applyProbe(ctx, rec, res, now)
		return
	}

	// No health check: alive-and-registered is healthy.
	m.obsMu.Lock()
	ob.health, ob.phase = "healthy", "running"
	m.obsMu.Unlock()
	m.maybeResetStable(rec.ID, now)
}

// seedHealthFromImage defaults an app's health from the image's Docker
// HEALTHCHECK when the app declares none, persisting it once (a defaulting
// write — generation unchanged). Called at boot, where the image is resolvable;
// a no-HEALTHCHECK image just leaves health nil (process-alive liveness).
func (m *Manager) seedHealthFromImage(ctx context.Context, rec Record) Record {
	if rec.Spec.Health != nil {
		return rec
	}
	hc, err := m.inst.ImageHealth(ctx, rec.Spec)
	if err != nil {
		m.log.Warn("app: resolve image health", "app", rec.ID, "err", err)
		return rec
	}
	if hc == nil {
		return rec
	}
	rec.Spec.Health = hc
	if err := m.store.Put(rec); err != nil {
		m.log.Warn("app: persist seeded health", "app", rec.ID, "err", err)
		return rec // boot with it in-memory even if the persist failed
	}
	m.log.Info("app: seeded health from image HEALTHCHECK", "app", rec.ID, "name", rec.Spec.Name)
	return rec
}

// bootInstance creates a fresh instance and records observed state.
func (m *Manager) bootInstance(ctx context.Context, rec Record, prev *observed, now time.Time) {
	rec = m.seedHealthFromImage(ctx, rec)
	id, err := m.inst.Create(ctx, rec.ID, rec.Spec)
	next := &observed{generation: rec.Generation, bootedAt: now}
	if prev != nil {
		next.restarts = prev.restarts
		next.consecutiveFailures = prev.consecutiveFailures
	}
	if err != nil {
		next.phase, next.lastErr = "pending", err.Error()
		next.consecutiveFailures++
		next.backoffUntil = now.Add(backoffFor(next.consecutiveFailures))
		if next.consecutiveFailures >= crashLoopThreshold {
			next.phase = "crashlooping"
		}
		m.log.Error("app: boot instance", "app", rec.ID, "name", rec.Spec.Name, "err", err, "failures", next.consecutiveFailures)
		m.obsMu.Lock()
		m.obs[rec.ID] = next
		m.obsMu.Unlock()
		return
	}
	next.instanceID, next.phase = id, "running"
	if rec.Spec.Health != nil && rec.Spec.Health.Type != "" {
		next.health = "unknown"
		next.nextProbe = now.Add(probeInterval(rec.Spec.Health))
	} else {
		next.health = "healthy"
	}
	m.log.Info("app instance booted", "app", rec.ID, "name", rec.Spec.Name, "instance", id, "restarts", next.restarts)
	m.obsMu.Lock()
	m.obs[rec.ID] = next
	m.obsMu.Unlock()
}

// canRoll reports whether an app can be updated with zero downtime. That needs
// two instances alive at once, which only works for a proxy-fronted app: it
// must have a Port (the proxy's routing target, and the TCP readiness gate when
// no health check is set) and NO fixed host publish — a published host port
// can't be bound by two instances simultaneously.
func canRoll(rec Record) bool {
	// A volume app is single-writer: the flip's incoming instance and the
	// current one would both mount one ext4 device = corruption (and the volume
	// guard would refuse the incoming attach anyway). So a volume app — even
	// proxy-fronted — redeploys via destroy-then-boot (a brief blip), never the
	// zero-downtime flip.
	return rec.Spec.Port > 0 && len(rec.Spec.Publish) == 0 && !rec.Spec.PublishAll &&
		len(rec.Spec.Volumes) == 0
}

// startRoll begins a rolling update: boot the new generation's instance WITHOUT
// making it current, so the old instance keeps serving until the incoming
// passes its readiness gate (advanceRoll). A boot failure leaves the old
// instance serving and backs off before retrying.
func (m *Manager) startRoll(ctx context.Context, rec Record, ob *observed, now time.Time) {
	rec = m.seedHealthFromImage(ctx, rec)
	id, err := m.inst.Create(ctx, rec.ID, rec.Spec)
	if err != nil {
		m.obsMu.Lock()
		ob.rollFailures++
		ob.rollBackoff = now.Add(backoffFor(ob.rollFailures))
		ob.lastErr = fmt.Sprintf("update to generation %d failed: boot: %v (serving generation %d)", rec.Generation, err, ob.generation)
		m.obsMu.Unlock()
		m.log.Error("app: rolling update boot failed; keeping current instance",
			"app", rec.ID, "name", rec.Spec.Name, "err", err)
		return
	}
	m.obsMu.Lock()
	ob.incomingInstanceID = id
	ob.incomingGeneration = rec.Generation
	ob.incomingReadyStreak = 0
	ob.rolloutUntil = now.Add(rolloutTimeout)
	m.obsMu.Unlock()
	m.log.Info("app: rolling update started",
		"app", rec.ID, "name", rec.Spec.Name, "incoming", id, "generation", rec.Generation)
}

// advanceRoll advances an in-progress rolling update: it aborts a stale/failed
// roll, promotes the incoming early if the current instance died mid-roll,
// flips once the incoming passes its readiness gate, or aborts on the rollout
// deadline. The old instance keeps serving throughout.
func (m *Manager) advanceRoll(ctx context.Context, rec Record, ob *observed, now time.Time) {
	// A newer spec arrived mid-roll: abandon this now-stale roll (no failure,
	// no backoff) so the next pass starts a fresh one for the current generation.
	if ob.incomingGeneration != rec.Generation {
		m.cancelRoll(ctx, ob, "superseded by a newer update")
		return
	}
	// Incoming vanished (failed to stay up) → abort, keep the old instance.
	if !m.inst.Exists(ob.incomingInstanceID) {
		m.abortRoll(ctx, rec, ob, now, "incoming instance exited")
		return
	}
	// Current (still-serving) instance died mid-roll → availability wins:
	// promote the incoming now, nothing to drain (the old is already gone).
	if ob.instanceID != "" && !m.inst.Exists(ob.instanceID) {
		m.log.Warn("app: current instance died mid-roll; promoting incoming",
			"app", rec.ID, "name", rec.Spec.Name, "incoming", ob.incomingInstanceID)
		m.flip(ctx, rec, ob, now, false)
		return
	}
	// Readiness gate: the incoming's health check, or a TCP connect to its port.
	res := m.probeReady(ctx, rec, ob.incomingInstanceID)
	var ready bool
	m.obsMu.Lock()
	if res == HealthPassing {
		ob.incomingReadyStreak++
		ready = ob.incomingReadyStreak >= readyThreshold(rec)
	} else {
		ob.incomingReadyStreak = 0
	}
	m.obsMu.Unlock()
	if ready {
		m.log.Info("app: rolling update ready; flipping route to new instance",
			"app", rec.ID, "name", rec.Spec.Name, "instance", ob.incomingInstanceID, "generation", ob.incomingGeneration)
		m.flip(ctx, rec, ob, now, true)
		return
	}
	// Not ready yet: abort if the rollout deadline has passed.
	if !now.Before(ob.rolloutUntil) {
		m.abortRoll(ctx, rec, ob, now, "rollout timed out")
	}
}

// flip makes the incoming instance current. When drain is true the superseded
// instance is kept alive for drainWindow (then reaped) so the cutover drops
// nothing; when false the old instance is already gone. Resets crash-loop and
// health state for the fresh instance and clears roll bookkeeping.
func (m *Manager) flip(ctx context.Context, rec Record, ob *observed, now time.Time, drain bool) {
	m.obsMu.Lock()
	// The drain slot holds at most one instance. If a prior roll's drained
	// instance is still here — a second update flipped inside drainWindow, before
	// the time-gated reap fired — overwriting the slot would orphan its VM. Capture
	// it and destroy it below (outside the lock, matching the reap in reconcilePrimary).
	stale := ""
	if drain && ob.instanceID != "" {
		stale = ob.drainInstanceID
		ob.drainInstanceID = ob.instanceID
		ob.drainUntil = now.Add(drainWindow)
	}
	ob.instanceID = ob.incomingInstanceID
	ob.generation = ob.incomingGeneration
	ob.bootedAt = now
	ob.phase = "running"
	ob.consecutiveFailures = 0
	ob.backoffUntil = time.Time{}
	ob.lastErr = ""
	if rec.Spec.Health != nil && rec.Spec.Health.Type != "" {
		ob.health = "unknown"
		ob.healthyStreak, ob.unhealthyStreak = 0, 0
		ob.nextProbe = now.Add(probeInterval(rec.Spec.Health))
	} else {
		ob.health = "healthy"
	}
	ob.incomingInstanceID, ob.incomingGeneration, ob.incomingReadyStreak = "", 0, 0
	ob.rolloutUntil = time.Time{}
	ob.rollFailures, ob.rollBackoff = 0, time.Time{}
	m.obsMu.Unlock()
	if stale != "" {
		m.log.Info("app: destroying prior draining instance superseded by a new update",
			"app", rec.ID, "name", rec.Spec.Name, "instance", stale)
		_ = m.inst.Destroy(ctx, stale)
	}
}

// abortRoll ends a failed rolling update: the incoming instance is destroyed
// and the old instance keeps serving. The failure is recorded (surfaced via
// AppStatus.LastError) and backed off; the served generation stays behind the
// desired one, so a later `app update` — or the backoff-gated retry — tries
// again without ever taking the app down.
func (m *Manager) abortRoll(ctx context.Context, rec Record, ob *observed, now time.Time, reason string) {
	m.obsMu.Lock()
	incoming := ob.incomingInstanceID
	ob.incomingInstanceID, ob.incomingGeneration, ob.incomingReadyStreak = "", 0, 0
	ob.rolloutUntil = time.Time{}
	ob.rollFailures++
	ob.rollBackoff = now.Add(backoffFor(ob.rollFailures))
	ob.lastErr = fmt.Sprintf("update to generation %d failed: %s (serving generation %d)", rec.Generation, reason, ob.generation)
	m.obsMu.Unlock()
	_ = m.inst.Destroy(ctx, incoming)
	m.log.Warn("app: rolling update aborted; keeping current instance",
		"app", rec.ID, "name", rec.Spec.Name, "incoming", incoming, "reason", reason, "serving_generation", ob.generation)
}

// cancelRoll abandons an in-progress roll that a newer update superseded. Unlike
// abortRoll this is not a failure: no backoff, no recorded error.
func (m *Manager) cancelRoll(ctx context.Context, ob *observed, reason string) {
	m.obsMu.Lock()
	incoming := ob.incomingInstanceID
	ob.incomingInstanceID, ob.incomingGeneration, ob.incomingReadyStreak = "", 0, 0
	ob.rolloutUntil = time.Time{}
	m.obsMu.Unlock()
	_ = m.inst.Destroy(ctx, incoming)
	m.log.Info("app: rolling update cancelled", "instance", incoming, "reason", reason)
}

// probeReady runs the incoming instance's readiness gate: its configured health
// check, or — when none is set — a TCP connect to the app's proxy port (canRoll
// guarantees Port > 0).
func (m *Manager) probeReady(ctx context.Context, rec Record, instanceID string) Health {
	if hc := rec.Spec.Health; hc != nil && hc.Type != "" {
		return m.inst.Probe(ctx, instanceID, *hc)
	}
	return m.inst.Probe(ctx, instanceID, api.HealthCheck{Type: "tcp", Port: rec.Spec.Port})
}

// readyThreshold is how many consecutive readiness passes the incoming instance
// needs before the flip: the health check's healthy threshold, or one
// successful TCP connect.
func readyThreshold(rec Record) int {
	if hc := rec.Spec.Health; hc != nil && hc.Type != "" {
		return healthyThreshold(hc)
	}
	return 1
}

// recordFailure marks the current instance dead and schedules a backed-off
// reboot, tracking crash-loop state.
func (m *Manager) recordFailure(appID, reason string, now time.Time) {
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	ob := m.obs[appID]
	if ob == nil {
		return
	}
	// A run that survived the crash-loop window is a fresh, isolated
	// failure; a fast one compounds toward the crash-loop guard.
	if now.Sub(ob.bootedAt) < crashLoopWindow {
		ob.consecutiveFailures++
	} else {
		ob.consecutiveFailures = 1
	}
	ob.restarts++
	ob.instanceID, ob.health, ob.lastErr = "", "unhealthy", reason
	ob.backoffUntil = now.Add(backoffFor(ob.consecutiveFailures))
	if ob.consecutiveFailures >= crashLoopThreshold {
		ob.phase = "crashlooping"
	} else {
		ob.phase = "pending"
	}
	m.log.Warn("app instance failed", "app", appID, "reason", reason, "failures", ob.consecutiveFailures, "phase", ob.phase)
}

// applyProbe folds a probe result into health streaks and restarts the
// instance when it is unhealthy past the threshold.
func (m *Manager) applyProbe(ctx context.Context, rec Record, res Health, now time.Time) {
	m.obsMu.Lock()
	ob := m.obs[rec.ID]
	if ob == nil {
		m.obsMu.Unlock()
		return
	}
	hc := rec.Spec.Health
	ob.nextProbe = now.Add(probeInterval(hc))
	inStartPeriod := now.Sub(ob.bootedAt) < startPeriod(hc)
	var restart bool
	instanceID := ob.instanceID
	switch res {
	case HealthPassing:
		ob.healthyStreak++
		ob.unhealthyStreak = 0
		if ob.healthyStreak >= healthyThreshold(hc) {
			ob.health, ob.phase = "healthy", "running"
		}
	case HealthFailing:
		if inStartPeriod {
			ob.health = "unknown" // grace: slow starters aren't failures yet
			break
		}
		ob.unhealthyStreak++
		ob.healthyStreak = 0
		ob.health = "unhealthy"
		if ob.unhealthyStreak >= unhealthyThreshold(hc) {
			restart = true
		}
	default:
		ob.health = "unknown"
	}
	m.obsMu.Unlock()

	if restart {
		if rec.Spec.Restart.Policy == wire.RestartNever {
			m.setStopped(rec.ID)
			return
		}
		_ = m.inst.Destroy(ctx, instanceID)
		m.recordFailure(rec.ID, "health check failing", now)
	} else {
		m.maybeResetStable(rec.ID, now)
	}
}

// maybeResetStable clears crash-loop state once an instance has run past
// the window (a one-off crash later doesn't count as a loop).
func (m *Manager) maybeResetStable(appID string, now time.Time) {
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	ob := m.obs[appID]
	if ob == nil || ob.instanceID == "" {
		return
	}
	if now.Sub(ob.bootedAt) >= crashLoopWindow {
		ob.consecutiveFailures = 0
		if ob.phase == "crashlooping" || ob.phase == "pending" {
			ob.phase = "running"
		}
	}
}

func (m *Manager) setStopped(appID string) {
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	if ob := m.obs[appID]; ob != nil {
		ob.phase, ob.instanceID, ob.health = "stopped", "", "unknown"
	}
}

// backoffFor returns the delay before the nth consecutive-failure reboot:
// exponential from baseBackoff, capped at maxBackoff.
func backoffFor(failures int) time.Duration {
	if failures <= 1 {
		return baseBackoff
	}
	d := baseBackoff << (failures - 1)
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

// Health-check field resolution with defaults.
func probeInterval(hc *api.HealthCheck) time.Duration {
	if hc != nil && hc.IntervalSec > 0 {
		return time.Duration(hc.IntervalSec) * time.Second
	}
	return defaultProbeInterval
}
func startPeriod(hc *api.HealthCheck) time.Duration {
	if hc != nil && hc.StartPeriodSec > 0 {
		return time.Duration(hc.StartPeriodSec) * time.Second
	}
	return defaultStartPeriod
}
func healthyThreshold(hc *api.HealthCheck) int {
	if hc != nil && hc.HealthyThreshold > 0 {
		return hc.HealthyThreshold
	}
	return defaultHealthyThreshold
}
func unhealthyThreshold(hc *api.HealthCheck) int {
	if hc != nil && hc.UnhealthyThreshold > 0 {
		return hc.UnhealthyThreshold
	}
	return defaultUnhealthyCount
}

// toResponse merges a persisted record with its observed status.
func (m *Manager) toResponse(rec Record) api.AppResponse {
	desired := "stopped"
	if rec.DesiredRunning {
		desired = "running"
	}
	resp := api.AppResponse{
		ID:           rec.ID,
		AppSpec:      rec.Spec,
		DesiredState: desired,
		Generation:   rec.Generation,
		CreatedAt:    rec.CreatedAt,
		UpdatedAt:    rec.UpdatedAt,
	}
	m.obsMu.Lock()
	ob := m.obs[rec.ID]
	extras := m.extras[rec.ID]
	m.obsMu.Unlock()
	if ob != nil {
		phase := ob.phase
		if phase == "" {
			phase = "pending"
		}
		resp.Status = &api.AppStatus{
			InstanceID:         ob.instanceID,
			InstanceGeneration: ob.generation,
			Phase:              phase,
			Health:             ob.health,
			Restarts:           ob.restarts,
			LastError:          ob.lastErr,
			LastWakeLatencyMs:  ob.lastWakeLatency.Milliseconds(),
			SleepCount:         ob.sleepCount,
		}
		// Endpoint set: the primary (when it has a live instance) plus any extras.
		var insts []api.InstanceStatus
		ready := 0
		if ob.instanceID != "" {
			insts = append(insts, api.InstanceStatus{InstanceID: ob.instanceID, Generation: ob.generation, Health: ob.health})
			if phase == "running" {
				ready++
			}
		}
		for _, r := range extras {
			insts = append(insts, api.InstanceStatus{InstanceID: r.id, Generation: r.generation, Health: r.health})
			ready++
		}
		resp.Status.Instances = insts
		resp.Status.ReadyReplicas = ready
		resp.Status.Replicas = m.replicaTarget(rec)
	}
	return resp
}

// validateSpec checks the structural rules an app spec must satisfy.
func validateSpec(spec api.AppSpec) error {
	if !IsValidName(spec.Name) {
		return fmt.Errorf("app: invalid name %q (want a DNS label: lowercase alphanumeric and hyphens, 1–40 chars)", spec.Name)
	}
	if spec.Image == nil || (spec.Image.OCI == "" && spec.Image.Path == "") {
		return errors.New("app: image is required")
	}
	switch spec.Restart.Policy {
	case "", wire.RestartNever, wire.RestartOnFailure, wire.RestartAlways:
	default:
		return fmt.Errorf("app: unknown restart policy %q", spec.Restart.Policy)
	}
	if hc := spec.Health; hc != nil && hc.Type != "" {
		switch hc.Type {
		case "http", "tcp":
			if hc.Port <= 0 {
				return fmt.Errorf("app: health check type %q requires a port", hc.Type)
			}
		case "exec":
			if len(hc.Cmd) == 0 {
				return errors.New("app: exec health check requires a cmd")
			}
		default:
			return fmt.Errorf("app: unknown health check type %q", hc.Type)
		}
	}
	if sp := spec.Sleep; sp != nil {
		if sp.IdleTimeoutSec < 0 {
			return fmt.Errorf("app: sleep idle_timeout_s must be >= 0, got %d", sp.IdleTimeoutSec)
		}
		// min_scale is the warm-replica floor (v0.5.2): 0 opts into scale-to-zero,
		// >=1 keeps that many instances always running. Negative is invalid.
		if sp.MinScale < 0 {
			return fmt.Errorf("app: sleep min_scale must be >= 0, got %d", sp.MinScale)
		}
		if sp.MaxScale < 0 {
			return fmt.Errorf("app: sleep max_scale must be >= 0, got %d", sp.MaxScale)
		}
		if sp.MaxScale > 0 && sp.MaxScale < sp.MinScale {
			return fmt.Errorf("app: sleep max_scale (%d) must be >= min_scale (%d)", sp.MaxScale, sp.MinScale)
		}
		// Scale-to-zero (min_scale 0 + idle_timeout) needs a way to be woken. Two
		// wake paths exist: an HTTP --port through the ingress proxy, or a published
		// host port (-p) fronted by the L4 waking forwarder (v0.6.1). With neither,
		// a slept app can never come back — reject it rather than silently strand it
		// always-on. (PublishAll/-P is not a wake trigger: the forwarder needs an
		// explicit host→guest mapping, so pair -P with an explicit -p or --port.)
		if sp.MinScale == 0 && sp.IdleTimeoutSec > 0 && spec.Port == 0 && len(spec.Publish) == 0 {
			return errors.New("app: a scale-to-zero app needs a wake trigger — set --port (HTTP proxy) or publish a host port with -p (TCP), or use --min-scale 1")
		}
		if len(spec.Volumes) > 0 {
			// A volume app is single-writer: it can never run 2+ live instances,
			// so scale-out is invalid.
			if sp.MinScale > 1 || sp.MaxScale > 1 {
				return errors.New("app: a volume-backed app cannot scale out (min_scale/max_scale > 1) — ext4 is single-writer")
			}
		}
		if sp.TargetConcurrency < 0 {
			return fmt.Errorf("app: sleep target_concurrency must be >= 0, got %d", sp.TargetConcurrency)
		}
		if sp.ConnIdleTimeoutSec < 0 {
			return fmt.Errorf("app: sleep conn_idle_timeout_s must be >= 0, got %d", sp.ConnIdleTimeoutSec)
		}
	}
	for _, target := range spec.CanCall {
		if !IsValidName(target) {
			return fmt.Errorf("app: invalid can_call target %q (want an app name: a DNS label)", target)
		}
		if target == spec.Name {
			return fmt.Errorf("app: can_call may not list the app itself (%q)", target)
		}
	}
	return nil
}
