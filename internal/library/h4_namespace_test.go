package library

import (
	"testing"
)

// H4: a live add is authoritative for its own path, and it survives a reconciliation
// pass that read the registry before it committed. Without the merge, the pass's whole
// set replaces the namespace and the add silently vanishes from the mount.
func TestAudioNamespacePublishMergedKeepsLiveAdditions_H4(t *testing.T) {
	n := NewAudioNamespace()
	snapshot := AudioProjection{Section: SectionMusic, VirtualPath: "Old/1.flac", Hash: "a", FileIndex: 1}
	n.Publish([]AudioProjection{snapshot})

	// The pass read its rows before this add committed; its own row is stale.
	live := AudioProjection{Section: SectionMusic, VirtualPath: "Live/2.flac", Hash: "b", FileIndex: 2}
	stale := live
	stale.Hash = "stale"
	n.AddProjections([]AudioProjection{live})
	n.PublishMerged([]AudioProjection{snapshot, stale})

	if got, ok := n.Lookup(live.Section, live.VirtualPath); !ok || got.Hash != live.Hash {
		t.Errorf("merged path = (%+v, %v), want the live projection %+v", got, ok, live)
	}
	if _, ok := n.Lookup(snapshot.Section, snapshot.VirtualPath); !ok {
		t.Error("the reconciled row is missing from the merged namespace")
	}

	// An addition absent from the snapshot survives the next merge.
	other := AudioProjection{Section: SectionMusic, VirtualPath: "Live/3.flac", Hash: "c", FileIndex: 3}
	n.AddProjections([]AudioProjection{other})
	n.PublishMerged([]AudioProjection{snapshot})
	if _, ok := n.Lookup(other.Section, other.VirtualPath); !ok {
		t.Error("a live addition absent from the snapshot vanished in the merge")
	}

	// A full publish supersedes live provenance: it carries the whole set.
	n.Publish(nil)
	if _, ok := n.Lookup(live.Section, live.VirtualPath); ok {
		t.Error("Publish must replace the whole set, live additions included")
	}
}
