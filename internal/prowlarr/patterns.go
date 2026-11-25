package prowlarr

import "regexp"

var (
	garbageRe = regexp.MustCompile(`(?i)camrip|hdcam|hdts|telesync|\bts\b|telecine|\btc\b|\bscr\b|screener|webscreener`)
	res4kRe   = regexp.MustCompile(`(?i)2160p|4k|uhd`)
	res1080Re = regexp.MustCompile(`(?i)1080p`)
	res720Re  = regexp.MustCompile(`(?i)720p`)
	reHexHash = regexp.MustCompile(`^[A-Fa-f0-9]{40}$`)
)
