package main

// Image rootfs cache. Pulling + flattening a large image (AIO is 3.4GB, ~3 min)
// on every /run and /restore is the dominant latency. We cache the FLATTENED
// rootfs on disk per image DIGEST — substrate only caches the pulled tarball in
// memory and re-untars per actor (it has a TODO for exactly this disk cache).
//
// Layout: <work>/imgcache/<digest>/rootfs      (the flattened image, built once)
//         <work>/imgcache/<digest>/config.json (the image run config)
// A per-sandbox rootfs is a HARDLINK copy (cp -al) of the cached rootfs — near
// instant, no data duplication. Safe because runsc -overlay2=root:self does
// copy-up: writes land in the sandbox's overlay filestore and never mutate the
// shared cached (lower) inodes.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// per-digest build locks so concurrent runs of the same image build the cache once.
var cacheLocks sync.Map // digest -> *sync.Mutex

func digestLock(d string) *sync.Mutex {
	m, _ := cacheLocks.LoadOrStore(d, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// prepareRootfsCached ensures the flattened rootfs for ref is in the cache, then
// hardlink-copies it into destRootfs. Returns the image run config. workDir is
// sandboxd's work dir (SANDBOXD_WORK).
func prepareRootfsCached(workDir, ref, destRootfs string) (*imageConfig, error) {
	ic, cacheDir, err := ensureCached(workDir, ref)
	if err != nil {
		return nil, err
	}
	// hardlink-copy cache/rootfs -> destRootfs
	if err := os.RemoveAll(destRootfs); err != nil {
		return nil, err
	}
	if err := hardlinkCopy(filepath.Join(cacheDir, "rootfs"), destRootfs); err != nil {
		return nil, fmt.Errorf("hardlink copy from cache: %w", err)
	}
	return ic, nil
}

// ensureCached pulls+flattens ref into <work>/imgcache/<digest> exactly once and
// returns the image config + cache dir. Subsequent calls are a fast metadata read.
func ensureCached(workDir, ref string) (*imageConfig, string, error) {
	// Resolve the digest WITHOUT pulling layers (cheap HEAD on the manifest).
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, "", fmt.Errorf("parse ref %q: %w", ref, err)
	}
	img, err := remote.Image(r, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(context.Background()))
	if err != nil {
		return nil, "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	dig, err := img.Digest()
	if err != nil {
		return nil, "", err
	}
	digest := strings.ReplaceAll(dig.String(), ":", "_")
	cacheDir := filepath.Join(workDir, "imgcache", digest)

	lk := digestLock(digest)
	lk.Lock()
	defer lk.Unlock()

	cfgPath := filepath.Join(cacheDir, "config.json")
	donePath := filepath.Join(cacheDir, ".done")
	if _, err := os.Stat(donePath); err == nil {
		// cache hit — read the stored image config
		var ic imageConfig
		if b, err := os.ReadFile(cfgPath); err == nil {
			if json.Unmarshal(b, &ic) == nil {
				return &ic, cacheDir, nil
			}
		}
		// fall through to rebuild if config unreadable
	}

	// cache miss — build it. Extract into a tmp dir then atomically rename.
	// Retry the pull/extract: a 3.4GB single-stream fetch (AIO) can hit transient
	// "connection reset by peer" from the registry CDN.
	tmp := cacheDir + ".tmp"
	rootfs := filepath.Join(tmp, "rootfs")
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		os.RemoveAll(tmp)
		if err := os.MkdirAll(rootfs, 0o755); err != nil {
			return nil, "", err
		}
		if err := extractImage(img, rootfs); err != nil {
			lastErr = err
			fmt.Fprintf(os.Stderr, "[cache] extract %s attempt %d failed: %v\n", ref, attempt, err)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		os.RemoveAll(tmp)
		return nil, "", fmt.Errorf("extract rootfs (after retries): %w", lastErr)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		os.RemoveAll(tmp)
		return nil, "", err
	}
	c := cf.Config
	ic := &imageConfig{
		Entrypoint: c.Entrypoint, Cmd: c.Cmd, Env: c.Env,
		WorkingDir: firstNonEmpty(c.WorkingDir, "/"), User: c.User,
		Ref: ref, Digest: dig.String(),
	}
	b, _ := json.MarshalIndent(ic, "", "  ")
	os.WriteFile(filepath.Join(tmp, "config.json"), b, 0o644)
	os.WriteFile(filepath.Join(tmp, ".done"), []byte("1"), 0o644)
	os.RemoveAll(cacheDir)
	if err := os.Rename(tmp, cacheDir); err != nil {
		os.RemoveAll(tmp)
		return nil, "", fmt.Errorf("commit cache: %w", err)
	}
	return ic, cacheDir, nil
}

// hardlinkCopy recreates the directory tree of src at dst, hardlinking regular
// files (shared inodes, ~instant) and recreating dirs/symlinks. Equivalent to
// `cp -al`. Writes are safe because the consumer overlays the tree copy-up.
func hardlinkCopy(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm())
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(link, target)
		default:
			// regular (and special) files: hardlink; fall back to copy on EXDEV.
			os.Remove(target)
			if err := os.Link(p, target); err != nil {
				return copyFile(p, target, fi)
			}
			return nil
		}
	})
}

func copyFile(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
