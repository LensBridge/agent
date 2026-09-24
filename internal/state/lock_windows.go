//go:build windows

package state

func lockBlocking(string) (func(), error) { return func() {}, nil }
