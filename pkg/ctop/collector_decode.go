package ctop

// Wave-18 CT-F1 — schema-tolerant decoding of `podman`/`docker` `--format json`
// output. The previous fixed structs matched NEITHER runtime's real output and
// failed to decode it (proven by captured real output in
// wave18_real_runtime_output_test.go): podman `ps` emits a JSON array with an
// `Id` key, a `Names` array, `State`/`Status` as top-level strings, and
// `Created`/`StartedAt` as unix ints; docker `ps` emits NDJSON with an `ID`
// key, a `Names` comma-string, and a `Ports` string; podman `stats` uses
// snake_case field names while docker `stats` uses PascalCase. Every
// polymorphic field is decoded permissively here and normalised.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// decodeJSONObjects splits runtime `--format json` output into individual
// object payloads: a JSON array (podman), NDJSON one-object-per-line (docker),
// or a single object. Empty/whitespace input yields no objects, not an error.
func decodeJSONObjects(data []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var out []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// rawContainerJSON is a permissive decode target tolerating the shape
// differences between `podman ps` and `docker ps --format json` output.
type rawContainerJSON struct {
	ID        string          `json:"Id"`
	IDUpper   string          `json:"ID"`
	Names     json.RawMessage `json:"Names"`
	Image     string          `json:"Image"`
	Created   json.RawMessage `json:"Created"`
	CreatedAt json.RawMessage `json:"CreatedAt"`
	State     json.RawMessage `json:"State"`
	Status    string          `json:"Status"`
	StartedAt json.RawMessage `json:"StartedAt"`
	Labels    json.RawMessage `json:"Labels"`
	Ports     json.RawMessage `json:"Ports"`
}

// decodedContainer is the runtime-neutral result of normalising one record.
type decodedContainer struct {
	ID        string
	Name      string
	Image     string
	State     string
	Status    string
	Created   time.Time
	StartedAt time.Time
	Labels    map[string]string
	Ports     []string
}

func (r rawContainerJSON) normalize() decodedContainer {
	id := r.ID
	if id == "" {
		id = r.IDUpper
	}
	state, status, startedAt := decodeContainerState(r.State, r.Status, r.StartedAt)
	created := decodeRuntimeTime(r.Created)
	if created.IsZero() {
		created = decodeRuntimeTime(r.CreatedAt)
	}
	return decodedContainer{
		ID:        id,
		Name:      decodeContainerName(r.Names),
		Image:     r.Image,
		State:     state,
		Status:    status,
		Created:   created,
		StartedAt: startedAt,
		Labels:    decodeLabels(r.Labels),
		Ports:     decodeRuntimePorts(r.Ports),
	}
}

// decodeLabels reads container labels from either a podman/labels object
// (`{"k":"v"}`) or a docker comma-separated `k=v,k2=v2` string.
func decodeLabels(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		out := map[string]string{}
		for _, pair := range strings.Split(s, ",") {
			pair = strings.TrimSpace(pair)
			if eq := strings.IndexByte(pair, '='); eq >= 0 {
				out[pair[:eq]] = pair[eq+1:]
			}
		}
		return out
	}
	return nil
}

// parseContainerList decodes a container-list payload from either runtime. A
// single malformed record is skipped rather than failing the whole list.
func parseContainerList(data []byte, rt, location string) ([]ContainerProcess, error) {
	raws, err := decodeJSONObjects(data)
	if err != nil {
		return nil, fmt.Errorf("parsing container list: %w", err)
	}
	host := "local"
	if strings.HasPrefix(location, "remote:") {
		host = strings.TrimPrefix(location, "remote:")
	}
	result := make([]ContainerProcess, 0, len(raws))
	for _, raw := range raws {
		var rc rawContainerJSON
		if err := json.Unmarshal(raw, &rc); err != nil {
			continue
		}
		d := rc.normalize()
		uptime := ""
		if !d.StartedAt.IsZero() {
			uptime = formatUptime(time.Since(d.StartedAt))
		}
		result = append(result, ContainerProcess{
			ID:        shortenID(d.ID),
			Name:      d.Name,
			Image:     d.Image,
			Runtime:   rt,
			Host:      host,
			Location:  location,
			State:     d.State,
			Status:    d.Status,
			Created:   d.Created,
			StartedAt: d.StartedAt,
			Uptime:    uptime,
			Labels:    d.Labels,
			Ports:     d.Ports,
		})
	}
	return result, nil
}

// parseContainerStats decodes a `stats --format json` payload, tolerating a
// podman array or a docker single-object / NDJSON line, and both the docker
// PascalCase field names and the podman snake_case names.
func parseContainerStats(data []byte) *ContainerProcess {
	raws, err := decodeJSONObjects(data)
	if err != nil || len(raws) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raws[0], &m); err != nil {
		return nil
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if raw, ok := m[k]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					return s
				}
				return strings.Trim(string(raw), `"`)
			}
		}
		return ""
	}
	memUsage := pick("MemUsage", "mem_usage")
	return &ContainerProcess{
		CPUPercent:    parsePercent(pick("CPUPerc", "cpu_percent")),
		MemoryUsage:   parseMemoryBytes(memUsage),
		MemoryLimit:   parseMemoryLimit(memUsage),
		MemoryPercent: parsePercent(pick("MemPerc", "mem_percent")),
		NetworkRx:     parseNetIO(pick("NetIO", "net_io"), true),
		NetworkTx:     parseNetIO(pick("NetIO", "net_io"), false),
		BlockRead:     parseBlockIO(pick("BlockIO", "block_io"), true),
		BlockWrite:    parseBlockIO(pick("BlockIO", "block_io"), false),
		PIDs:          parsePIDs(pick("PIDs", "pids")),
	}
}

// decodeContainerName reads the first container name from either a podman
// Names array or a docker comma-separated Names string, trimming the "/".
func decodeContainerName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return extractName(arr)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if i := strings.IndexByte(s, ','); i >= 0 {
			s = s[:i]
		}
		return strings.TrimPrefix(strings.TrimSpace(s), "/")
	}
	return ""
}

// decodeContainerState resolves state/status/started-at from the real runtime
// shape: `podman ps` / `docker ps --format json` emit State and Status as
// top-level strings, and StartedAt as a top-level unix int (podman) or RFC3339
// string. A non-string State (which these commands never emit) yields an empty
// state rather than a decode failure.
func decodeContainerState(rawState json.RawMessage, topStatus string, rawStarted json.RawMessage) (state, status string, startedAt time.Time) {
	status = topStatus
	var s string
	if len(rawState) > 0 && json.Unmarshal(rawState, &s) == nil {
		return s, status, decodeRuntimeTime(rawStarted)
	}
	return "", status, decodeRuntimeTime(rawStarted)
}

// decodeRuntimeTime accepts a unix-seconds int (podman) or an RFC3339 string.
// Human strings ("5 days ago") and zero/absent values yield a zero time.
func decodeRuntimeTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		if n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// decodeRuntimePorts reads published ports as "<host-port>/<proto>" from either
// a podman array of port objects (snake_case host_port/protocol) or a docker
// "0.0.0.0:8080->80/tcp, ..." mapping string. Both paths report the HOST port
// (the port a user connects to) so the two runtimes render consistently.
func decodeRuntimePorts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return parseDockerPortsString(s)
	}
	var objs []struct {
		HostPort int    `json:"host_port"`
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(raw, &objs) == nil {
		var out []string
		for _, o := range objs {
			if o.HostPort > 0 {
				out = append(out, fmt.Sprintf("%d/%s", o.HostPort, o.Protocol))
			}
		}
		return out
	}
	return nil
}

// parseDockerPortsString extracts published host ports as "<host-port>/<proto>"
// from a docker Ports string like "0.0.0.0:8080->80/tcp, :::8080->80/tcp". Only
// published mappings (those with "->") are reported.
func parseDockerPortsString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		arrow := strings.Index(part, "->")
		if arrow < 0 {
			continue
		}
		hostSide := part[:arrow]
		contSide := part[arrow+2:]
		proto := ""
		if slash := strings.LastIndexByte(contSide, '/'); slash >= 0 {
			proto = contSide[slash+1:]
		}
		hostPort := hostSide
		if colon := strings.LastIndexByte(hostSide, ':'); colon >= 0 {
			hostPort = hostSide[colon+1:]
		}
		hostPort = strings.TrimSpace(hostPort)
		if hostPort == "" {
			continue
		}
		mapping := hostPort
		if proto != "" {
			mapping = hostPort + "/" + proto
		}
		if !seen[mapping] {
			seen[mapping] = true
			out = append(out, mapping)
		}
	}
	return out
}
