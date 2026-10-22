// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The formats a scan recognises, and the kinds of place it found one in.
const (
	FormatGGUF        = "gguf"
	FormatSafetensors = "safetensors"
	FormatOllama      = "ollama"

	SourceDir      = "dir"
	SourceHFCache  = "hf-cache"
	SourceLMStudio = "lmstudio"
	SourceOllama   = "ollama"
)

// ollamaModelLayer is the manifest layer that holds the weights; the others are
// the config, the template and the adapters wrapped around them.
const ollamaModelLayer = "application/vnd.ollama.image.model"

// Model is one model on disk: a GGUF or its parts, a directory of safetensors,
// an Ollama tag. Bytes is what the file occupies here, which is what a lease
// has to fit.
//
// Name is the file's stem, which is the only name that is never shared: a
// model's quants and its vision adapter all answer to the same general.name,
// and that one is Title.
type Model struct {
	Name       string    `json:"name" yaml:"name"`
	Title      string    `json:"title,omitempty" yaml:"title,omitempty"` // general.name / the Ollama tag
	Path       string    `json:"path" yaml:"path"`
	Files      []string  `json:"files,omitempty" yaml:"files,omitempty"`           // every part of a split GGUF
	Companions []string  `json:"companions,omitempty" yaml:"companions,omitempty"` // mmproj/drafts that go with it
	Bytes      uint64    `json:"bytes" yaml:"bytes"`
	Format     string    `json:"format" yaml:"format"`                   // gguf | safetensors | ollama
	Source     string    `json:"source" yaml:"source"`                   // dir | hf-cache | lmstudio | ollama
	Shelf      Shelf     `json:"shelf,omitempty" yaml:"shelf,omitempty"` // chat | image | audio; empty = undetermined
	Role       string    `json:"role,omitempty" yaml:"role,omitempty"`   // model | mmproj | draft | embedding
	Arch       string    `json:"arch,omitempty" yaml:"arch,omitempty"`   // model_type for safetensors
	Quant      string    `json:"quant,omitempty" yaml:"quant,omitempty"`
	Params     uint64    `json:"params,omitempty" yaml:"params,omitempty"`
	Context    uint64    `json:"context,omitempty" yaml:"context,omitempty"`
	Layers     uint64    `json:"layers,omitempty" yaml:"layers,omitempty"`
	Parts      int       `json:"parts,omitempty" yaml:"parts,omitempty"` // >1 for a split GGUF
	ModTime    time.Time `json:"mod_time,omitempty" yaml:"mod_time,omitempty"`
	Err        string    `json:"err,omitempty" yaml:"err,omitempty"` // an unreadable model, never a failed scan

	// Usage is what the harbor's request log says about this model, set by
	// MatchUsage when a listing asked for it.
	Usage *ModelUsage `json:"usage,omitempty" yaml:"usage,omitempty"`

	// CachedBytes is how many of the model's files' bytes the kernel page
	// cache holds now, measured by Cache when a listing asked for it. The
	// kernel keeps those pages for the machine as a whole and gives them
	// back when it needs the RAM, so the share says a reload is likely to
	// read from memory, not that the RAM is held.
	CachedBytes uint64 `json:"cached_bytes,omitempty" yaml:"cached_bytes,omitempty"`
	// CachedSize is the size of the files Cache measured for CachedBytes,
	// the share's denominator beside its numerator.
	CachedSize uint64 `json:"cached_size,omitempty" yaml:"cached_size,omitempty"`
}

// root is one place to look and what to call what is found there.
type root struct {
	path, source string
}

// DefaultRoots are the model directories this machine's tools use: the
// Hugging Face cache, LM Studio's two spellings, Ollama's store and ~/models.
// Only the ones that exist are returned, which is what makes the list worth
// printing.
func DefaultRoots() []string {
	var out []string
	for _, r := range wantedRoots() {
		out = append(out, r.path)
	}
	return out
}

// MissingRoots reports which of dirs are not directories, so a listing can warn
// about each and still print what the rest of the library holds.
func MissingRoots(dirs []string) []string {
	_, missing := rootsFor(dirs)
	return missing
}

// wantedRoots is the default root list with the directories that are not there
// dropped: a tool that was never installed has no directory, and that is not a
// warning.
func wantedRoots() []root {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// The environment spells a tool's store out; where it does not, the store is
	// under the home directory. An absolute setting is taken as it stands.
	hub := filepath.Join(home, ".cache", "huggingface", "hub")
	if hf := os.Getenv("HF_HOME"); hf != "" {
		hub = filepath.Join(hf, "hub")
	}
	ollamaModels := filepath.Join(home, ".ollama", "models")
	if om := os.Getenv("OLLAMA_MODELS"); om != "" {
		ollamaModels = om
	}
	var out []root
	add := func(p, source string) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, root{path: p, source: source})
		}
	}
	add(hub, SourceHFCache)
	add(filepath.Join(home, ".lmstudio", "models"), SourceLMStudio)
	add(filepath.Join(home, ".cache", "lm-studio", "models"), SourceLMStudio)
	add(ollamaModels, SourceOllama)
	add(filepath.Join(home, "models"), SourceDir)
	return out
}

// Scan reads every model under dirs, or under the default roots when dirs is
// empty. A model it cannot read comes back with Err set: one half-finished
// download never fails the scan, so the rest of the library still reads.
//
// A directory that was named and is not there is the exception, and comes back
// as one Model carrying that error - typing a path wrong must not read as an
// empty library.
func Scan(dirs []string) []Model {
	roots, missing := rootsFor(dirs)
	if len(roots) == 0 {
		return []Model{{Name: strings.Join(missing, ", "), Err: "no such model directory"}}
	}
	var out []Model
	for _, r := range roots {
		out = append(out, scanRoot(r)...)
	}
	for i := range out {
		out[i].Shelf = ShelfOf(&out[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	AttachCompanions(out)
	return out
}

// Main returns the models that run on their own: everything but another file's
// vision projector or draft. An embedding model is a model - it gets its own
// lease, its own context and its own row.
func Main(ms []Model) []Model {
	var out []Model
	for _, m := range ms {
		if m.Role != RoleMMProj && m.Role != RoleDraft {
			out = append(out, m)
		}
	}
	return out
}

// IsCompanion is a file that only means something next to a model.
func IsCompanion(m Model) bool { return m.Role == RoleMMProj || m.Role == RoleDraft }

// AttachCompanions puts every mmproj and draft on the model it belongs to, and
// only when the directory says which one that is: one main model, or several
// that are the same model under different quants (one general.name), which is
// how a library of quants sits next to its one projector. Anything else is a
// guess about which of two models the adapter goes with, and Berth does not
// guess. AttachCompanions is idempotent, so a listing that filters the
// companions out after attaching them is safe.
func AttachCompanions(ms []Model) {
	refineDrafts(ms)
	mains := map[string][]int{}
	byDir := map[string][]int{}
	for i := range ms {
		if ms[i].Path == "" {
			continue
		}
		dir := filepath.Dir(ms[i].Path)
		byDir[dir] = append(byDir[dir], i)
		// An embedding model is not a candidate owner: a vision projector or a
		// speculative draft never belongs to one, so it creates no ambiguity.
		if !IsCompanion(ms[i]) && ms[i].Role != RoleEmbedding {
			mains[dir] = append(mains[dir], i)
		}
	}
	// A folder of nothing but adapters (a repo's mmproj/ or MTP/ subfolder)
	// belongs to the models one level up.
	for dir, idx := range byDir {
		if len(mains[dir]) > 0 {
			continue
		}
		if up := filepath.Dir(dir); len(mains[up]) > 0 {
			byDir[up] = append(byDir[up], idx...)
			delete(byDir, dir)
		}
	}
	for dir, comps := range byDir {
		owners := companionOwners(ms, mains[dir])
		for _, owner := range owners {
			m := &ms[owner]
			attachCompanions(ms, m, comps)
		}
	}
}

// mtpNameWords name a file that is an MTP-family draft by name alone. A bare
// "mtp" is not one of them: a model with an MTP head keeps the name of the model.
var mtpNameWords = []string{"mtp", "medusa"}

// draftShare is how small a file has to be next to the biggest model in its
// directory to be that model's draft: a head is a fraction of the weights it
// speculates for, and the drafts on a real machine are 5-10% of them.
const draftShare = 4

// refineDrafts gives a file its draft role when only its size says so: an
// "-MTP-" file beside a 27B model that holds 1.9B parameters is that model's
// speculative head, and the same word on a file that is 27B itself is a model
// with the head trained into it. Names alone cannot tell the two apart, and
// hiding a 15 GiB model out of the listing is the worse mistake.
func refineDrafts(ms []Model) {
	biggest := map[string]uint64{}
	for i := range ms {
		if ms[i].Path == "" || IsCompanion(ms[i]) || ms[i].Params == 0 {
			continue
		}
		dir := filepath.Dir(ms[i].Path)
		if ms[i].Params > biggest[dir] {
			biggest[dir] = ms[i].Params
		}
	}
	for i := range ms {
		m := &ms[i]
		if m.Role != RoleModel || m.Params == 0 {
			continue
		}
		name := strings.ToLower(filepath.Base(m.Path))
		hit := false
		for _, w := range mtpNameWords {
			if strings.Contains(name, w) {
				hit = true
			}
		}
		dir := filepath.Dir(m.Path)
		big := biggest[dir]
		if _, own := biggest[dir]; !own || big == m.Params {
			// Alone in its folder (a repo's MTP/ subfolder): the models it
			// speculates for are one level up.
			big = biggest[filepath.Dir(dir)]
		}
		if hit && big > 0 && m.Params*draftShare <= big {
			m.Role = RoleDraft
		}
	}
}

// attachCompanions records one directory's adapters on one model.
func attachCompanions(ms []Model, m *Model, comps []int) {
	for _, i := range comps {
		if !IsCompanion(ms[i]) {
			continue
		}
		if !slices.Contains(m.Companions, ms[i].Path) {
			m.Companions = append(m.Companions, ms[i].Path)
		}
	}
	sort.Strings(m.Companions)
}

// companionOwners is the models the directory's adapters belong to: every main
// model when they are all one model - one general.name, however many quants -
// and none when the directory holds two different models, where which one the
// adapter projects is a guess Berth does not make. Embedding models are not in
// the running: nothing shipped beside them is theirs.
func companionOwners(ms []Model, mains []int) []int {
	if len(mains) == 0 {
		return nil
	}
	if len(mains) == 1 {
		return mains
	}
	name := ms[mains[0]].Title
	if name == "" {
		return nil
	}
	for _, i := range mains[1:] {
		if ms[i].Title != name {
			return nil
		}
	}
	return mains
}

// rootsFor resolves the directories to scan, dropping repeats, and reports the
// ones that are not there.
func rootsFor(dirs []string) (roots []root, missing []string) {
	if len(dirs) == 0 {
		return wantedRoots(), nil
	}
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for _, d := range dirs {
		p := d
		if !filepath.IsAbs(p) {
			p = filepath.Join(home, p)
		}
		p = filepath.Clean(p)
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			missing = append(missing, d)
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		roots = append(roots, root{path: p, source: sourceOf(p)})
	}
	return roots, missing
}

// sourceOf names a root by the layout it holds.
func sourceOf(p string) string {
	if isDir(filepath.Join(p, "manifests")) && isDir(filepath.Join(p, "blobs")) {
		return SourceOllama
	}
	if ents, err := os.ReadDir(p); err == nil {
		for _, e := range ents {
			// The HF scan reads snapshots/, so that is what makes a root an
			// HF cache. A models--* husk with none, from an abandoned
			// download, must not hide the plain models around it.
			if e.IsDir() && strings.HasPrefix(e.Name(), "models--") && isDir(filepath.Join(p, e.Name(), "snapshots")) {
				return SourceHFCache
			}
		}
	}
	return SourceDir
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// scanRoot reads one root, dispatching on the layout it holds.
func scanRoot(r root) []Model {
	switch sourceOf(r.path) {
	case SourceOllama:
		return scanOllama(r)
	case SourceHFCache:
		return scanHFCache(r)
	}
	return scanDir(r)
}

// splitRE matches llama.cpp's split names: model-00001-of-00003.gguf.
var splitRE = regexp.MustCompile(`^(.*)-(\d+)-of-(\d+)\.gguf$`)

// skipFile is a download in progress: a file whose name is not the model's.
func skipFile(name string) bool {
	return strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".incomplete")
}

// ggufKey is the grouping of one GGUF name: the parts of a split model share a
// key, and a whole file is its own group. index is 0 for a whole file.
func ggufKey(name string) (base string, index, want int) {
	if m := splitRE.FindStringSubmatch(name); m != nil {
		n, err1 := strconv.Atoi(m[2])
		total, err2 := strconv.Atoi(m[3])
		if err1 == nil && err2 == nil && n > 0 && total > 1 {
			return m[1], n, total
		}
	}
	return name, 0, 0
}

// ggufSet is one model's GGUF files: a whole file, or the parts of one.
type ggufSet struct {
	parts map[int]string // 1-based; index 0 when the file is not split
	want  int            // the part count the names declare
	bytes uint64
	mod   time.Time
}

func (s *ggufSet) add(index int, p string, fi os.FileInfo) {
	if s.parts == nil {
		s.parts = map[int]string{}
	}
	s.parts[index] = p
	s.bytes += uint64(fi.Size())
	if fi.ModTime().After(s.mod) {
		s.mod = fi.ModTime()
	}
}

// ordered is the model's files in part order, which is also the order of a
// whole file's one entry.
func (s *ggufSet) ordered() []string {
	idx := make([]int, 0, len(s.parts))
	for i := range s.parts {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, s.parts[i])
	}
	return out
}

// dirSet is one directory's safetensors: its shards and its config.
type dirSet struct {
	files     []string
	bytes     uint64
	hasConfig bool
	mod       time.Time
}

func (d *dirSet) add(p string, fi os.FileInfo) {
	d.files = append(d.files, p)
	d.bytes += uint64(fi.Size())
	if fi.ModTime().After(d.mod) {
		d.mod = fi.ModTime()
	}
}

// scanDir walks a plain directory: every GGUF, or split GGUF, is a model, and a
// directory holding safetensors and a config.json is one more.
func scanDir(r root) []Model {
	sets := map[string]*ggufSet{}
	var order []string
	dirs := map[string]*dirSet{}
	var dirOrder []string

	walkDir(r.path, func(p string, fi os.FileInfo) {
		if fi.IsDir() || skipFile(fi.Name()) {
			return
		}
		dir := filepath.Dir(p)
		switch {
		case strings.HasSuffix(fi.Name(), ".gguf"):
			base, index, want := ggufKey(fi.Name())
			key := dir + "\x00" + base
			s := sets[key]
			if s == nil {
				s = &ggufSet{}
				sets[key] = s
				order = append(order, key)
			}
			if want > s.want {
				s.want = want
			}
			s.add(index, p, fi)
		case strings.HasSuffix(fi.Name(), ".safetensors"):
			d := dirs[dir]
			if d == nil {
				d = &dirSet{}
				dirs[dir] = d
				dirOrder = append(dirOrder, dir)
			}
			d.add(p, fi)
		case fi.Name() == "config.json":
			d := dirs[dir]
			if d == nil {
				d = &dirSet{}
				dirs[dir] = d
				dirOrder = append(dirOrder, dir)
			}
			d.hasConfig = true
		}
	})

	var out []Model
	for _, key := range order {
		out = append(out, ggufModel(sets[key], r.source))
	}
	for _, dir := range dirOrder {
		d := dirs[dir]
		// Weights without a config are not a model anyone can load, and a
		// config without weights is a directory waiting for a download.
		if len(d.files) == 0 || !d.hasConfig {
			continue
		}
		sort.Strings(d.files)
		out = append(out, Model{
			Name:    filepath.Base(dir),
			Role:    RoleForArch(modelType(dir)),
			Path:    dir,
			Files:   d.files,
			Bytes:   d.bytes,
			Format:  FormatSafetensors,
			Source:  r.source,
			Arch:    modelType(dir),
			ModTime: d.mod,
		})
	}
	return out
}

// ggufModel is one GGUF (or one split set) and what its headers say. Every part
// of a split model carries the tensors its share of the weights is made of, so
// the parameter count and the quant come from reading all of them - headers
// only, never a weight.
func ggufModel(s *ggufSet, source string) Model {
	files := s.ordered()
	m := Model{
		Name:    Stem(files[0]),
		Path:    files[0],
		Files:   files,
		Bytes:   s.bytes,
		Format:  FormatGGUF,
		Source:  source,
		Role:    RoleModel,
		Parts:   s.want,
		ModTime: s.mod,
	}
	g, err := ReadGGUF(files[0])
	if err != nil {
		m.Err = err.Error()
		// The stem still says what the file is called, and a part still says
		// whether this is a projector or a draft.
		m.Role = RoleOf("", files[0])
		return m
	}
	for _, part := range files[1:] {
		if o, err := ReadGGUF(part); err == nil {
			g.Merge(o)
		} else {
			m.Err = err.Error() // a part that will not read is reported, not skipped
			break
		}
	}
	applyHeader(&m, g)
	m.Title = g.Name()
	var missing []string
	for i := 1; i <= s.want; i++ {
		if _, ok := s.parts[i]; !ok {
			missing = append(missing, strconv.Itoa(i))
		}
	}
	if len(missing) > 0 {
		word := "parts"
		if len(missing) == 1 {
			word = "part"
		}
		m.Err = fmt.Sprintf("missing %s %s", word, strings.Join(missing, ", "))
	}
	return m
}

// applyHeader puts one GGUF header's claims on a Model: what it is called
// inside, what it is made of, and whether it is a model at all or a companion
// that goes with one.
func applyHeader(m *Model, g *GGUF) {
	m.Arch, m.Quant = g.Arch(), g.FileType()
	m.Params, m.Context, m.Layers = g.Params, g.ContextLength(), g.Layers()
	m.Role = RoleOf(g.Arch(), m.Path)
	if m.Parts == 0 {
		m.Parts = g.SplitCount()
	}
}

// walkDir walks dir with filepath.WalkDir and hands every entry the FileInfo
// from os.Stat, so a symlink is measured by what it points at.
func walkDir(dir string, fn func(p string, fi os.FileInfo)) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of a cache is not the scan's fault
		}
		if d.IsDir() {
			if fi, err := os.Stat(p); err == nil {
				fn(p, fi)
			}
			return nil
		}
		// A symlink is read as the file it points at; a dangling one is not a
		// model and drops out here.
		fi, err := os.Stat(p)
		if err != nil {
			return nil
		}
		fn(p, fi)
		return nil
	})
}

// resolve is the file a symlink points at, so a blob two revisions link twice
// is counted once.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// scanOllama reads an Ollama store: one tag is one model, its weights are the
// manifest's model layer, and that blob is a GGUF.
func scanOllama(r root) []Model {
	man := filepath.Join(r.path, "manifests")
	var out []Model
	walkDir(man, func(p string, fi os.FileInfo) {
		if fi.IsDir() || skipFile(fi.Name()) {
			return
		}
		rel, err := filepath.Rel(man, p)
		if err != nil {
			return
		}
		// <registry>/<namespace>/<model>/<tag>, however many namespace parts
		// a registry from another host brings with it.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 {
			return
		}
		// The library namespace is the one nobody types.
		name := "ollama:" + parts[len(parts)-2] + ":" + parts[len(parts)-1]
		out = append(out, ollamaModel(r.path, name, p, fi))
	})
	return out
}

// ollamaManifest is the part of an Ollama manifest this reads.
type ollamaManifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      uint64 `json:"size"`
	} `json:"layers"`
}

// ollamaModel is one tag: the manifest names a blob by digest, and the blob is
// the GGUF.
func ollamaModel(store, name, manifestPath string, fi os.FileInfo) Model {
	m := Model{Name: name, Path: manifestPath, Format: FormatOllama, Source: SourceOllama, ModTime: fi.ModTime()}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		m.Err = err.Error()
		return m
	}
	var man ollamaManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		m.Err = "manifest: " + err.Error()
		return m
	}
	var digest string
	for _, l := range man.Layers {
		if l.MediaType == ollamaModelLayer {
			digest = l.Digest
			break
		}
	}
	if digest == "" {
		m.Err = "manifest has no " + ollamaModelLayer + " layer"
		return m
	}
	// A blob is named for its digest with the colon spelled as a dash.
	blob := filepath.Join(store, "blobs", strings.ReplaceAll(digest, ":", "-"))
	m.Path, m.Files = blob, []string{blob}
	g, err := ReadGGUF(blob)
	if err != nil {
		m.Err = err.Error()
		return m
	}
	applyHeader(&m, g)
	// The blob is a hash and the tag is the model's name, so the tag stays the
	// Name (`models show ollama:qwen3:latest` means what it always meant) and
	// the header's name becomes the Title.
	m.Title = g.Name()
	if m.Title == "" {
		m.Title = name
	}
	if st, err := os.Stat(blob); err == nil {
		m.Bytes, m.ModTime = uint64(st.Size()), st.ModTime()
	}
	// Its Role says whether this tag's weights are a model or an adapter
	// Ollama runs next to one.
	return m
}

// scanHFCache reads a Hugging Face hub cache: one models--org--repo directory is
// one model, whose snapshots symlink the blobs.
func scanHFCache(r root) []Model {
	ents, err := os.ReadDir(r.path)
	if err != nil {
		return nil
	}
	var out []Model
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
			continue
		}
		repo := filepath.Join(r.path, e.Name())
		// models--org--repo is the org's repo, with the slashes spelled twice.
		name := strings.ReplaceAll(strings.TrimPrefix(e.Name(), "models--"), "--", "/")
		if m, ok := hfModel(repo, name); ok {
			out = append(out, m)
		}
	}
	return out
}

// hfModel reads one cached repo: the files under snapshots/, counted once per
// blob however many revisions link it.
func hfModel(repo, name string) (Model, bool) {
	m := Model{Name: name, Path: repo, Format: FormatGGUF, Source: SourceHFCache, Role: RoleModel}
	seen := map[string]bool{}
	var ggufs, tensors []string
	var ggufBytes, tensorBytes, biggest uint64
	var header string
	var hasConfig bool
	walkDir(filepath.Join(repo, "snapshots"), func(p string, fi os.FileInfo) {
		if fi.IsDir() || skipFile(fi.Name()) {
			return
		}
		key := resolve(p)
		if fi.Name() == "config.json" {
			hasConfig = true
			return
		}
		if seen[key] {
			return
		}
		seen[key] = true
		if fi.ModTime().After(m.ModTime) {
			m.ModTime = fi.ModTime()
		}
		switch {
		case strings.HasSuffix(p, ".gguf"):
			ggufs = append(ggufs, p)
			ggufBytes += uint64(fi.Size())
			// The weights dominate a split set, so the largest file is a part
			// of the model rather than a second model's thumbnail.
			if uint64(fi.Size()) >= biggest {
				header, biggest = p, uint64(fi.Size())
			}
		case strings.HasSuffix(p, ".safetensors"):
			tensors = append(tensors, p)
			tensorBytes += uint64(fi.Size())
		}
	})
	sort.Strings(ggufs)
	sort.Strings(tensors)
	switch {
	case len(ggufs) > 0:
		m.Files, m.Bytes = ggufs, ggufBytes
		g, err := ReadGGUF(header)
		if err != nil {
			m.Err = err.Error()
			return m, true
		}
		applyHeader(&m, g)
		m.Title = g.Name()
		return m, true
	case len(tensors) > 0:
		m.Files, m.Bytes, m.Format = tensors, tensorBytes, FormatSafetensors
		if hasConfig {
			m.Arch = modelType(filepath.Dir(tensors[0]))
			m.Role = RoleForArch(m.Arch)
		}
		return m, true
	}
	return m, false
}

// modelType is the architecture a directory's config.json states.
func modelType(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		ModelType string `json:"model_type"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return cfg.ModelType
}

// Stem is a model's name: the file, without its suffix and without its split
// part. It is the only name that never collides - a model's quants, its drafts
// and its vision projectors all answer to the same general.name - and it is what
// `berth models show NAME` and the Ollama export take.
func Stem(p string) string {
	name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	if m := splitRE.FindStringSubmatch(filepath.Base(p)); m != nil {
		name = m[1]
	}
	return name
}

// Duplicates groups the models that are likely one model held twice: the same
// format, the same size and the same quant of the same architecture. It is a
// hint, not a verdict - Berth never deletes what it only suspects.
func Duplicates(ms []Model) [][]Model {
	var groups [][]Model
	index := map[string]int{}
	for _, m := range ms {
		if m.Bytes == 0 {
			continue // an unreadable model matches nothing, and 0 matches a lot
		}
		key := strings.Join([]string{m.Format, strconv.FormatUint(m.Bytes, 10), m.Quant, m.Arch}, "\x00")
		if i, ok := index[key]; ok {
			groups[i] = append(groups[i], m)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, []Model{m})
	}
	var out [][]Model
	for _, g := range groups {
		if len(g) > 1 {
			out = append(out, g)
		}
	}
	return out
}
