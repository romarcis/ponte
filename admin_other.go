//go:build !windows

package main

import "errors"

func adminStartEnabled() bool        { return false }
func isElevated() bool               { return false }
func setAdminStart(bool) error       { return errors.New("non supportato") }
func adminStartCommand(string) error { return nil }
func restartAsAdmin() error          { return errors.New("non supportato") }
