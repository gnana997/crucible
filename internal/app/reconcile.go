package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// Instantiator boots and tears down an app's instance. The daemon
// implements it (mapping an AppSpec through image resolution into a real
// sandbox via internal/sandbox.Manager); the reconciler depends only on
// this narrow interface, so its convergence logic is decoupled from the
// VMM machinery and testable with a fake.
type Instantiator interface {
	// Create boots a new instance from spec and returns its instance
	// (sandbox) id. appID is attached to the instance for attribution.
	Create(ctx context.Context, appID string, spec api.AppSpec) (instanceID string, err error)

	// Exists reports whether the instance is still live (registered in the
	// Manager). The reconciler's liveness signal in v0.4.0; health-based
	// liveness lands with the health checks (W5).
	Exists(instanceID string) bool

	// Destroy tears down the instance. Absent/already-gone is not an error.
	Destroy(ctx context.Context, instanceID string) error
}

// observed is an app's runtime (never persisted) state. It is rebuilt from
// scratch on Start: an empty observed map plus the persisted desired state
// is exactly what drives re-creation after a daemon restart.
type observed struct {
	instanceID string
	generation uint64 // generation of the spec this instance was booted from
	phase      string // api.AppStatus phases
	restarts   int
	lastErr    string
}

// defaultReconcileInterval is how often the loop re-converges absent an
// explicit trigger — the safety net that catches an instance that vanished
// between events.
const defaultReconcileInterval = 10 * time.Second

// Manager is the control-plane engine: it owns the durable app store and a
// reconcile loop that converges actual instances toward desired state.
// Level-triggered and idempotent (the network-reconcile template), so a
// pass is always safe to repeat.
type Manager struct {
	store *Store
	inst  Instantiator
	log   *slog.Logger

	interval time.Duration

	// reconcile serializes convergence passes; obsMu guards the observed
	// map read outside a pass (status reads).
	reconcileMu sync.Mutex
	obsMu       sync.Mutex
	obs         map[string]*observed // appID -> observed

	trigger chan struct{} // coalescing wake for the loop
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewManager returns a control-plane manager over store, instantiating
// through inst. Call Start to run the loop.
func NewManager(store *Store, inst Instantiator, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		store:    store,
		inst:     inst,
		log:      log,
		interval: defaultReconcileInterval,
		obs:      make(map[string]*observed),
		trigger:  make(chan struct{}, 1),
	}
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
	now := time.Now().UTC()
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
	m.Trigger()
	return rec, nil
}

// Delete removes an app and triggers a reconcile that tears down its
// instance. Absent id is ErrNotFound.
func (m *Manager) Delete(id string) error {
	if _, found, err := m.store.Get(id); err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	if err := m.store.Delete(id); err != nil {
		return err
	}
	m.Trigger()
	return nil
}

// SetDesired flips an app between running and stopped (spec retained) and
// triggers a reconcile.
func (m *Manager) SetDesired(id string, running bool) error {
	rec, found, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	rec.DesiredRunning = running
	rec.UpdatedAt = time.Now().UTC()
	if err := m.store.Put(rec); err != nil {
		return err
	}
	m.Trigger()
	return nil
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

// Start runs an initial convergence (this is the survive-restart step:
// desired apps whose instances were reaped get re-created) and then the
// background loop. Returns after the first pass so the daemon knows apps
// are being restored.
func (m *Manager) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// Initial synchronous pass: bring desired apps up before returning.
	m.reconcile(ctx)

	m.wg.Add(1)
	go m.loop(loopCtx)
	return nil
}

// Stop halts the reconcile loop. It does not tear down instances (daemon
// shutdown drains sandboxes separately; the app store retains desired
// state for the next Start).
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

// reconcile converges actual instances toward desired state: tear down
// instances whose app is gone or stopped, then ensure every desired app
// has a live instance, (re)creating from spec. One pass at a time.
func (m *Manager) reconcile(ctx context.Context) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	recs, err := m.store.List()
	if err != nil {
		m.log.Error("app reconcile: list store", "err", err)
		return
	}
	byID := make(map[string]Record, len(recs))
	for _, r := range recs {
		byID[r.ID] = r
	}

	// 1. Tear down instances whose app was deleted or stopped.
	m.obsMu.Lock()
	tracked := make([]string, 0, len(m.obs))
	for appID := range m.obs {
		tracked = append(tracked, appID)
	}
	m.obsMu.Unlock()

	for _, appID := range tracked {
		rec, stillDesired := byID[appID]
		if stillDesired && rec.DesiredRunning {
			continue
		}
		m.obsMu.Lock()
		ob := m.obs[appID]
		m.obsMu.Unlock()
		if ob != nil && ob.instanceID != "" {
			if err := m.inst.Destroy(ctx, ob.instanceID); err != nil {
				m.log.Warn("app reconcile: destroy instance", "app", appID, "instance", ob.instanceID, "err", err)
			}
		}
		if !stillDesired {
			m.obsMu.Lock()
			delete(m.obs, appID)
			m.obsMu.Unlock()
		} else {
			m.setPhase(appID, ob, "stopped")
		}
	}

	// 2. Ensure every desired-running app has a current live instance.
	for _, rec := range recs {
		if !rec.DesiredRunning {
			continue
		}
		m.obsMu.Lock()
		ob := m.obs[rec.ID]
		m.obsMu.Unlock()

		switch {
		case ob == nil || ob.instanceID == "":
			m.bootInstance(ctx, rec, ob, false)
		case !m.inst.Exists(ob.instanceID):
			// Instance vanished. Respect the never-restart policy;
			// backoff + crash-loop guard land in W4.
			if rec.Spec.Restart.Policy == wire.RestartNever {
				m.setPhase(rec.ID, ob, "stopped")
				continue
			}
			m.bootInstance(ctx, rec, ob, true)
		case ob.generation != rec.Generation:
			// Spec changed → redeploy (naive destroy+create; zero-downtime
			// deploys are a later item).
			_ = m.inst.Destroy(ctx, ob.instanceID)
			m.bootInstance(ctx, rec, ob, false)
		}
	}
}

// bootInstance creates a fresh instance for rec and records the observed
// state. isRestart increments the restart counter.
func (m *Manager) bootInstance(ctx context.Context, rec Record, prev *observed, isRestart bool) {
	id, err := m.inst.Create(ctx, rec.ID, rec.Spec)
	restarts := 0
	if prev != nil {
		restarts = prev.restarts
	}
	if isRestart {
		restarts++
	}
	if err != nil {
		m.log.Error("app reconcile: boot instance", "app", rec.ID, "name", rec.Spec.Name, "err", err)
		m.obsMu.Lock()
		m.obs[rec.ID] = &observed{generation: rec.Generation, phase: "pending", restarts: restarts, lastErr: err.Error()}
		m.obsMu.Unlock()
		return
	}
	m.log.Info("app instance booted", "app", rec.ID, "name", rec.Spec.Name, "instance", id, "restart", isRestart)
	m.obsMu.Lock()
	m.obs[rec.ID] = &observed{instanceID: id, generation: rec.Generation, phase: "running", restarts: restarts}
	m.obsMu.Unlock()
}

func (m *Manager) setPhase(appID string, ob *observed, phase string) {
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	if cur := m.obs[appID]; cur != nil {
		cur.phase = phase
		cur.instanceID = ""
	} else if ob != nil {
		ob.phase = phase
		ob.instanceID = ""
		m.obs[appID] = ob
	}
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
	m.obsMu.Unlock()
	if ob != nil {
		phase := ob.phase
		if phase == "" {
			phase = "pending"
		}
		resp.Status = &api.AppStatus{
			InstanceID: ob.instanceID,
			Phase:      phase,
			Restarts:   ob.restarts,
			LastError:  ob.lastErr,
		}
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
	return nil
}
