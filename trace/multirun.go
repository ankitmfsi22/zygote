package trace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/mindfire/zygote/vfs"
)

// Multi-run archive layout (zip). A multi-run archive holds several recordings that share one object store:
//
//	runs.json              {"version":1,"format":"zygote-multi-run","runs":[{"name":"<run>","manifest":"runs/<run>/trace.json"}, ...]}
//	runs/<run>/trace.json  the run's Recording, byte-for-byte the trace.json of its original single-run bundle
//	objects/<blake3 hex>   one shared object store; an object used by several runs is stored once
//
// The runs in runs.json are sorted by name and the archive has no top-level trace.json, so a zyg that only
// understands single-run bundles refuses it instead of misreading it. Output is deterministic: the same runs
// always produce the same bytes. Run names match [A-Za-z0-9._-]+, are not "." or "..", and are at most 128 bytes.
// A single-run bundle is unchanged: trace.json at the top level plus objects/<blake3 hex>.
const (
	runsIndexName  = "runs.json"
	runsPrefix     = "runs/"
	objectsPrefix  = "objects/"
	manifestName   = "trace.json"
	multiRunFormat = "zygote-multi-run"
	maxRunNameLen  = 128
)

var (
	// ErrMultiRunArchive indicates a multi-run archive was opened where a single-run bundle is required.
	ErrMultiRunArchive = errors.New("bundle is a multi-run archive")
	// ErrSingleRunBundle indicates a single-run bundle was given where a multi-run archive is required.
	ErrSingleRunBundle = errors.New("bundle is a single-run bundle, not a multi-run archive")
	// ErrInvalidArchive indicates a malformed multi-run archive.
	ErrInvalidArchive = errors.New("corrupt multi-run archive")
	// ErrInvalidRunName indicates a run name that is not allowed in a multi-run archive.
	ErrInvalidRunName = errors.New("invalid run name")
	// ErrUnknownRun indicates a run name that is not in the archive.
	ErrUnknownRun = errors.New("run not found in archive")
)

// InputError marks a failure to read or validate an input bundle, as opposed to a failure to write the output.
type InputError struct {
	Path string
	Err  error
}

func (e *InputError) Error() string { return e.Path + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *InputError) Unwrap() error { return e.Err }

type runsIndex struct {
	Version int      `json:"version"`
	Format  string   `json:"format"`
	Runs    []runRef `json:"runs"`
}

type runRef struct {
	Name     string `json:"name"`
	Manifest string `json:"manifest"`
}

type archiveRun struct {
	name     string
	manifest []byte
	rec      Recording
}

// Archive is a fully validated single-run bundle or multi-run archive held in memory.
type Archive struct {
	multi bool
	runs  []archiveRun
	store *vfs.MemStore
}

func invalidArchive(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArchive, fmt.Sprintf(format, args...))
}

func validRunName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("%w: %q", ErrInvalidRunName, name)
	case len(name) > maxRunNameLen:
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidRunName, name[:16]+"...", maxRunNameLen)
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("%w: %q may only contain letters, digits, '.', '_' and '-'", ErrInvalidRunName, name)
		}
	}
	return nil
}

func manifestPath(run string) string { return runsPrefix + run + "/" + manifestName }

func findEntry(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func readEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

func decodeRecording(b []byte) (Recording, error) {
	var rec Recording
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&rec); err != nil {
		return Recording{}, err
	}
	if rec.Version != 1 {
		return Recording{}, fmt.Errorf("unsupported format version: %d", rec.Version)
	}
	return rec, nil
}

func parseRunsIndex(f *zip.File) (runsIndex, error) {
	b, err := readEntry(f)
	if err != nil {
		return runsIndex{}, err
	}
	var idx runsIndex
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&idx); err != nil {
		return runsIndex{}, invalidArchive("%s: %v", runsIndexName, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return runsIndex{}, invalidArchive("%s: trailing data after the JSON document", runsIndexName)
	}
	if idx.Version != 1 {
		return runsIndex{}, invalidArchive("%s: unsupported version %d", runsIndexName, idx.Version)
	}
	if idx.Format != multiRunFormat {
		return runsIndex{}, invalidArchive("%s: unsupported format %q", runsIndexName, idx.Format)
	}
	if len(idx.Runs) == 0 {
		return runsIndex{}, invalidArchive("%s lists no runs", runsIndexName)
	}
	seen := make(map[string]struct{}, len(idx.Runs))
	for _, r := range idx.Runs {
		if err := validRunName(r.Name); err != nil {
			return runsIndex{}, fmt.Errorf("%w: %s: %w", ErrInvalidArchive, runsIndexName, err)
		}
		if _, dup := seen[r.Name]; dup {
			return runsIndex{}, invalidArchive("%s: duplicate run name %q", runsIndexName, r.Name)
		}
		seen[r.Name] = struct{}{}
		if want := manifestPath(r.Name); r.Manifest != want {
			return runsIndex{}, invalidArchive("%s: run %q has manifest %q, want %q", runsIndexName, r.Name, r.Manifest, want)
		}
	}
	return idx, nil
}

func openZipFile(filename string, cfg importConfig) (*zip.Reader, error) {
	data, err := os.ReadFile(filename) //nolint:gosec
	if err != nil {
		return nil, err
	}
	encrypted := bytes.HasPrefix(data, ageMagic)
	if encrypted {
		data, err = decryptBundle(data, cfg)
		if err != nil {
			return nil, err
		}
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if encrypted {
			return nil, fmt.Errorf("%w: %v", ErrCorruptEncrypted, err)
		}
		return nil, err
	}
	return zr, nil
}

// OpenArchive reads a single-run bundle or a multi-run archive, decrypting it when needed, and validates every
// object hash and the full reachability of every run before returning. Any problem is an error.
func OpenArchive(filename string, opts ...ImportOption) (*Archive, error) {
	var cfg importConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	zr, err := openZipFile(filename, cfg)
	if err != nil {
		return nil, err
	}
	return loadArchive(zr)
}

func loadArchive(zr *zip.Reader) (*Archive, error) {
	if f := findEntry(zr, runsIndexName); f != nil {
		return loadMulti(zr, f)
	}
	return loadSingle(zr)
}

func loadSingle(zr *zip.Reader) (*Archive, error) {
	store := vfs.NewMemStore()
	b, err := importZip(zr, store)
	if err != nil {
		return nil, err
	}
	var manifest []byte
	for _, f := range zr.File {
		if f.Name != manifestName {
			continue
		}
		if manifest != nil {
			return nil, errors.New("corrupt bundle: duplicate trace.json")
		}
		if manifest, err = readEntry(f); err != nil {
			return nil, err
		}
	}
	return &Archive{store: store, runs: []archiveRun{{manifest: manifest, rec: b.Recording}}}, nil
}

func isObjectName(hexName string) (vfs.Hash, bool) {
	var h vfs.Hash
	if len(hexName) != len(h)*2 {
		return h, false
	}
	if _, err := hex.Decode(h[:], []byte(hexName)); err != nil || hex.EncodeToString(h[:]) != hexName {
		return vfs.Hash{}, false
	}
	return h, true
}

func loadMulti(zr *zip.Reader, indexFile *zip.File) (*Archive, error) {
	idx, err := parseRunsIndex(indexFile)
	if err != nil {
		return nil, err
	}
	manifests := make(map[string]string, len(idx.Runs))
	for _, r := range idx.Runs {
		manifests[r.Manifest] = r.Name
	}

	store := vfs.NewMemStore()
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if _, dup := files[f.Name]; dup {
			return nil, invalidArchive("duplicate entry %q", f.Name)
		}
		files[f.Name] = f

		switch {
		case f.Name == runsIndexName:
		case f.Name == manifestName:
			return nil, invalidArchive("unexpected top-level %s in a multi-run archive", manifestName)
		case strings.HasPrefix(f.Name, objectsPrefix):
			want, ok := isObjectName(strings.TrimPrefix(f.Name, objectsPrefix))
			if !ok {
				return nil, invalidArchive("object %q is not named by a 64-character lowercase hex hash", f.Name)
			}
			data, err := readEntry(f)
			if err != nil {
				return nil, err
			}
			if hashBytes(data) != want {
				return nil, ErrHashMismatch
			}
			if _, err := store.Put(data); err != nil {
				return nil, err
			}
		default:
			if _, ok := manifests[f.Name]; !ok {
				return nil, invalidArchive("unexpected entry %q", f.Name)
			}
		}
	}

	a := &Archive{multi: true, store: store}
	for _, r := range idx.Runs {
		f, ok := files[r.Manifest]
		if !ok {
			return nil, invalidArchive("run %q: missing %s", r.Name, r.Manifest)
		}
		manifest, err := readEntry(f)
		if err != nil {
			return nil, err
		}
		rec, err := decodeRecording(manifest)
		if err != nil {
			return nil, fmt.Errorf("run %q: %w", r.Name, err)
		}
		if err := checkRun(store, rec); err != nil {
			return nil, fmt.Errorf("run %q: %w", r.Name, err)
		}
		a.runs = append(a.runs, archiveRun{name: r.Name, manifest: manifest, rec: rec})
	}
	slices.SortFunc(a.runs, func(x, y archiveRun) int { return strings.Compare(x.name, y.name) })
	return a, nil
}

// checkRun applies the same validation as importZip to a run whose objects live in store.
func checkRun(store vfs.Store, rec Recording) error {
	if err := validateEffects(store, rec.Effects); err != nil {
		return err
	}
	for _, step := range rec.Steps {
		hb, err := hex.DecodeString(step.Root)
		if err != nil || len(hb) != 32 {
			continue
		}
		var h vfs.Hash
		copy(h[:], hb)
		if err := vfs.Verify(context.Background(), store, vfs.Snapshot{Root: h}); err != nil {
			return ErrMissingObject
		}
	}
	return nil
}

// needed returns the sorted hashes of every object a run needs: its step trees and its CAS-spilled effect values.
func (r archiveRun) needed(store vfs.Store) ([]vfs.Hash, error) {
	var snaps []vfs.Snapshot
	for _, step := range r.rec.Steps {
		hb, err := hex.DecodeString(step.Root)
		if err != nil || len(hb) != 32 {
			continue
		}
		var h vfs.Hash
		copy(h[:], hb)
		snaps = append(snaps, vfs.Snapshot{Root: h})
	}
	reachable, err := vfs.Reachable(context.Background(), store, snaps)
	if err != nil {
		return nil, err
	}
	for _, e := range r.rec.Effects {
		if e.ValueRef == "" {
			continue
		}
		h, err := parseRef(e.ValueRef)
		if err != nil {
			return nil, err
		}
		reachable[h] = struct{}{}
	}
	delete(reachable, vfs.EmptyTreeHash)
	hashes := make([]vfs.Hash, 0, len(reachable))
	for h := range reachable {
		hashes = append(hashes, h)
	}
	slices.SortFunc(hashes, func(a, b vfs.Hash) int { return bytes.Compare(a[:], b[:]) })
	return hashes, nil
}

// Multi reports whether the archive is a multi-run archive rather than a single-run bundle.
func (a *Archive) Multi() bool { return a.multi }

// Runs returns the run names in sorted order. A single-run bundle has exactly one run, named "".
func (a *Archive) Runs() []string {
	names := make([]string, len(a.runs))
	for i, r := range a.runs {
		names[i] = r.name
	}
	return names
}

func (a *Archive) run(name string) (archiveRun, error) {
	for _, r := range a.runs {
		if r.name == name {
			return r, nil
		}
	}
	return archiveRun{}, fmt.Errorf("%w: %q (runs: %s)", ErrUnknownRun, name, strings.Join(a.Runs(), ", "))
}

// ImportRun copies the objects one run needs into store and returns it as a Bundle.
// A single-run bundle's run is named "".
func (a *Archive) ImportRun(name string, store vfs.Store) (Bundle, error) {
	r, err := a.run(name)
	if err != nil {
		return Bundle{}, err
	}
	hashes, err := r.needed(a.store)
	if err != nil {
		return Bundle{}, ErrMissingObject
	}
	for _, h := range hashes {
		data, ok := a.store.Get(h)
		if !ok {
			return Bundle{}, ErrMissingObject
		}
		if _, err := store.Put(data); err != nil {
			return Bundle{}, err
		}
	}
	return Bundle{Recording: r.rec, Store: store}, nil
}

// ListRuns validates a multi-run archive and returns its run names in sorted order.
// A single-run bundle yields ErrSingleRunBundle.
func ListRuns(filename string, opts ...ImportOption) ([]string, error) {
	a, err := OpenArchive(filename, opts...)
	if err != nil {
		return nil, err
	}
	if !a.multi {
		return nil, ErrSingleRunBundle
	}
	return a.Runs(), nil
}

// ImportRun validates a multi-run archive and imports the named run into store.
// A single-run bundle yields ErrSingleRunBundle.
func ImportRun(filename, name string, store vfs.Store, opts ...ImportOption) (Bundle, error) {
	a, err := OpenArchive(filename, opts...)
	if err != nil {
		return Bundle{}, err
	}
	if !a.multi {
		return Bundle{}, ErrSingleRunBundle
	}
	return a.ImportRun(name, store)
}

// multiRunError builds the ErrMultiRunArchive error ImportFile returns for a multi-run archive.
func multiRunError(f *zip.File) error {
	idx, err := parseRunsIndex(f)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: it has %d runs; use `zyg split` to extract one, or `zyg verify --corpus` to replay them", ErrMultiRunArchive, len(idx.Runs))
}

type blob struct {
	hash vfs.Hash
	data []byte
}

func writeBlobs(zw *zip.Writer, blobs []blob) error {
	for _, b := range blobs {
		w, err := zw.Create(objectsPrefix + hex.EncodeToString(b.hash[:]))
		if err != nil {
			return err
		}
		if _, err := w.Write(b.data); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeEntry(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// writeSingleRunZip writes a standard single-run bundle with the exact manifest bytes given.
func writeSingleRunZip(w io.Writer, manifest []byte, blobs []blob) error {
	zw := zip.NewWriter(w)
	if err := writeEntry(zw, manifestName, manifest); err != nil {
		return err
	}
	return writeBlobs(zw, blobs)
}

func writeMultiRunZip(w io.Writer, runs []archiveRun, blobs []blob) error {
	idx := runsIndex{Version: 1, Format: multiRunFormat, Runs: make([]runRef, len(runs))}
	for i, r := range runs {
		idx.Runs[i] = runRef{Name: r.name, Manifest: manifestPath(r.name)}
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	if err := writeEntry(zw, runsIndexName, b); err != nil {
		return err
	}
	for _, r := range runs {
		if err := writeEntry(zw, manifestPath(r.name), r.manifest); err != nil {
			return err
		}
	}
	return writeBlobs(zw, blobs)
}

func (a *Archive) blobsFor(r archiveRun) ([]blob, error) {
	hashes, err := r.needed(a.store)
	if err != nil {
		return nil, ErrMissingObject
	}
	blobs := make([]blob, len(hashes))
	for i, h := range hashes {
		data, ok := a.store.Get(h)
		if !ok {
			return nil, ErrMissingObject
		}
		blobs[i] = blob{h, data}
	}
	return blobs, nil
}
