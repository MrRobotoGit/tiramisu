package webhook

import "testing"

func TestIdentityKeysRankByShape(t *testing.T) {
	const rec, rel, art, extra = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444"
	payload := `{"Metadata":{"Guid":[{"id":"mbid://` + rec + `"}],"parentGuid":"mbid://` + rel +
		`","grandparentGuid":"mbid://` + art + `","other":"mbid://` + extra + `"}}`
	k := IdentityKeysFromPayload(payload)
	for id, want := range map[string]int{rec: 2, rel: 1, art: 0, extra: 0} {
		if got, ok := k.Rank(id); !ok || got != want {
			t.Errorf("%s: rank %d %v, want %d", id, got, ok, want)
		}
	}
	if k.Empty() || !(IdentityKeys{}).Empty() {
		t.Fatal("Empty")
	}
}

func TestJellyfinProviderIDs(t *testing.T) {
	const tr = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	k := IdentityKeysFromPayload(`{"Metadata":{"ProviderIds":{"MusicBrainzTrack":"` + tr + `"}}}`)
	if r, ok := k.Rank(tr); !ok || r != 2 {
		t.Fatalf("track id rank %d %v", r, ok)
	}
}

func TestImdbIDAndSanitize(t *testing.T) {
	if got := ImdbID(`{"Guid":[{"id":"imdb://tt0111161"}]}`); got != "tt0111161" {
		t.Fatalf("imdb %q", got)
	}
	if ImdbID(`{}`) != "" {
		t.Fatal("imdb from nothing")
	}
	// Pins the behaviour kept from the old code: the comma goes with the empty value.
	if got := SanitizeEmptyNumbers(`{"year": ,"x":1}`); got != `{"year":0"x":1}` {
		t.Fatalf("sanitize %q", got)
	}
}
