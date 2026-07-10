package serviceregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Service struct {
	Name         string            `json:"name"`
	Host         string            `json:"host"`
	Port         int               `json:"port"`
	Protocol     string            `json:"protocol"`
	HealthPath   string            `json:"health_path,omitempty"`
	HealthType   string            `json:"health_type,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	DiscoveredAt time.Time         `json:"discovered_at"`
	LastChecked  time.Time         `json:"last_checked"`
	Healthy      bool              `json:"healthy"`
}

type ServiceRegistry struct {
	mu               sync.RWMutex
	persistMu        sync.Mutex
	services         map[string]*Service
	serviceFiles     map[string]string
	registryDir      string
	defaultHost      string
	logger           Logger
	warnEmptyDirOnce sync.Once // SR-HARD-3: warn exactly once when persist() is a no-op because registryDir is empty
}

// persistBeforeLockHook is a package-level test seam (nil in production, so the
// only production cost is a single nil check). It is invoked inside persist() at
// the point IMMEDIATELY BEFORE persistMu is acquired. Because the SR-HARD-1 fix
// marshals the snapshot AFTER acquiring persistMu, a test can pause a persist
// here — before it captures anything — mutate the registry, run a second persist
// to completion, then release, and deterministically observe that the released
// persist captures the FRESH state (freshest-wins) rather than a stale snapshot
// captured before the mutation. Same idiomatic seam pattern as pkg/vm's
// killByPortHook. Tests overriding it MUST NOT use t.Parallel() (the
// swap-and-restore of a package-level var is not concurrency-safe).
var persistBeforeLockHook func()

// persistBeforeRenameHook is a second package-level test seam (nil in
// production). It is invoked inside persist() IMMEDIATELY BEFORE the atomic
// os.Rename — the point at which the snapshot is already marshaled to a temp
// file and persistMu is held, but the write has not yet been committed. The
// SR-HARD-2 guard pauses a persist here (a non-empty snapshot captured, about to
// land) to prove Clear() cannot remove the file and then have the paused persist
// resurrect it: with the fix Clear blocks on persistMu until the rename commits
// and only then removes the file; reverting the fix lets Clear remove the file
// first, so the paused persist's rename resurrects the cleared service. Tests
// overriding it MUST NOT use t.Parallel().
var persistBeforeRenameHook func()

type Logger interface {
	Info(msg string, args ...any)
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Info(msg string, args ...any)  {}
func (nopLogger) Debug(msg string, args ...any) {}
func (nopLogger) Warn(msg string, args ...any)  {}
func (nopLogger) Error(msg string, args ...any) {}

type Option func(*ServiceRegistry)

func WithLogger(l Logger) Option {
	return func(r *ServiceRegistry) { r.logger = l }
}

func WithRegistryDir(dir string) Option {
	return func(r *ServiceRegistry) { r.registryDir = dir }
}

func WithDefaultHost(host string) Option {
	return func(r *ServiceRegistry) { r.defaultHost = host }
}

func New(opts ...Option) *ServiceRegistry {
	r := &ServiceRegistry{
		services:     make(map[string]*Service),
		serviceFiles: make(map[string]string),
		defaultHost:  "localhost",
		logger:       nopLogger{},
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.registryDir == "" {
		if dir, err := os.Getwd(); err == nil {
			r.registryDir = filepath.Join(dir, ".service-registry")
		}
	}
	r.loadFromDisk()
	return r
}

func (r *ServiceRegistry) Register(name string, port int, opts ...ServiceOption) error {
	r.mu.Lock()

	svc := &Service{
		Name:     name,
		Host:     r.defaultHost,
		Port:     port,
		Protocol: "tcp",
		Healthy:  true,
		Labels:   make(map[string]string),
	}
	for _, opt := range opts {
		opt(svc)
	}
	svc.DiscoveredAt = time.Now()
	svc.LastChecked = time.Now()

	r.services[name] = svc
	host, port := svc.Host, svc.Port
	r.mu.Unlock()

	r.logger.Info("Registered service %s at %s:%d", name, host, port)
	// SR-HARD-3: persist() now returns its failure. Register's signature already
	// returns error, so propagate it — a service the caller believes is durably
	// registered but never reached disk (MkdirAll/Write/Rename failed) is a
	// silent-persistence-failure defect. The in-memory registration stands; the
	// error tells the caller the on-disk snapshot did not update.
	if err := r.persist(); err != nil {
		return fmt.Errorf("register %s: persist failed: %w", name, err)
	}
	return nil
}

type ServiceOption func(*Service)

func WithHost(host string) ServiceOption {
	return func(s *Service) { s.Host = host }
}

func WithHealthPath(path string) ServiceOption {
	return func(s *Service) { s.HealthPath = path }
}

func WithHealthType(ht string) ServiceOption {
	return func(s *Service) { s.HealthType = ht }
}

func WithProtocol(p string) ServiceOption {
	return func(s *Service) { s.Protocol = p }
}

func WithLabels(labels map[string]string) ServiceOption {
	return func(s *Service) {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}
		for k, v := range labels {
			s.Labels[k] = v
		}
	}
}

// cloneLabels returns a deep copy of a service's Labels map so that a Service
// value handed out by an accessor (Get/GetAll/List/Discover) does not alias the
// registry's internal map. A bare struct copy (`copy := *svc`) duplicates only
// the map header, leaving both structs pointing at the same backing map — a
// caller mutating the returned Labels would corrupt stored state and could race
// a concurrent accessor ranging the same map. Nil-safe: a nil input yields nil.
func cloneLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *ServiceRegistry) Get(name string) (*Service, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	svc, ok := r.services[name]
	if !ok {
		return nil, false
	}
	copy := *svc
	copy.Labels = cloneLabels(svc.Labels)
	return &copy, true
}

func (r *ServiceRegistry) GetEndpoint(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if svc, ok := r.services[name]; ok {
		return fmt.Sprintf("%s:%d", svc.Host, svc.Port)
	}
	return ""
}

func (r *ServiceRegistry) GetURL(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if svc, ok := r.services[name]; ok {
		if svc.Protocol == "https" {
			return fmt.Sprintf("https://%s:%d", svc.Host, svc.Port)
		}
		return fmt.Sprintf("http://%s:%d", svc.Host, svc.Port)
	}
	return ""
}

func (r *ServiceRegistry) GetAll() map[string]Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]Service, len(r.services))
	for name, svc := range r.services {
		s := *svc
		s.Labels = cloneLabels(svc.Labels)
		result[name] = s
	}
	return result
}

func (r *ServiceRegistry) List() []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Service, 0, len(r.services))
	for _, svc := range r.services {
		s := *svc
		s.Labels = cloneLabels(svc.Labels)
		result = append(result, s)
	}
	return result
}

func (r *ServiceRegistry) Unregister(name string) {
	r.mu.Lock()
	delete(r.services, name)
	r.mu.Unlock()
	r.logger.Info("Unregistered service %s", name)
	r.persist()
}

func (r *ServiceRegistry) UpdateHealth(name string, healthy bool) {
	r.mu.Lock()
	svc, ok := r.services[name]
	if ok {
		svc.Healthy = healthy
		svc.LastChecked = time.Now()
	}
	r.mu.Unlock()
	if ok {
		r.persist()
	}
}

func (r *ServiceRegistry) Discover(ctx context.Context, name string, defaultPort int, portRange ...int) (*Service, error) {
	r.mu.RLock()
	if svc, ok := r.services[name]; ok {
		cp := *svc
		cp.Labels = cloneLabels(svc.Labels)
		r.mu.RUnlock()
		return &cp, nil
	}
	r.mu.RUnlock()

	start := defaultPort
	end := defaultPort + 1
	if len(portRange) >= 2 {
		start = portRange[0]
		end = portRange[1]
	}

	for port := start; port < end; port++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if r.checkPort(r.defaultHost, port) {
			if err := r.Register(name, port); err != nil {
				return nil, err
			}
			svc, ok := r.Get(name)
			if !ok {
				// The service was unregistered between Register and this read
				// (a concurrent Unregister lands in the window where Discover
				// holds no lock). Surface the absence honestly rather than
				// returning (nil, nil), which would nil-deref any caller that
				// checks only the error before using the returned *Service
				// (CT-HARDEN-SR-DISCOVER).
				return nil, fmt.Errorf("service %s was unregistered during discovery", name)
			}
			return svc, nil
		}
	}

	return nil, fmt.Errorf("service %s not discovered in port range %d-%d", name, start, end-1)
}

func (r *ServiceRegistry) DiscoverMultiple(ctx context.Context, services map[string]int) error {
	for name, defaultPort := range services {
		if _, err := r.Discover(ctx, name, defaultPort, defaultPort, defaultPort+100); err != nil {
			r.logger.Warn("Failed to discover service %s: %v", name, err)
		}
	}
	return nil
}

func (r *ServiceRegistry) checkPort(host string, port int) bool {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// FindAvailablePort returns the first port at or above startPort that is free
// to bind. NOTE (inherent TOCTOU): the port is confirmed free by binding and
// immediately closing a listener, so between this call and the caller actually
// binding the port another process can claim it. Callers that need a race-free
// port MUST bind with port 0 (let the OS assign) and keep the returned
// listener, rather than relying on this check-then-use helper.
func (r *ServiceRegistry) FindAvailablePort(startPort int) int {
	for port := startPort; port < startPort+10000; port++ {
		if r.isPortAvailable(port) {
			return port
		}
	}
	return 0
}

func (r *ServiceRegistry) isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", r.defaultHost, port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// persist writes a snapshot of the registry to disk under persistMu. persistMu
// is acquired FIRST, then the snapshot is marshaled under r.mu.RLock (an
// in-memory, non-blocking step), then the blocking disk I/O runs — all while
// holding persistMu but with r.mu already released.
//
// SR-HARD-1: capturing the snapshot INSIDE persistMu makes snapshot-capture and
// the atomic rename one indivisible unit with respect to every other persist()
// call. If the marshal ran OUTSIDE persistMu (as it once did), a goroutine that
// captured an OLDER snapshot could block on the contended persistMu while a
// goroutine holding a NEWER snapshot wrote first, then rename its stale bytes
// LAST — resurrecting an Unregister'd service (or dropping a just-Register'd
// one) on the next restart. Acquiring persistMu first guarantees write order
// equals capture order, so the last writer always holds the freshest state.
//
// MANDATORY DEVELOPMENT PRINCIPLE #2 is preserved: only the marshal runs under
// r.mu.RLock; the blocking os.MkdirAll/CreateTemp/Write/Rename run with r.mu
// released, so a slow fsync never stalls readers/writers waiting on r.mu.
// persistMu (a separate mutex) intentionally serializes the writers. The write
// is atomic: a temp file in the same directory is renamed over the target, so a
// crash mid-write leaves the previous registry intact rather than a truncated
// file (§9 data-safety).
//
// SR-HARD-3: persist returns its failure instead of only logging it, so callers
// whose signature carries an error (Register) can surface a silent-persistence
// failure rather than reporting durable success on a snapshot that never landed.
func (r *ServiceRegistry) persist() error {
	if r.registryDir == "" {
		// SR-HARD-3: an empty registryDir (e.g. os.Getwd() failed in New) makes
		// persist a silent in-memory-only no-op. Surface it once at Warn so the
		// operator knows persistence is disabled instead of assuming it works.
		r.warnEmptyDirOnce.Do(func() {
			r.logger.Warn("service registry directory is empty; persistence disabled (in-memory only)")
		})
		return nil
	}

	// SR-HARD-1 test seam: nil in production. Fires BEFORE persistMu is acquired
	// (and therefore before the snapshot is marshaled), the controllable point a
	// deterministic guard uses to force the stale-write interleaving.
	if persistBeforeLockHook != nil {
		persistBeforeLockHook()
	}

	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	r.mu.RLock()
	data, err := json.MarshalIndent(r.services, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		r.logger.Warn("Failed to marshal services: %v", err)
		return fmt.Errorf("marshal services: %w", err)
	}

	if err := os.MkdirAll(r.registryDir, 0755); err != nil {
		r.logger.Warn("Failed to create registry dir: %v", err)
		return fmt.Errorf("create registry dir: %w", err)
	}

	file := filepath.Join(r.registryDir, "services.json")
	tmp, err := os.CreateTemp(r.registryDir, "services-*.json.tmp")
	if err != nil {
		r.logger.Warn("Failed to create temp registry file: %v", err)
		return fmt.Errorf("create temp registry file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		r.logger.Warn("Failed to write temp registry: %v", err)
		return fmt.Errorf("write temp registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		r.logger.Warn("Failed to close temp registry: %v", err)
		return fmt.Errorf("close temp registry: %w", err)
	}
	// SR-HARD-2 test seam: nil in production. Fires with persistMu held and the
	// temp file written, immediately before the atomic rename commits the write.
	if persistBeforeRenameHook != nil {
		persistBeforeRenameHook()
	}
	if err := os.Rename(tmpName, file); err != nil {
		os.Remove(tmpName)
		r.logger.Warn("Failed to rename registry into place: %v", err)
		return fmt.Errorf("rename registry into place: %w", err)
	}
	return nil
}

func (r *ServiceRegistry) loadFromDisk() {
	if r.registryDir == "" {
		return
	}
	file := filepath.Join(r.registryDir, "services.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}

	var loaded map[string]*Service
	if err := json.Unmarshal(data, &loaded); err != nil {
		// A corrupt registry file must NOT silently yield an empty registry —
		// that looks identical to "no services were ever registered" and masks
		// real data loss. Preserve the corrupt bytes aside for recovery and
		// surface the failure loudly instead of swallowing it.
		corrupt := file + ".corrupt"
		if renameErr := os.Rename(file, corrupt); renameErr != nil {
			r.logger.Error(
				"Registry file %s is corrupt (%v) and could not be preserved: %v",
				file, err, renameErr,
			)
		} else {
			r.logger.Error(
				"Registry file %s is corrupt (%v); moved aside to %s",
				file, err, corrupt,
			)
		}
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, svc := range loaded {
		r.services[name] = svc
	}
	r.logger.Info("Loaded %d services from registry", len(loaded))
}

func (r *ServiceRegistry) Clear() {
	r.mu.Lock()
	r.services = make(map[string]*Service)
	r.mu.Unlock()

	if r.registryDir == "" {
		return
	}
	// SR-HARD-2: the file removal is coordinated with persist() through persistMu
	// so an in-flight persist() (which holds persistMu across its snapshot capture
	// AND its atomic rename) can never rename a stale, non-empty snapshot back
	// over the file after Clear removed it — which would resurrect the cleared
	// services on the next restart. The blocking os.Remove runs OUTSIDE r.mu
	// (MANDATORY DEVELOPMENT PRINCIPLE #2), matching persist()'s discipline.
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	file := filepath.Join(r.registryDir, "services.json")
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		r.logger.Warn("Failed to remove registry file on Clear: %v", err)
	}
}

var globalRegistry *ServiceRegistry
var globalRegistryMu sync.Mutex

func Global() *ServiceRegistry {
	globalRegistryMu.Lock()
	defer globalRegistryMu.Unlock()
	if globalRegistry == nil {
		globalRegistry = New()
	}
	return globalRegistry
}

func SetGlobal(r *ServiceRegistry) {
	globalRegistryMu.Lock()
	defer globalRegistryMu.Unlock()
	globalRegistry = r
}
