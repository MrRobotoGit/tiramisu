//go:build windows

package config

func fileOwner(string) (int, bool) { return 0, false }
