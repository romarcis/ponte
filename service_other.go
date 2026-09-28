//go:build !windows

package main

// The Ponte service exists only on Windows, for its protected desktop.

func serviceCommand([]string) bool { return false }
