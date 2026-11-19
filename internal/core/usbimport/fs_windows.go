//go:build windows

package usbimport

import (
	"fmt"
	"os"
)

// The USB helper only runs on Linux; these keep the package building on a
// Windows dev box.

func openRegularNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return os.Open(path)
}

func chownToDirOwner(string, string) {}
