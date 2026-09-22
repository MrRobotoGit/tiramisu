package library

import (
	"fmt"

	"tiramisu/internal/metadb"
)

// AudioProjectionStatus is what a requested projection would become. Idempotency
// is per projection, so a partly present album reports a mix of the two.
type AudioProjectionStatus string

const (
	AudioProjectionPresent AudioProjectionStatus = "present"
	AudioProjectionCreated AudioProjectionStatus = "created"
)

// AudioProjectionLookup is the read side of the projection registry.
// *metadb.DB satisfies it.
type AudioProjectionLookup interface {
	GetAudioProjection(section, virtualPath string) (*metadb.AudioProjection, bool, error)
	AudioProjectionBySource(hash string, fileIndex, cueTrack int) (*metadb.AudioProjection, bool, error)
	AudioProjectionByPortableKey(section, portableKey string) (*metadb.AudioProjection, bool, error)
}

// AudioProjectionPlan is one requested projection and what it would become.
// Existing is set only when the projection is already present.
type AudioProjectionPlan struct {
	VirtualPath string
	Source      ResolvedSource
	Status      AudioProjectionStatus
	Existing    *metadb.AudioProjection
}

// ClassifyAudioProjection decides whether a projection is present, would be
// created, or conflicts. The destination is checked before the release.
func ClassifyAudioProjection(lookup AudioProjectionLookup, section Section, hash, virtualPath string, source ResolvedSource, cueTrack int) (AudioProjectionPlan, error) {
	existing, found, err := lookup.GetAudioProjection(string(section), virtualPath)
	if err != nil {
		return AudioProjectionPlan{}, fmt.Errorf("reading projection %s/%s: %w", section, virtualPath, err)
	}
	if found {
		// Only a committed row counts as present, and every identity field must match: a
		// re-sorted torrent keeps the hash but moves the index and source path.
		if existing.State == metadb.AudioCommitted &&
			existing.Hash == hash &&
			existing.FileIndex == source.FileIndex &&
			existing.CueTrack == cueTrack &&
			existing.SourcePath == source.SourcePath &&
			existing.Size == source.Size {
			return AudioProjectionPlan{
				VirtualPath: virtualPath,
				Source:      source,
				Status:      AudioProjectionPresent,
				Existing:    existing,
			}, nil
		}
		return AudioProjectionPlan{}, fmt.Errorf("%w: %s/%s already holds different content", metadb.ErrAudioPathConflict, section, virtualPath)
	}

	// The registry constrains the portable key independently of virtual_path, so
	// a spelling differing only by case or normal form is already taken.
	portableKey := PortablePathKey(virtualPath)
	if keyOwner, taken, err := lookup.AudioProjectionByPortableKey(string(section), portableKey); err != nil {
		return AudioProjectionPlan{}, fmt.Errorf("reading portable key %s/%s: %w", section, portableKey, err)
	} else if taken {
		return AudioProjectionPlan{}, fmt.Errorf("%w: %s/%s collides with %s on a case-insensitive or normalising filesystem", metadb.ErrAudioPathConflict, section, virtualPath, keyOwner.VirtualPath)
	}

	owner, found, err := lookup.AudioProjectionBySource(hash, source.FileIndex, cueTrack)
	if err != nil {
		return AudioProjectionPlan{}, fmt.Errorf("reading source %s:%d: %w", hash, source.FileIndex, err)
	}
	if found {
		return AudioProjectionPlan{}, fmt.Errorf("%w: %s:%d is already projected at %s/%s", metadb.ErrAudioSourceConflict, hash, source.FileIndex, owner.Section, owner.VirtualPath)
	}
	return AudioProjectionPlan{VirtualPath: virtualPath, Source: source, Status: AudioProjectionCreated}, nil
}

// PlanAudioProjections classifies a whole request, in request order. It is
// fail-closed: one conflict fails the call and returns no partial plan.
func PlanAudioProjections(lookup AudioProjectionLookup, section Section, hash string, requests []AudioFileRequest, sources []ResolvedSource) ([]AudioProjectionPlan, error) {
	if len(requests) != len(sources) {
		return nil, fmt.Errorf("%d requests but %d resolved sources", len(requests), len(sources))
	}
	plans := make([]AudioProjectionPlan, 0, len(requests))
	// Two distinct source paths can resolve to one file index, which the registry
	// would only catch at commit; planning must not project it twice.
	type sourceKey struct{ index, cueTrack int }
	claimedBy := make(map[sourceKey]string, len(requests))
	keyClaimedBy := make(map[string]string, len(requests))
	for i := range requests {
		// The destination is classified first so a taken path outranks an in-request
		// duplicate, wherever the duplicate sits.
		plan, err := ClassifyAudioProjection(lookup, section, hash, requests[i].Path, sources[i], requests[i].CueTrack)
		if err != nil {
			return nil, err
		}
		// Two spellings in one request collide for the same reason two rows would.
		portableKey := PortablePathKey(requests[i].Path)
		if previous, claimed := keyClaimedBy[portableKey]; claimed {
			return nil, fmt.Errorf("%w: %q and %q share a portable key", metadb.ErrAudioPathConflict, previous, requests[i].Path)
		}
		keyClaimedBy[portableKey] = requests[i].Path

		key := sourceKey{sources[i].FileIndex, requests[i].CueTrack}
		if previous, claimed := claimedBy[key]; claimed {
			return nil, fmt.Errorf("%w: %s:%d requested at both %q and %q", metadb.ErrAudioSourceConflict, hash, sources[i].FileIndex, previous, requests[i].Path)
		}
		claimedBy[key] = requests[i].Path
		plans = append(plans, plan)
	}
	return plans, nil
}
