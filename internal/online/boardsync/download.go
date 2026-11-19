package boardsync

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/online/devauth"
)

// maxPackage is the largest package the board will download.
const maxPackage = mbu.MaxPackageBytes

var errTooLarge = fmt.Errorf("the download is larger than %d MiB, the most a package may be", maxPackage>>20)

func signRequest(req *http.Request, body []byte, deviceID string, key ed25519.PrivateKey, now time.Time) {
	devauth.Sign(req, body, deviceID, key, now)
}

// saveCapped streams r into a new file at path, reading at most limit bytes;
// anything longer is errTooLarge and the file is removed. When h is not nil
// every byte is also hashed into it.
func saveCapped(path string, r io.Reader, limit int64, h hash.Hash) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, err
	}
	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}
	// One byte past the limit tells "exactly at the limit" from "too big".
	n, err := io.Copy(w, io.LimitReader(r, limit+1))
	if err == nil && n > limit {
		err = errTooLarge
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		if errors.Is(err, errTooLarge) {
			return n, errTooLarge
		}
		return n, err
	}
	return n, nil
}

func jitter(b time.Duration) time.Duration {
	if b < 4 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(b / 4)))
}

func removeAll(p string) { _ = os.RemoveAll(p) }
