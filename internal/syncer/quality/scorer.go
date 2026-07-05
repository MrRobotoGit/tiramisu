package quality

import (
	"regexp"
)

// Weights defines scoring weights for movie selection.
type Weights struct {
	Res4K           int `json:"res_4k"`
	Res1080p        int `json:"res_1080p"`
	HDR             int `json:"hdr"`
	DolbyVision     int `json:"dolby_vision"`
	HDR10Plus       int `json:"hdr10_plus"`
	Atmos           int `json:"atmos"`
	Audio51         int `json:"audio_5_1"`
	Stereo          int `json:"stereo"`
	BluRay          int `json:"bluray"`
	SeederBonus     int `json:"seeder_bonus"`
	SeederThreshold int `json:"seeder_threshold"`
}

// Profile combines weights with TV-specific bonuses.
type Profile struct {
	Weights       Weights
	FullpackBonus int `json:"fullpack_bonus"`
}

var (
	re4K        = regexp.MustCompile(`(?i)(4k|2160p|uhd)`)
	re1080p     = regexp.MustCompile(`(?i)(1080p)`)
	reHDR       = regexp.MustCompile(`(?i)\bHDR\b`)
	reDV        = regexp.MustCompile(`(?i)(dolby.?vision|\bdv\b)`)
	reHDR10Plus = regexp.MustCompile(`(?i)(hdr10\+|hdr10plus)`)
	reAtmos     = regexp.MustCompile(`(?i)(atmos)`)
	re51        = regexp.MustCompile(`(?i)(5\.1|dts|ddp5|ddp|dd\+|eac3|ac3)`)
	reStereo    = regexp.MustCompile(`(?i)(stereo|aac|mp3|2\.0)`)
	reBluRay    = regexp.MustCompile(`(?i)(bluray|blu.?ray|bdrip|bdremux|remux)`)

	reFullpackS      = regexp.MustCompile(`(?i)s\d{2}\s*(complete|pack|full)`)
	reFullpackSeason = regexp.MustCompile(`(?i)season\s*\d+\s*(complete|pack|full)`)
)

// DefaultMovieProfile returns the default scoring profile for movies.
func DefaultMovieProfile() Profile {
	return Profile{
		Weights: Weights{
			Res4K:           200,
			Res1080p:        50,
			HDR:             100,
			DolbyVision:     150,
			HDR10Plus:       100,
			Atmos:           50,
			Audio51:         25,
			Stereo:          -50,
			BluRay:          10,
			SeederBonus:     5,
			SeederThreshold: 50,
		},
	}
}

// Score calculates a quality score for a title based on its metadata and seeders.
func Score(title string, seeders int, profile Profile) int {
	w := profile.Weights
	score := 0

	if re4K.MatchString(title) {
		score += w.Res4K
	} else if re1080p.MatchString(title) {
		score += w.Res1080p
	}

	if reDV.MatchString(title) {
		score += w.DolbyVision
	}
	if reHDR10Plus.MatchString(title) {
		score += w.HDR10Plus
	} else if reHDR.MatchString(title) {
		score += w.HDR
	}

	if reAtmos.MatchString(title) {
		score += w.Atmos
	}
	if re51.MatchString(title) {
		score += w.Audio51
	}
	if reStereo.MatchString(title) && !re51.MatchString(title) && !reAtmos.MatchString(title) {
		score += w.Stereo
	}

	if reBluRay.MatchString(title) {
		score += w.BluRay
	}

	if seeders > w.SeederThreshold {
		score += w.SeederBonus
	}

	return score
}
