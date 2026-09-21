package main

import (
	"errors"
	"testing"
)

// Spec 8: readiness is externally observable. Only a published coherent committed
// set reports Ready; a failed or not-yet-reconciled namespace must not look ready.
func TestAudioNamespaceStateIsObservable(t *testing.T) {
	e := newVFSEnv(t) // vfsEnv installs a fresh namespace as the global one

	if got := audioNamespaceState(); got != "Unreconciled" {
		t.Errorf("fresh namespace state = %q, want Unreconciled", got)
	}
	if got := audioNamespaceEntries(); got != 0 {
		t.Errorf("fresh namespace entries = %d, want 0", got)
	}

	e.ns.MarkUnavailable()
	if got := audioNamespaceState(); got != "Unavailable" {
		t.Errorf("unavailable namespace state = %q, want Unavailable", got)
	}

	e.publish(committedA("Artist/Album/01 - Track_01234567.flac"))
	if got := audioNamespaceState(); got != "Ready" {
		t.Errorf("published namespace state = %q, want Ready", got)
	}
	if got := audioNamespaceEntries(); got != 1 {
		t.Errorf("published namespace entries = %d, want 1", got)
	}

	e.ns.MarkFailed(errors.New("reconciliation failed"))
	if got := audioNamespaceState(); got != "Failed" {
		t.Errorf("failed namespace state = %q, want Failed", got)
	}
}
