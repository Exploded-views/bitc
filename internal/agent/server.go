//go:build linux

package agent

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ResultFiles are the only names the results server serves.
var ResultFiles = map[string]bool{"state.json": true, "out.log": true, "err.log": true, "artifacts.tar.gz": true, "result.json": true}

// liveLogs are the results served from their .part while the job runs (spec 8.4, 8.7): a caller
// tails them by Range. result.json, state.json and the artifacts are served only once committed.
var liveLogs = map[string]bool{"out.log": true, "err.log": true}

// NewServer serves GET|HEAD /v1/<name> with Range, only with Authorization: Bearer <hex B>;
// anything else is 404 without a body, other methods 405 (spec 8.4).
func NewServer(dir string, bearer []byte) (http.Handler, error) {
	if len(bearer) != 32 || bytes.Equal(bearer, make([]byte, 32)) {
		return nil, errors.New("the results server needs a 256-bit bearer")
	}
	want := []byte("Bearer " + hex.EncodeToString(bearer))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/v1/")
		got := []byte(r.Header.Get("Authorization"))
		if !ok || !ResultFiles[name] || subtle.ConstantTimeCompare(got, want) != 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fd := -1
		var err error
		if liveLogs[name] {
			// out.log and err.log are served while they grow: the .part first, then the committed
			// name (a rename between the two opens leaves the committed one). The other files
			// appear only once committed.
			fd, err = unix.Open(filepath.Join(dir, name+".part"), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if fd < 0 {
			fd, err = unix.Open(filepath.Join(dir, name), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f := os.NewFile(uintptr(fd), name)
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, "", st.ModTime(), f)
	}), nil
}
