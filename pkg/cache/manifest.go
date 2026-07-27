// Package cache provides a content-addressable store for image
// artifacts (qcow2 for VMs, Android system-images for emulators).
//
// Anti-bluff posture (clauses 6.J/6.L inherited from Containers' parent):
// the SHA-256 verify-on-fetch is the load-bearing safety property.
// A downloaded image whose computed SHA does not match the manifest
// entry's declared SHA is REJECTED — the partial download is removed,
// and Get returns an error. This is what makes the cache bluff-resistant:
// a silent corrupt cache hit is exactly what §6.J forbids.
package cache

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Manifest declares a project's pinned image artifacts. Lives outside
// Containers (the consuming project ships its at tools/lava-containers/vm-images.json).
// pkg/cache/ defines the schema; consumers populate it.
type Manifest struct {
	Version int          `json:"version"` // currently 1
	Images  []ImageEntry `json:"images"`
}

// ImageEntry is a single pinned artifact.
type ImageEntry struct {
	ID     string `json:"id"`     // canonical id, e.g. "alpine-3.20-x86_64"
	URL    string `json:"url"`    // source URL the cache fetches on miss
	SHA256 string `json:"sha256"` // hex-encoded, 64 chars, REQUIRED
	Size   int64  `json:"size"`   // bytes, REQUIRED — sanity-check the download
	Format string `json:"format"` // "qcow2" | "android-system-image" | "raw"
}

const supportedManifestVersion = 1

// LoadManifest parses + validates a manifest file.
//
// Validation rules (all REQUIRED — relaxing any of them is the
// canonical bluff vector for this package; see manifest_test.go's
// falsifiability rehearsals):
//
//   - schema version MUST equal supportedManifestVersion
//   - every image MUST have a non-empty ID
//   - IDs MUST be unique within the manifest
//   - every SHA256 MUST be exactly 64 hex chars (lowercase or upper)
//   - URL + Size MUST be present
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Version != supportedManifestVersion {
		return nil, fmt.Errorf("manifest %s: schema version %d not supported (expect %d)",
			path, m.Version, supportedManifestVersion)
	}
	seen := make(map[string]bool)
	for i, e := range m.Images {
		if e.ID == "" {
			return nil, fmt.Errorf("manifest %s: image[%d] has empty id", path, i)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("manifest %s: duplicate id %q", path, e.ID)
		}
		seen[e.ID] = true
		if len(e.SHA256) != 64 {
			return nil, fmt.Errorf("manifest %s: image %q has malformed SHA256 (len=%d, want 64)",
				path, e.ID, len(e.SHA256))
		}
		if _, err := hex.DecodeString(e.SHA256); err != nil {
			return nil, fmt.Errorf("manifest %s: image %q SHA256 not hex: %w", path, e.ID, err)
		}
		if e.URL == "" {
			return nil, fmt.Errorf("manifest %s: image %q has empty url", path, e.ID)
		}
		if e.Size <= 0 {
			return nil, fmt.Errorf("manifest %s: image %q has non-positive size", path, e.ID)
		}
	}
	return &m, nil
}

// FindByID returns the image entry whose ID matches.
//
// A nil receiver returns a clean error instead of panicking. This is the
// single choke point Store.Get/Verify/Refresh all funnel through, so
// guarding here protects the whole exported Store API against a nil
// *Manifest — e.g. a caller that forwards LoadManifest's return value
// without checking its error, or any future external consumer of this
// decoupled, reusable module (per the containers module's §11.4.28
// equal-codebase mandate this package must stay project-agnostic and
// safe for callers this repo does not control).
func (m *Manifest) FindByID(id string) (*ImageEntry, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest: nil manifest (no image with id %q)", id)
	}
	for i := range m.Images {
		if m.Images[i].ID == id {
			return &m.Images[i], nil
		}
	}
	return nil, fmt.Errorf("manifest: no image with id %q", id)
}
