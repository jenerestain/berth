// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// byPath finds a scan result by the path it names, which is the one field a
// test can always name up front.
func byPath(ms []Model, path string) (Model, bool) {
	for _, m := range ms {
		if m.Path == path {
			return m, true
		}
	}
	return Model{}, false
}

// byName finds a scan result by its listing name, which is the file's stem.
func byName(ms []Model, name string) (Model, bool) {
	for _, m := range ms {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

// byTitle finds a scan result by the display name its header gave it. A model's
// quants share one general.name, so a test that means "the file called X" uses
// byName and only a test about the header uses this.
func byTitle(ms []Model, title string) (Model, bool) {
	for _, m := range ms {
		if m.Title == title {
			return m, true
		}
	}
	return Model{}, false
}

// sampleFile is the library's smallest legitimate member: one GGUF with a name
// of its own.
func sampleFile(t *testing.T, path, name string) int {
	t.Helper()
	data := buildGGUF(t, 3, []kv{
		{"general.architecture", "llama"},
		{"general.name", name},
		{"general.file_type", uint32(15)},
		{"llama.context_length", uint32(8192)},
		{"llama.block_count", uint32(2)},
	}, []tensor{{"a", []uint64{4096, 4096}, 1}})
	writeGGUF(t, path, data)
	return len(data)
}

func TestScanDir(t *testing.T) {
	dir := t.TempDir()
	sampleFile(t, filepath.Join(dir, "plain.gguf"), "Plain Model")
	size := sampleFile(t, filepath.Join(dir, "deep", "split-00001-of-00002.gguf"), "Split Model")
	sampleFile(t, filepath.Join(dir, "deep", "split-00002-of-00002.gguf"), "Split Model")
	sampleFile(t, filepath.Join(dir, "gone-00001-of-00003.gguf"), "Gone Model")
	sampleFile(t, filepath.Join(dir, "gone-00002-of-00003.gguf"), "Gone Model")
	// A download in progress and a config with no weights beside it are not
	// models; neither is a stray file the scan has no reader for.
	writeGGUF(t, filepath.Join(dir, "half.gguf.part"), []byte("half a file"))
	writeGGUF(t, filepath.Join(dir, "notes.txt"), []byte("not a model"))
	st := filepath.Join(dir, "st")
	writeGGUF(t, filepath.Join(st, "model-00001-of-00002.safetensors"), []byte(strings.Repeat("w", 1000)))
	writeGGUF(t, filepath.Join(st, "model-00002-of-00002.safetensors"), []byte(strings.Repeat("w", 500)))
	if err := os.WriteFile(filepath.Join(st, "config.json"), []byte(`{"model_type":"qwen2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGGUF(t, filepath.Join(dir, "lonely", "config.json"), []byte(`{"model_type":"llama"}`))

	ms := Scan([]string{dir})
	if len(ms) != 4 {
		t.Fatalf("scan found %d models, want 4: %s", len(ms), dump(ms))
	}

	plain, ok := byTitle(ms, "Plain Model")
	if !ok {
		t.Fatalf("no Plain Model in %s", dump(ms))
	}
	if plain.Parts != 0 || len(plain.Files) != 1 || plain.Format != FormatGGUF || plain.Source != SourceDir {
		t.Errorf("plain = parts %d, %d files, %s/%s", plain.Parts, len(plain.Files), plain.Format, plain.Source)
	}
	if plain.Context != 8192 || plain.Quant != "Q4_K_M" || plain.Params != 4096*4096 {
		t.Errorf("plain = ctx %d quant %q params %d", plain.Context, plain.Quant, plain.Params)
	}
	if plain.Bytes == 0 || plain.ModTime.IsZero() {
		t.Errorf("plain has no size or date: %+v", plain)
	}

	split, ok := byTitle(ms, "Split Model")
	if !ok {
		t.Fatalf("no Split Model in %s", dump(ms))
	}
	if split.Parts != 2 || len(split.Files) != 2 {
		t.Errorf("split = parts %d, %d files, want 2 and 2", split.Parts, len(split.Files))
	}
	if split.Bytes != uint64(2*size) {
		t.Errorf("split bytes = %d, want both parts (%d each)", split.Bytes, size)
	}
	if split.Err != "" {
		t.Errorf("split error = %q, want none", split.Err)
	}
	if !strings.HasSuffix(split.Files[0], "-00001-of-00002.gguf") {
		t.Errorf("split part order = %v, want part 1 first", split.Files)
	}

	gone, ok := byTitle(ms, "Gone Model")
	if !ok {
		t.Fatalf("no Gone Model in %s", dump(ms))
	}
	if gone.Err != "missing part 3" {
		t.Errorf("gone error = %q, want %q", gone.Err, "missing part 3")
	}
	if gone.Parts != 3 || len(gone.Files) != 2 {
		t.Errorf("gone = parts %d, %d files, want 3 and 2", gone.Parts, len(gone.Files))
	}
	// What is there is still listed: the header of part 1 says what the model
	// is even when part 3 is still downloading.
	if gone.Arch != "llama" {
		t.Errorf("gone arch = %q, want llama", gone.Arch)
	}

	tensors, ok := byPath(ms, st)
	if !ok {
		t.Fatalf("no safetensors model in %s", dump(ms))
	}
	if tensors.Format != FormatSafetensors || tensors.Arch != "qwen2" {
		t.Errorf("safetensors = %s arch %q, want safetensors and qwen2", tensors.Format, tensors.Arch)
	}
	if tensors.Bytes != 1500 {
		t.Errorf("safetensors bytes = %d, want the two shards (1500), config.json aside", tensors.Bytes)
	}
	if len(tensors.Files) != 2 {
		t.Errorf("safetensors files = %v, want both shards", tensors.Files)
	}
}

func TestScanHFCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink needs an admin token or Developer Mode, which the CI runner does not run with")
	}
	hub := t.TempDir()
	repo := filepath.Join(hub, "models--org--repo")
	blob := filepath.Join(repo, "blobs", "sha256-"+strings.Repeat("a", 64))
	size := sampleFile(t, blob, "Whatever The Blob Says")
	// Two revisions link the same blob: it is one model's file, not two, and
	// its size is counted once.
	for _, rev := range []string{"rev1", "rev2"} {
		link := filepath.Join(repo, "snapshots", rev, "model.gguf")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(filepath.Dir(link), blob)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, link); err != nil {
			t.Fatal(err)
		}
	}
	// A repo whose snapshots hold nothing is not a model.
	writeGGUF(t, filepath.Join(hub, "models--empty--repo", "refs", "main"), []byte("main"))

	ms := Scan([]string{hub})
	if len(ms) != 1 {
		t.Fatalf("scan found %d models, want 1: %s", len(ms), dump(ms))
	}
	m := ms[0]
	if m.Name != "org/repo" {
		t.Errorf("name = %q, want org/repo", m.Name)
	}
	if m.Source != SourceHFCache || m.Format != FormatGGUF {
		t.Errorf("source/format = %s/%s, want hf-cache/gguf", m.Source, m.Format)
	}
	if m.Bytes != uint64(size) {
		t.Errorf("bytes = %d, want the blob once (%d)", m.Bytes, size)
	}
	if len(m.Files) != 1 {
		t.Errorf("files = %v, want the one blob the two revisions link", m.Files)
	}
	if m.Arch != "llama" || m.Quant != "Q4_K_M" {
		t.Errorf("arch/quant = %s/%s, want llama/Q4_K_M from the blob's header", m.Arch, m.Quant)
	}
}

// A models--* directory with no snapshots is the husk of an abandoned
// download, not one repo of an HF cache. It must not hide the plain models
// around it, and a real cache with a husk beside it is still a cache.
func TestScanHFHusk(t *testing.T) {
	cases := []struct {
		name  string
		setup func(root string)
		want  string // "name/source" for every model, in scan order
	}{
		{
			name: "husk beside a plain store",
			setup: func(root string) {
				sampleFile(t, filepath.Join(root, "plain.gguf"), "Plain Model")
				if err := os.Mkdir(filepath.Join(root, "models--x--y"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "plain/dir",
		},
		{
			name: "a real cache is an HF cache",
			setup: func(root string) {
				sampleFile(t, filepath.Join(root, "models--org--repo", "snapshots", "model.gguf"), "Whatever The Blob Says")
			},
			want: "org/repo/hf-cache",
		},
		{
			name: "husk beside a real cache",
			setup: func(root string) {
				sampleFile(t, filepath.Join(root, "models--org--repo", "snapshots", "model.gguf"), "Whatever The Blob Says")
				if err := os.Mkdir(filepath.Join(root, "models--x--y"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "org/repo/hf-cache",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(root)
			var parts []string
			for _, m := range Scan([]string{root}) {
				parts = append(parts, m.Name+"/"+m.Source)
			}
			if got := strings.Join(parts, ", "); got != tc.want {
				t.Errorf("scan = %s, want %s", got, tc.want)
			}
		})
	}
}

// testManifest is the Ollama tag file: a layer list, of which one layer is the weights.
type testManifest struct {
	MediaType string        `json:"mediaType"`
	Layers    []ollamaLayer `json:"layers"`
}

type ollamaLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

func writeManifest(t *testing.T, path string, layers ...ollamaLayer) {
	t.Helper()
	raw, err := json.Marshal(testManifest{MediaType: "application/vnd.docker.distribution.manifest.v2+json", Layers: layers})
	if err != nil {
		t.Fatal(err)
	}
	writeGGUF(t, path, raw)
}

func TestScanOllama(t *testing.T) {
	store := t.TempDir()
	man := filepath.Join(store, "manifests", "registry.ollama.ai", "library", "qwen3", "latest")
	digest := "sha256:" + strings.Repeat("b", 64)
	blob := filepath.Join(store, "blobs", "sha256-"+strings.Repeat("b", 64))
	size := sampleFile(t, blob, "Qwen3 8B")
	writeManifest(t, man,
		ollamaLayer{MediaType: "application/vnd.ollama.image.config", Digest: "sha256:" + strings.Repeat("c", 64), Size: 40},
		ollamaLayer{MediaType: ollamaModelLayer, Digest: digest, Size: size},
		ollamaLayer{MediaType: "application/vnd.ollama.image.template", Digest: "sha256:" + strings.Repeat("d", 64), Size: 10},
	)
	// A tag whose manifest names a blob that is not here is that tag's problem.
	ghost := filepath.Join(store, "manifests", "registry.ollama.ai", "library", "ghost", "latest")
	writeManifest(t, ghost, ollamaLayer{MediaType: ollamaModelLayer, Digest: "sha256:" + strings.Repeat("e", 64)})
	// And a tag with no weights layer at all is a container, not a model.
	parts := filepath.Join(store, "manifests", "registry.ollama.ai", "library", "parts", "latest")
	writeManifest(t, parts, ollamaLayer{MediaType: "application/vnd.ollama.image.adapter", Digest: "sha256:" + strings.Repeat("f", 64)})

	ms := Scan([]string{store})
	if len(ms) != 3 {
		t.Fatalf("scan found %d models, want 3: %s", len(ms), dump(ms))
	}
	m, ok := byName(ms, "ollama:qwen3:latest")
	if !ok {
		t.Fatalf("no ollama:qwen3:latest in %s", dump(ms))
	}
	if m.Format != FormatOllama || m.Source != SourceOllama {
		t.Errorf("format/source = %s/%s, want ollama/ollama", m.Format, m.Source)
	}
	if m.Bytes != uint64(size) {
		t.Errorf("bytes = %d, want the blob's %d", m.Bytes, size)
	}
	if m.Path != blob {
		t.Errorf("path = %q, want the blob %q", m.Path, blob)
	}
	if m.Arch != "llama" || m.Name == "Qwen3 8B" {
		t.Errorf("arch = %q, name = %q: the tag names the model, the blob its arch", m.Arch, m.Name)
	}
	ghostModel, ok := byName(ms, "ollama:ghost:latest")
	if !ok {
		t.Fatalf("the missing blob dropped its tag: %s", dump(ms))
	}
	if ghostModel.Err == "" {
		t.Error("a manifest naming a blob that is not there reported no error")
	}
	partsModel, ok := byName(ms, "ollama:parts:latest")
	if !ok {
		t.Fatalf("the weights-less tag dropped: %s", dump(ms))
	}
	if !strings.Contains(partsModel.Err, ollamaModelLayer) {
		t.Errorf("error = %q, want it to name the missing layer type", partsModel.Err)
	}
}

func TestScanMissingRoot(t *testing.T) {
	dir := t.TempDir()
	sampleFile(t, filepath.Join(dir, "plain.gguf"), "Plain Model")
	// A root that is not there is reported by MissingRoots, and the roots that
	// are there still read: a mistyped path never empties the listing.
	ms := Scan([]string{dir, filepath.Join(dir, "nope")})
	if len(ms) != 1 {
		t.Fatalf("scan found %d models, want 1: %s", len(ms), dump(ms))
	}
	if _, ok := byTitle(ms, "Plain Model"); !ok {
		t.Errorf("the root that exists was skipped: %s", dump(ms))
	}
	missing := MissingRoots([]string{dir, filepath.Join(dir, "nope")})
	if len(missing) != 1 || !strings.HasSuffix(missing[0], "nope") {
		t.Errorf("MissingRoots = %v, want the one that is not there", missing)
	}
	// When every named root is missing there is nothing to list but the fact.
	ms = Scan([]string{filepath.Join(dir, "nope")})
	if len(ms) != 1 || ms[0].Err == "" {
		t.Fatalf("scan = %+v, want one model whose error is the missing directory", ms)
	}
}

func TestScanDefaultRootsExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		// os.UserHomeDir reads USERPROFILE there.
		t.Setenv("USERPROFILE", home)
	}
	if got := DefaultRoots(); len(got) != 0 {
		t.Errorf("DefaultRoots = %v over an empty home, want none", got)
	}
	// ~/models is a root the moment it exists.
	models := filepath.Join(home, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DefaultRoots(); len(got) != 1 || got[0] != models {
		t.Errorf("DefaultRoots = %v, want %s", got, models)
	}
	ms := Scan(nil)
	if len(ms) != 0 {
		t.Errorf("scan of an empty ~/models found %s", dump(ms))
	}
	// Ollama's store is found by its layout, wherever OLLAMA_MODELS points.
	store := filepath.Join(home, "ollama-store")
	man := filepath.Join(store, "manifests", "registry.ollama.ai", "library", "x", "t")
	writeManifest(t, man, ollamaLayer{MediaType: ollamaModelLayer, Digest: "sha256:" + strings.Repeat("1", 64)})
	writeGGUF(t, filepath.Join(store, "blobs", "sha256-"+strings.Repeat("1", 64)), buildGGUF(t, 3, nil, nil))
	t.Setenv("OLLAMA_MODELS", store)
	ms = Scan(nil)
	if len(ms) != 1 {
		t.Fatalf("$OLLAMA_MODELS was not scanned: %s", dump(ms))
	}
	if ms[0].Name != "ollama:x:t" || ms[0].Source != SourceOllama {
		t.Errorf("scan = %+v, want ollama:x:t from an ollama root", ms[0])
	}
}

func TestScanEmptyDir(t *testing.T) {
	if ms := Scan([]string{t.TempDir()}); len(ms) != 0 {
		t.Errorf("an empty directory holds %s", dump(ms))
	}
}

func TestDuplicates(t *testing.T) {
	groups := func(ms []Model) string {
		var parts []string
		for _, g := range Duplicates(ms) {
			var names []string
			for _, m := range g {
				names = append(names, m.Name)
			}
			parts = append(parts, strings.Join(names, "+"))
		}
		return strings.Join(parts, ", ")
	}
	a := Model{Name: "a", Format: FormatGGUF, Bytes: 100, Quant: "Q4_K_M", Arch: "llama"}
	same := Model{Name: "b", Format: FormatGGUF, Bytes: 100, Quant: "Q4_K_M", Arch: "llama"}
	otherQuant := Model{Name: "c", Format: FormatGGUF, Bytes: 100, Quant: "Q8_0", Arch: "llama"}
	otherBytes := Model{Name: "d", Format: FormatGGUF, Bytes: 101, Quant: "Q4_K_M", Arch: "llama"}
	broken := Model{Name: "e", Format: FormatGGUF, Quant: "Q4_K_M", Arch: "llama"} // no size, so it matches nothing
	if got := groups([]Model{a, same, otherQuant, otherBytes, broken}); got != "a+b" {
		t.Errorf("duplicates = %q, want only a+b", got)
	}
	if got := groups([]Model{a, otherQuant, otherBytes}); got != "" {
		t.Errorf("duplicates = %q, want none", got)
	}
}

// dump is a scan's answer in one line, for a failure that has to be read.
func dump(ms []Model) string {
	var b strings.Builder
	for i, m := range ms {
		if i > 0 {
			fmt.Fprint(&b, "; ")
		}
		fmt.Fprintf(&b, "%s (%s/%s, %d bytes, err %q)", m.Name, m.Format, m.Source, m.Bytes, m.Err)
	}
	return b.String()
}

// A repo that keeps its MTP heads in MTP/ and its projector in mmproj/: the
// adapters belong to the models one folder up, and a 1.9B "mtp-" file there is
// a draft, never a model of its own.
func TestCompanionsInSubfolders(t *testing.T) {
	ms := []Model{
		{Name: "Distill-Compact", Title: "Distill", Path: "/r/Distill-Compact.gguf", Role: RoleModel, Params: 34_700_000_000},
		{Name: "Distill-Mini", Title: "Distill", Path: "/r/Distill-Mini.gguf", Role: RoleModel, Params: 34_700_000_000},
		{Name: "mtp-Distill-Q8_0", Title: "Distill", Path: "/r/MTP/mtp-Distill-Q8_0.gguf", Role: RoleModel, Params: 1_900_000_000},
		{Name: "mmproj-F16", Path: "/r/mmproj/mmproj-F16.gguf", Role: RoleMMProj, Params: 446_000_000},
	}
	AttachCompanions(ms)
	if ms[2].Role != RoleDraft {
		t.Fatalf("mtp- head in MTP/ has role %q, want draft", ms[2].Role)
	}
	for _, i := range []int{0, 1} {
		if len(ms[i].Companions) != 2 {
			t.Fatalf("%s companions = %v, want the draft and the projector", ms[i].Name, ms[i].Companions)
		}
	}
}

// A 27B model with an MTP head trained in keeps its place as a model.
func TestMergedMTPModelStaysAModel(t *testing.T) {
	ms := []Model{{Name: "Q-27B-MTP-IQ4_XS", Path: "/s/Q-27B-MTP-IQ4_XS.gguf", Role: RoleModel, Params: 27_300_000_000}}
	AttachCompanions(ms)
	if ms[0].Role != RoleModel {
		t.Fatalf("a full model named MTP became %q", ms[0].Role)
	}
}
