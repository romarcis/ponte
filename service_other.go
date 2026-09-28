//go:build !windows

package main

import "errors"

// The Ponte service exists only on Windows, for its protected desktop.

func serviceCommand([]string) bool { return false }
func serviceState() string         { return "" }
func setService(bool) error        { return errors.New("non supportato") }
