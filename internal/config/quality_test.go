package config

import "testing"

// Absent profiles must yield exactly the values compiled into the engines today,
// so an install that never opens the panel keeps its current picks.
func TestQualityDefaultsMatchShippedConstants(t *testing.T) {
	m := DefaultMovieWeights()
	cases := map[string]int{
		"res_4k": 1000, "res_1080p": 200, "hdr": 60, "dolby_vision": 100,
		"atmos": 50, "audio_5_1": 25, "stereo_penalty": -50, "remux": 30,
		"preferred_language": 60, "unknown_size_4k_penalty": -5,
		"seeder_cap": 50, "min_seeders": 15, "min_4k_gb": 10, "max_4k_gb": 40,
		"min_1080p_gb": 4, "max_1080p_gb": 20,
	}
	got := map[string]int{
		"res_4k": m.Res4K, "res_1080p": m.Res1080p, "hdr": m.HDR, "dolby_vision": m.DolbyVision,
		"atmos": m.Atmos, "audio_5_1": m.Audio51, "stereo_penalty": m.StereoPenalty, "remux": m.Remux,
		"preferred_language": m.PreferredLanguage, "unknown_size_4k_penalty": m.UnknownSize4KPenalty,
		"seeder_cap": m.SeederCap, "min_seeders": m.MinSeeders, "min_4k_gb": m.Min4KGB, "max_4k_gb": m.Max4KGB,
		"min_1080p_gb": m.Min1080pGB, "max_1080p_gb": m.Max1080pGB,
	}
	for k, want := range cases {
		if got[k] != want {
			t.Errorf("movie %s = %d, want %d", k, got[k], want)
		}
	}

	tv := DefaultTVWeights()
	tvCases := map[string]int{
		"res_4k": 1000, "res_1080p": 200, "hdr": 100, "dolby_vision": 150, "atmos": 50, "audio_5_1": 25,
		"preferred_language": 40, "fullpack": 500,
		"seeder_tier_100": 100, "seeder_tier_50": 50, "seeder_tier_20": 10, "min_seeders": 5,
		"min_seeders_4k": 5, "season_skip_score": 1000,
	}
	tvGot := map[string]int{
		"res_4k": tv.Res4K, "res_1080p": tv.Res1080p, "hdr": tv.HDR, "dolby_vision": tv.DolbyVision, "atmos": tv.Atmos,
		"audio_5_1": tv.Audio51, "preferred_language": tv.PreferredLanguage, "fullpack": tv.Fullpack,
		"seeder_tier_100": tv.SeederTier100, "seeder_tier_50": tv.SeederTier50,
		"seeder_tier_20": tv.SeederTier20, "min_seeders": tv.MinSeeders,
		"min_seeders_4k": tv.MinSeeders4K, "season_skip_score": tv.SeasonSkipScore,
	}
	for k, want := range tvCases {
		if tvGot[k] != want {
			t.Errorf("tv %s = %d, want %d", k, tvGot[k], want)
		}
	}
}

// A nil profile means "use defaults"; a present one is taken as written, including zeros.
func TestQualityResolveHonoursExplicitZero(t *testing.T) {
	var cfg QualityScoringConfig
	if got := cfg.MovieWeights().Res4K; got != 1000 {
		t.Fatalf("nil profile: Res4K = %d, want 1000", got)
	}

	cfg.Movies = &MovieWeights{Res4K: 0, Res1080p: 200}
	if got := cfg.MovieWeights().Res4K; got != 0 {
		t.Fatalf("explicit zero was overwritten: Res4K = %d, want 0", got)
	}

	var tvCfg QualityScoringConfig
	if got := tvCfg.TVWeights().Res4K; got != 1000 {
		t.Fatalf("nil TV profile: Res4K = %d, want 1000", got)
	}

	tvCfg.TV = &TVWeights{Res4K: 0, Res1080p: 200}
	if got := tvCfg.TVWeights().Res4K; got != 0 {
		t.Fatalf("explicit zero was overwritten in TV profile: Res4K = %d, want 0", got)
	}
}
