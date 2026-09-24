//go:build windows

package selfupdate

import (
	"fmt"
	"os"
)

// The self-updater only runs on Linux; this keeps the package building on a
// Windows dev box.
func openRegularNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.Open(path)
}
