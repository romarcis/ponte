//go:build !windows

package main

// ponytail: circles only on Windows; Linux needs an X11/Wayland overlay window.
func showRipple(x, y int, color uint32) {}
