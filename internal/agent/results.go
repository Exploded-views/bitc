//go:build linux

package agent

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Results is /run/bitc/<job>/ on container disk (root, 0700; .part then rename).
type Results struct {
	Dir string

	mu         sync.Mutex
	k          []byte
	job, nonce string
	seq        int64
}

func NewResults(root, job string) (*Results, error) {
	dir := filepath.Join(root, job)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Results{Dir: dir}, unix.Chmod(dir, 0o700)
}

func (r *Results) Create(name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(r.Dir, name+".part"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

func (r *Results) Commit(name string) error {
	return os.Rename(filepath.Join(r.Dir, name+".part"), filepath.Join(r.Dir, name))
}

// Bind gives WriteState the key, job id and attempt nonce it signs with.
func (r *Results) Bind(k []byte, job, nonce string) {
	r.mu.Lock()
	r.k, r.job, r.nonce = k, job, nonce
	r.mu.Unlock()
}

// WriteState writes state.json: {phase, exit, job, nonce, seq, ts, mac}; the MAC is under K
// (domain "bitc-state-v1") and seq grows with every write. An unbound Results refuses.
func (r *Results) WriteState(phase string, exit *int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.k) == 0 {
		return errors.New("state: no key bound")
	}
	r.seq++
	b, _ := json.Marshal(SignState(r.k, State{Phase: phase, Exit: exit, Job: r.job, Nonce: r.nonce,
		Seq: r.seq, TS: time.Now().Unix()}))
	f, err := r.Create("state.json")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	f.Close()
	if err != nil {
		return err
	}
	return r.Commit("state.json")
}

func fileSum(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

// Finish commits out.log and err.log, packs artifacts and writes the MACed result.json.
func (r *Results) Finish(k []byte, exit int, workdir string, artifacts []string) (ResultFile, error) {
	for _, n := range []string{"out.log", "err.log"} {
		if _, err := os.Stat(filepath.Join(r.Dir, n+".part")); err == nil {
			if err := r.Commit(n); err != nil {
				return ResultFile{}, err
			}
		}
	}
	a, err := r.Create("artifacts.tar.gz")
	if err != nil {
		return ResultFile{}, err
	}
	if err := PackArtifacts(workdir, artifacts, a); err != nil {
		a.Close()
		return ResultFile{}, err
	}
	a.Close()
	if err := r.Commit("artifacts.tar.gz"); err != nil {
		return ResultFile{}, err
	}
	rf := ResultFile{Exit: exit}
	if rf.OutBytes, rf.OutSHA256, err = fileSum(filepath.Join(r.Dir, "out.log")); err != nil {
		return rf, err
	}
	if rf.ErrBytes, rf.ErrSHA256, err = fileSum(filepath.Join(r.Dir, "err.log")); err != nil {
		return rf, err
	}
	if _, rf.ArtifactsSHA256, err = fileSum(filepath.Join(r.Dir, "artifacts.tar.gz")); err != nil {
		return rf, err
	}
	rf = Sign(k, rf)
	b, _ := json.Marshal(rf)
	f, err := r.Create("result.json")
	if err != nil {
		return rf, err
	}
	_, err = f.Write(b)
	f.Close()
	if err != nil {
		return rf, err
	}
	return rf, r.Commit("result.json")
}

// PackArtifacts tars regular files and dirs under the declared paths that exist (symlinks are
// neither followed nor stored); an empty archive when none exist (spec 8.4 step 6).
func PackArtifacts(workdir string, paths []string, w io.Writer) error {
	rootDir, err := os.OpenRoot(workdir)
	if err != nil {
		return err
	}
	defer rootDir.Close()
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		root := filepath.Join(workdir, filepath.Clean("/"+p))
		if st, err := os.Lstat(root); err != nil || st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() && !info.IsDir() {
				return nil // symlinks, devices, sockets: skipped
			}
			rel, _ := filepath.Rel(workdir, path)
			hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
			if info.IsDir() {
				hdr.Typeflag, hdr.Name = tar.TypeDir, hdr.Name+"/"
			} else {
				hdr.Typeflag, hdr.Size = tar.TypeReg, info.Size()
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, err := rootDir.Open(rel)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if err != nil && !strings.Contains(err.Error(), "no such file") {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
