//go:build !darwin && !linux && !windows

package app

func localProjectVolume(path string) error { return nil }
