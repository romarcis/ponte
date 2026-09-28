//go:build !windows

package main

import "errors"

func isElevated() bool             { return false }
func adminCommand(string) error    { return nil }
func startAsAdmin(*App, bool) bool { return false }
func grantAdmin() error            { return errors.New("non supportato") }
func removeAdmin() error           { return nil }
func setupAsAdmin() bool           { return false }
func relaunchAsAdmin(bool) error   { return errors.New("non supportato") }
