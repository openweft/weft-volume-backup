// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-volumes/oci/registry"
)

// fakeOCI is a tiny in-memory OCI Distribution v2 server: per-repo tag→manifest
// storage with tags/list, manifest GET/PUT, and manifest DELETE — enough to
// drive ociRegistry and the Controller end-to-end. It is intentionally simpler
// than the registry package's own emulator (no blobs/uploads), covering only the
// surface this package uses.
type fakeOCI struct {
	mu sync.Mutex
	// repo -> (tag -> manifest body)
	manifests map[string]map[string][]byte

	failList   string // repo whose tags/list returns 500
	failDelete string // repo whose DELETE returns 500
	failGet    string // repo whose manifest GET returns 500
}

func newFakeOCI() *fakeOCI {
	return &fakeOCI{manifests: map[string]map[string][]byte{}}
}

// put stores a manifest body under repo/tag directly (test setup helper).
func (f *fakeOCI) put(repo, tag string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifests[repo] == nil {
		f.manifests[repo] = map[string][]byte{}
	}
	f.manifests[repo][tag] = body
}

// repoOf extracts the repo and trailing segment from a /v2/<repo>/<kind>/<ref>
// path. kind is "tags" or "manifests".
func repoAndRef(path, kind string) (repo, ref string, ok bool) {
	// path: /v2/<repo...>/<kind>/<ref>
	const prefix = "/v2/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	marker := "/" + kind + "/"
	i := strings.Index(rest, marker)
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(marker):], true
}

func (f *fakeOCI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			f.handleTags(w, r)
		case strings.Contains(r.URL.Path, "/manifests/"):
			f.handleManifest(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeOCI) handleTags(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
	if repo == f.failList {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":[{"code":"DENIED"}]}`)
		return
	}
	f.mu.Lock()
	var tags []string
	for tag := range f.manifests[repo] {
		tags = append(tags, tag)
	}
	f.mu.Unlock()
	sort.Strings(tags)
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: repo, Tags: tags})
	w.Write(body)
}

func (f *fakeOCI) handleManifest(w http.ResponseWriter, r *http.Request) {
	repo, ref, ok := repoAndRef(r.URL.Path, "manifests")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.put(repo, ref, body)
		w.Header().Set("Docker-Content-Digest", registry.Digest(body))
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		if repo == f.failGet {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors":[{"code":"DENIED"}]}`)
			return
		}
		f.mu.Lock()
		body, present := f.manifests[repo][ref]
		f.mu.Unlock()
		if !present {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			return
		}
		w.Header().Set("Content-Type", registry.MediaTypeManifest)
		w.Write(body)
	case http.MethodDelete:
		if repo == f.failDelete {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors":[{"code":"DENIED"}]}`)
			return
		}
		f.mu.Lock()
		_, present := f.manifests[repo][ref]
		delete(f.manifests[repo], ref)
		f.mu.Unlock()
		if !present {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

// startFakeOCI spins up the server and returns it with a ClientFor bound to it.
func startFakeOCI(t *testing.T) (*fakeOCI, ClientFor, func()) {
	t.Helper()
	f := newFakeOCI()
	srv := httptest.NewServer(f.handler())
	clientFor := func(repo string) *registry.Client {
		return &registry.Client{BaseURL: srv.URL, Repository: repo}
	}
	return f, clientFor, srv.Close
}
