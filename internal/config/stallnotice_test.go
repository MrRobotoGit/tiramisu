package config

import "testing"

func TestStallNoticeOnByDefault(t *testing.T) {
	for doc, want := range map[string]bool{`{}`: true, `{"stall_notice": false}`: false, `{"stall_notice": true}`: true} {
		cfg, err := applyDocument([]byte(doc))
		if err != nil || cfg.StallNotice != want {
			t.Fatalf("%s: stall_notice=%v err=%v", doc, cfg.StallNotice, err)
		}
	}
	if !liveKeys["stall_notice"] {
		t.Fatal("stall_notice must apply without a restart")
	}
}
