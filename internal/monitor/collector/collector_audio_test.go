package collector

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The dashboard reads readiness from the same /metrics scrape as everything else, so
// the collector must carry the two audio fields (spec 8).
func TestFetchFUSEBufferReadsAudioNamespaceReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"read_ahead_budget":1048576,"v304_banned_peers":3,` +
			`"audio_namespace_state":"Ready","audio_namespace_entries":42}`))
	}))
	defer server.Close()

	c := &Collector{metricsURL: server.URL, httpClient: server.Client()}
	var status HealthStatus
	c.fetchFUSEBuffer(&status)

	if status.AudioNamespaceState != "Ready" {
		t.Errorf("audio namespace state = %q, want Ready", status.AudioNamespaceState)
	}
	if status.AudioNamespaceEntries != 42 {
		t.Errorf("audio namespace entries = %d, want 42", status.AudioNamespaceEntries)
	}
	if status.V304BannedPeers != 3 {
		t.Errorf("sanity: banned peers = %d, want 3", status.V304BannedPeers)
	}
}
