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
	mu           sync.RWMutex
	persistMu    sync.Mutex
	services     map[string]*Service
	serviceFiles map[string]string
	registryDir  string
	defaultHost  string
	logger       Logger
}

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
	r.persist()
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
			svc, _ := r.Get(name)
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

// persist writes a snapshot of the registry to disk. The snapshot is marshaled
// under the read lock (an in-memory, non-blocking step), but the blocking disk
// I/O runs OUTSIDE r.mu — MANDATORY DEVELOPMENT PRINCIPLE #2: no blocking
// operation may run inside a synchronized region, so a slow fsync never stalls
// every reader/writer waiting on r.mu. persistMu serializes concurrent writers
// (they no longer hold r.mu, so two could otherwise race on the file). The
// write is atomic: a temp file in the same directory is renamed over the
// target, so a crash mid-write leaves the previous registry intact rather than
// a truncated file (§9 data-safety).
func (r *ServiceRegistry) persist() {
	if r.registryDir == "" {
		return
	}

	r.mu.RLock()
	data, err := json.MarshalIndent(r.services, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		r.logger.Warn("Failed to marshal services: %v", err)
		return
	}

	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	if err := os.MkdirAll(r.registryDir, 0755); err != nil {
		r.logger.Warn("Failed to create registry dir: %v", err)
		return
	}

	file := filepath.Join(r.registryDir, "services.json")
	tmp, err := os.CreateTemp(r.registryDir, "services-*.json.tmp")
	if err != nil {
		r.logger.Warn("Failed to create temp registry file: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		r.logger.Warn("Failed to write temp registry: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		r.logger.Warn("Failed to close temp registry: %v", err)
		return
	}
	if err := os.Rename(tmpName, file); err != nil {
		os.Remove(tmpName)
		r.logger.Warn("Failed to rename registry into place: %v", err)
	}
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
	defer r.mu.Unlock()
	r.services = make(map[string]*Service)
	if r.registryDir != "" {
		file := filepath.Join(r.registryDir, "services.json")
		os.Remove(file)
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
