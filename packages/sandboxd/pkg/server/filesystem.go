// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
)

// Wire types live in filesystem_types.go; the /v1/metadata handler lives in
// metadata.go.

var modePattern = regexp.MustCompile(`^0[0-7]{3}$`)

// Values of FileEntry.Type, also accepted by the "type" query on GET and HEAD.
const (
	entryTypeFile      = "file"
	entryTypeDirectory = "directory"
)

// maxRecursiveEntries caps a recursive listing, which sandboxd buffers in
// memory before encoding it, so one request cannot exhaust the daemon.
const maxRecursiveEntries = 10000

var errListingTooLarge = errors.New("listing too large")

// Caps on one extract=tar request. A small gzip body can expand a thousand
// times over, so these keep one request from filling the sandbox disk or inodes.
const (
	maxExtractEntries = 100000
	maxExtractBytes   = 1 << 30
)

// RESTServer implements the sandboxd Filesystem & Runtime REST API on top of
// net/http. All file paths are confined to rootDir via pathutil.SanitizePath.
type RESTServer struct {
	rootDir           string
	resolvedRoot      string
	metadataEnvPrefix string
	startTime         time.Time
	ready             atomic.Bool
	log               logr.Logger

	// environ is swappable for tests; defaults to os.Environ.
	environ func() []string
	// maxListEntries is swappable for tests; defaults to maxRecursiveEntries.
	maxListEntries int
	// maxExtractEntries and maxExtractBytes are swappable for tests.
	maxExtractEntries int
	maxExtractBytes   int64
	// metadataEnv returns the filtered /v1/metadata env map, computed once
	// on first use: the daemon's environment is immutable after start.
	metadataEnv func() map[string]string
}

// NewRESTServer builds a RESTServer rooted at rootDir. rootDir must be
// non-empty. Only environment variables carrying metadataEnvPrefix are exposed
// on /v1/metadata. The server starts ready; SetReady(false) flips /v1/health
// to 503.
func NewRESTServer(rootDir, metadataEnvPrefix string, log logr.Logger) *RESTServer {
	if metadataEnvPrefix == "" {
		metadataEnvPrefix = "SANDBOX_"
	}
	resolvedRoot, err := pathutil.SanitizePath(rootDir, ".")
	if err != nil {
		resolvedRoot, _ = filepath.Abs(filepath.Clean(rootDir))
	}
	s := &RESTServer{
		rootDir:           rootDir,
		resolvedRoot:      resolvedRoot,
		metadataEnvPrefix: metadataEnvPrefix,
		startTime:         time.Now(),
		log:               log,
		environ:           os.Environ,
		maxListEntries:    maxRecursiveEntries,
		maxExtractEntries: maxExtractEntries,
		maxExtractBytes:   maxExtractBytes,
	}
	s.ready.Store(true)
	s.metadataEnv = sync.OnceValue(s.buildMetadataEnv)
	return s
}

// SetReady toggles the /v1/health response between 200 and 503. Flipped to
// false at the start of graceful shutdown so Kubernetes stops routing.
func (s *RESTServer) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Handler returns the fully routed http.Handler for the REST API.
func (s *RESTServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// A "GET" pattern also serves HEAD with the body suppressed, giving the
	// KEP's existence probe (HEAD /v1/files/{path}) for free.
	mux.HandleFunc("GET /v1/files/{path...}", s.handleGetFile)
	mux.HandleFunc("PUT /v1/files/{path...}", s.handlePutFile)
	mux.HandleFunc("DELETE /v1/files/{path...}", s.handleDeleteFile)
	mux.HandleFunc("POST /v1/files/{path...}", s.handleFileAction)
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/metadata", s.handleMetadata)
	return mux
}

func (s *RESTServer) writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.V(4).Info("failed to encode response body", "error", err)
	}
}

func (s *RESTServer) writeError(w http.ResponseWriter, httpCode int, code, message string) {
	s.writeJSON(w, httpCode, APIError{Code: code, Message: message})
}

// writeFSError maps a filesystem error to the status the spec documents for it.
func (s *RESTServer) writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", "path does not exist within the sandbox")
	case errors.Is(err, os.ErrPermission):
		s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		s.writeError(w, http.StatusConflict, "CONFLICT", "path conflicts with an existing file or directory")
	default:
		s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// resolvePath sanitizes the {path...} wildcard against the sandbox root,
// writing the spec-mandated error response on failure.
func (s *RESTServer) resolvePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	return s.resolveUserPath(w, r.PathValue("path"), pathutil.SanitizePath)
}

// resolveUserPath is resolvePath for any client-supplied path, such as a
// destination query. sanitize is pathutil.SanitizePath, or
// pathutil.SanitizeEntryPath to leave a final symlink unresolved.
func (s *RESTServer) resolveUserPath(w http.ResponseWriter, userPath string,
	sanitize func(rootDir, userPath string) (string, error)) (string, bool) {
	target, err := sanitize(s.rootDir, userPath)
	if err != nil {
		if errors.Is(err, pathutil.ErrPathEscapes) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED",
				"path traversal outside sandbox root is forbidden")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return "", false
	}
	return target, true
}

// boolQuery reads an optional boolean query parameter. It writes a 400 and
// returns ok=false when the value does not parse.
func (s *RESTServer) boolQuery(w http.ResponseWriter, r *http.Request, name string) (value, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, true
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("%s %q must be a boolean", name, raw))
		return false, false
	}
	return parsed, true
}

// typeQuery reads the optional "type" query parameter. It writes a 400 and
// returns ok=false when the value is neither "file" nor "directory".
func (s *RESTServer) typeQuery(w http.ResponseWriter, r *http.Request) (value string, ok bool) {
	query := r.URL.Query()
	value = query.Get("type")
	if query.Has("type") && value != entryTypeFile && value != entryTypeDirectory {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("type %q must be %q or %q", value, entryTypeFile, entryTypeDirectory))
		return "", false
	}
	return value, true
}

// extractQuery reports whether the request carries "extract=tar". It writes a
// 400 and returns ok=false for any other value.
func (s *RESTServer) extractQuery(w http.ResponseWriter, r *http.Request) (extract, ok bool) {
	query := r.URL.Query()
	if !query.Has("extract") {
		return false, true
	}
	if value := query.Get("extract"); value != "tar" {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("extract %q must be %q", value, "tar"))
		return false, false
	}
	return true, true
}

// modeQuery reads the optional "mode" query parameter, returning def when it
// is absent. It writes a 400 and returns ok=false when the value is invalid.
func (s *RESTServer) modeQuery(w http.ResponseWriter, r *http.Request, def os.FileMode) (os.FileMode, bool) {
	raw := r.URL.Query().Get("mode")
	if raw == "" {
		return def, true
	}
	if !modePattern.MatchString(raw) {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("mode %q must match ^0[0-7]{3}$", raw))
		return 0, false
	}
	parsed, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return 0, false
	}
	return os.FileMode(parsed), true
}

func (s *RESTServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	wantType, ok := s.typeQuery(w, r)
	if !ok {
		return
	}
	maxDepth, ok := s.listDepth(w, r)
	if !ok {
		return
	}

	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "NOT_FOUND", "path does not exist within the sandbox")
		} else if os.IsPermission(err) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	if msg := typeMismatch(wantType, info.IsDir()); msg != "" {
		s.writeError(w, http.StatusConflict, "CONFLICT", msg)
		return
	}

	if info.IsDir() {
		s.serveDirectoryListing(w, r, target, maxDepth)
		return
	}

	f, err := os.Open(target)
	if err != nil {
		if os.IsPermission(err) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent streams via seek + chunked io.Copy — the file is never
	// fully buffered in memory, so downloads are not size-capped. It also
	// handles HEAD, Content-Length, and Range requests, and keeps the
	// Content-Type set above.
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// typeMismatch returns why the path disagrees with the "type" query, or "".
func typeMismatch(want string, isDir bool) string {
	switch {
	case want == entryTypeFile && isDir:
		return "path is a directory, not a file"
	case want == entryTypeDirectory && !isDir:
		return "path is a file, not a directory"
	}
	return ""
}

// listDepth returns how many directory levels a listing descends: 1 unless
// recursive=true, then max_depth when given and unlimited otherwise. It writes
// a 400 and returns ok=false on an invalid value.
func (s *RESTServer) listDepth(w http.ResponseWriter, r *http.Request) (depth int, ok bool) {
	recursive, ok := s.boolQuery(w, r, "recursive")
	if !ok {
		return 0, false
	}
	depth = math.MaxInt
	if raw := r.URL.Query().Get("max_depth"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				fmt.Sprintf("max_depth %q must be a positive integer", raw))
			return 0, false
		}
		depth = n
	}
	if !recursive {
		depth = 1
	}
	return depth, true
}

// serveDirectoryListing lists target up to maxDepth levels deep. Entry names
// are paths relative to target, so a flat listing yields plain names.
func (s *RESTServer) serveDirectoryListing(w http.ResponseWriter, r *http.Request, target string, maxDepth int) {
	requestPath := r.PathValue("path")
	entries := []FileEntry{}
	// WalkDir visits each directory's entries in name order and does not
	// follow symlinks, so a link to a directory is listed but never entered.
	err := filepath.WalkDir(target, func(p string, d fs.DirEntry, walkErr error) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if p == target {
			return walkErr
		}
		if walkErr != nil {
			return nil // Unreadable subdirectory: keep it listed, without contents.
		}
		rel, err := filepath.Rel(target, p)
		if err != nil {
			return err
		}
		entry, ok := s.fileEntry(d, filepath.ToSlash(rel), filepath.Join(requestPath, rel))
		if !ok {
			return nil
		}
		if maxDepth > 1 && len(entries) >= s.maxListEntries {
			return errListingTooLarge
		}
		entries = append(entries, entry)
		// SkipDir on a non-directory would skip the rest of its parent.
		if d.IsDir() && strings.Count(entry.Name, "/")+1 >= maxDepth {
			return fs.SkipDir
		}
		return nil
	})
	if errors.Is(err, errListingTooLarge) {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("listing exceeds %d entries; lower max_depth or list a subdirectory", s.maxListEntries))
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	relPath := requestPath
	if !strings.HasPrefix(relPath, "/") {
		relPath = "/" + relPath
	}
	s.writeJSON(w, http.StatusOK, DirectoryListing{Path: relPath, Entries: entries})
}

// fileEntry describes d, which lives at sandboxPath below the sandbox root, as
// a listing row called name. It returns ok=false for entries a listing omits:
// ones that vanished, and symlinks that are broken or leave the sandbox root.
func (s *RESTServer) fileEntry(d fs.DirEntry, name, sandboxPath string) (entry FileEntry, ok bool) {
	info, err := d.Info()
	if err != nil {
		return FileEntry{}, false
	}
	if d.Type()&os.ModeSymlink != 0 {
		resolved, err := pathutil.SanitizePath(s.rootDir, sandboxPath)
		if err != nil {
			return FileEntry{}, false
		}
		if info, err = os.Stat(resolved); err != nil {
			return FileEntry{}, false
		}
	}
	entryType := entryTypeFile
	if info.IsDir() {
		entryType = entryTypeDirectory
	}
	return FileEntry{
		Name:       name,
		Size:       info.Size(),
		Type:       entryType,
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		Mode:       fmt.Sprintf("%04o", info.Mode().Perm()),
	}, true
}

func (s *RESTServer) handlePutFile(w http.ResponseWriter, r *http.Request) {
	putType, ok := s.typeQuery(w, r)
	if !ok {
		return
	}
	isDirectory := putType == entryTypeDirectory

	extract, ok := s.extractQuery(w, r)
	if !ok {
		return
	}

	defaultMode := os.FileMode(0o644)
	if isDirectory || extract {
		defaultMode = 0o755
	}
	mode, ok := s.modeQuery(w, r, defaultMode)
	if !ok {
		return
	}

	appendMode, ok := s.boolQuery(w, r, "append")
	if !ok {
		return
	}

	createOnly, ok := s.createOnly(w, r)
	if !ok {
		return
	}

	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}

	if extract {
		if appendMode || putType != "" {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"extract cannot be used with append or type")
			return
		}
		s.putArchive(w, r, target, mode)
		return
	}

	if isDirectory {
		if appendMode {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"append cannot be used with type=directory")
			return
		}
		// Directory creation is idempotent, so it cannot honor the precondition.
		if createOnly {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"If-None-Match cannot be used with type=directory")
			return
		}
		s.putDirectory(w, r, target, mode)
		return
	}

	if info, err := os.Stat(target); err == nil && info.IsDir() {
		if createOnly {
			s.writePathExists(w)
			return
		}
		if msg := typeMismatch(putType, true); msg != "" {
			s.writeError(w, http.StatusConflict, "CONFLICT", msg)
			return
		}
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "target path is a directory")
		return
	}

	body, cleanup, err := requestFileBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer cleanup()

	write := atomicWrite
	switch {
	case createOnly:
		write = atomicCreate
	case appendMode:
		write = appendWrite
	}
	if err := write(target, body, mode); err != nil {
		switch {
		case createOnly && errors.Is(err, os.ErrExist):
			s.writePathExists(w)
		case errors.Is(err, os.ErrPermission):
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		default:
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// putDirectory handles PUT with type=directory. It ignores the request body.
func (s *RESTServer) putDirectory(w http.ResponseWriter, r *http.Request, target string, mode os.FileMode) {
	parents, ok := s.boolQuery(w, r, "parents")
	if !ok {
		return
	}

	if err := makeDirectory(target, mode, parents); err != nil {
		s.writeMakeDirectoryError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeMakeDirectoryError maps a makeDirectory failure to the status the spec
// documents for it.
func (s *RESTServer) writeMakeDirectoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.writeError(w, http.StatusNotFound, "NOT_FOUND",
			"parent directory does not exist; pass parents=true to create it")
	case errors.Is(err, os.ErrExist):
		s.writeError(w, http.StatusConflict, "CONFLICT", "path exists and is not a directory")
	default:
		s.writeFSError(w, err)
	}
}

// makeDirectory creates the directory target, and its missing parents when
// parents is true. It succeeds when target is already a directory, so retries
// are safe, and applies mode only to a directory it creates.
func makeDirectory(target string, mode os.FileMode, parents bool) error {
	if parents {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("failed to create parent directory: %w", err)
		}
	}

	err := os.Mkdir(target, mode)
	if errors.Is(err, os.ErrExist) {
		if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Mkdir applies the umask; chmod so a new directory gets exactly mode, as
	// atomicWrite does for files.
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("failed to set directory mode: %w", err)
	}
	return nil
}

// requestFileBody returns the file payload reader for a PUT request: the raw
// body, or the required "file" part of a multipart/form-data upload.
func requestFileBody(r *http.Request) (io.Reader, func(), error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if contentType != "" && err == nil && mediaType == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil, nil, fmt.Errorf("multipart body is missing the required %q part", "file")
			}
			if err != nil {
				return nil, nil, fmt.Errorf("invalid multipart body: %w", err)
			}
			if part.FormName() == "file" {
				return part, func() { _ = part.Close() }, nil
			}
			_ = part.Close()
		}
	}
	return r.Body, func() {}, nil
}

// writePathExists writes the 412 returned when a create-only PUT finds an
// existing path.
func (s *RESTServer) writePathExists(w http.ResponseWriter) {
	s.writeError(w, http.StatusPreconditionFailed, "FAILED_PRECONDITION", "path already exists")
}

// createOnly reports whether the request carries "If-None-Match: *". sandboxd
// has no ETags, so it rejects any other value instead of ignoring a
// precondition the client relies on. It writes a 400 and returns ok=false in
// that case.
func (s *RESTServer) createOnly(w http.ResponseWriter, r *http.Request) (createOnly, ok bool) {
	values := r.Header.Values("If-None-Match")
	if len(values) == 0 {
		return false, true
	}
	if len(values) == 1 && strings.TrimSpace(values[0]) == "*" {
		return true, true
	}
	s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", `If-None-Match must be "*"`)
	return false, false
}

// atomicWrite streams src into target using the temp-file-then-rename
// strategy. Parent directories are created automatically.
//
// Concurrency: handlers run in parallel (one goroutine per request, no
// cross-request locking). Each successful rename(2) publishes a complete file
// atomically, so an already-open reader sees either the previous complete file
// or the new one. A concurrent delete can remove the temporary file or its
// parent directory before rename, in which case the write fails.
func atomicWrite(target string, src io.Reader, mode os.FileMode) error {
	return writeViaTemp(target, src, mode, os.Rename)
}

// atomicCreate is atomicWrite for a target that must not exist: when it does,
// the write fails with an error wrapping os.ErrExist. link(2) never replaces
// its destination, so the check and the create are one atomic step.
func atomicCreate(target string, src io.Reader, mode os.FileMode) error {
	return writeViaTemp(target, src, mode, func(tmp, target string) error {
		if err := os.Link(tmp, target); err != nil {
			return err
		}
		_ = os.Remove(tmp)
		return nil
	})
}

// writeViaTemp writes src to a synced temporary file beside target, then calls
// place to put it at target.
func writeViaTemp(target string, src io.Reader, mode os.FileMode, place func(tmp, target string) error) (err error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	// Remove the temp file on any failure, including client disconnects
	// mid-upload.
	defer func() {
		if err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
		}
	}()

	if err = tmpFile.Chmod(mode); err != nil {
		return fmt.Errorf("failed to set file mode: %w", err)
	}
	if _, err = io.Copy(tmpFile, src); err != nil {
		return fmt.Errorf("failed to write file contents: %w", err)
	}
	if err = tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}
	if err = tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	if err = place(tmpFile.Name(), target); err != nil {
		return fmt.Errorf("failed to move file into place: %w", err)
	}
	return nil
}

// appendWrite streams src to the end of target. It creates target, and its
// parent directories, when missing, and applies mode only to a file it creates.
// Unlike atomicWrite, readers can see a partial append, and concurrent appends
// can interleave between writes.
func appendWrite(target string, src io.Reader, mode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	// O_EXCL tells us whether this request created the file.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, mode)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	}
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("failed to close file: %w", cerr)
		}
	}()

	// OpenFile applies the umask; chmod so a new file gets exactly mode, as
	// atomicWrite does.
	if created {
		if err = f.Chmod(mode); err != nil {
			return fmt.Errorf("failed to set file mode: %w", err)
		}
	}
	if _, err = io.Copy(f, src); err != nil {
		return fmt.Errorf("failed to append file contents: %w", err)
	}
	return nil
}

var (
	// errBadArchive marks an archive that cannot be extracted as sent.
	errBadArchive = errors.New("invalid archive")
	// errArchiveConflict marks an entry whose type clashes with what already
	// exists at its path.
	errArchiveConflict = errors.New("archive entry conflicts with an existing path")
	// errArchiveTooLarge marks an archive over the entry or byte cap.
	errArchiveTooLarge = errors.New("archive too large")

	gzipMagic = []byte{0x1f, 0x8b}
)

// putArchive handles PUT with extract=tar. It unpacks the request body, a tar
// stream that may be gzip-compressed, into target and creates target first
// when it is missing. Extraction is not atomic: an error leaves the entries
// written so far in place.
func (s *RESTServer) putArchive(w http.ResponseWriter, r *http.Request, target string, mode os.FileMode) {
	if err := makeDirectory(target, mode, true); err != nil {
		s.writeMakeDirectoryError(w, err)
		return
	}

	body, cleanup, err := requestFileBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer cleanup()

	if err := s.extractTar(body, target); err != nil {
		s.writeExtractError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// extractTar writes the entries of the tar stream src below dir.
func (s *RESTServer) extractTar(src io.Reader, dir string) error {
	buffered := bufio.NewReader(src)
	src = buffered
	if magic, _ := buffered.Peek(len(gzipMagic)); bytes.Equal(magic, gzipMagic) {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		defer func() { _ = gz.Close() }()
		src = gz
	}

	// tar.Reader yields exactly hdr.Size bytes per file, so the header size
	// is what the entry writes and the cap can fail before writing anything.
	tr := tar.NewReader(src)
	entries, bytesLeft := 0, s.maxExtractBytes
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		entries++
		if hdr.Typeflag == tar.TypeReg {
			bytesLeft -= hdr.Size
		}
		if entries > s.maxExtractEntries || bytesLeft < 0 {
			return errArchiveTooLarge
		}
		if err := s.extractEntry(tr, hdr, dir); err != nil {
			return err
		}
	}
}

// extractEntry writes one archive entry below dir. It supports regular files,
// directories, and symlinks. It rejects hard links, which could alias a file
// outside the sandbox root, and device and FIFO entries.
func (s *RESTServer) extractEntry(body io.Reader, hdr *tar.Header, dir string) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil // `git archive` writes one that carries the commit ID.
	}
	switch hdr.Typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
	default:
		return fmt.Errorf("%w: entry %q has unsupported type %q", errBadArchive, hdr.Name, hdr.Typeflag)
	}
	name := filepath.Clean(filepath.FromSlash(hdr.Name))
	if name == "." {
		return nil
	}
	if !filepath.IsLocal(name) {
		return fmt.Errorf("%w: entry %q", pathutil.ErrPathEscapes, hdr.Name)
	}

	// Resolve only the parent, so an existing symlink at the entry's own path
	// is replaced and never followed. Resolving the parent also stops an entry
	// from writing through a symlink an earlier entry planted. Confine it to
	// dir, which is already resolved.
	parent, err := pathutil.SanitizePath(dir, filepath.Dir(name))
	if err != nil {
		return err
	}
	leaf := filepath.Join(parent, filepath.Base(name))

	// A directory entry may only meet a directory, and any other entry only a
	// non-directory. Lstat reports a symlink as a non-directory.
	isDir := hdr.Typeflag == tar.TypeDir
	if existing, err := os.Lstat(leaf); err == nil && existing.IsDir() != isDir {
		return fmt.Errorf("%w: %q", errArchiveConflict, hdr.Name)
	}

	perm := hdr.FileInfo().Mode().Perm()
	switch hdr.Typeflag {
	case tar.TypeDir:
		// Keep owner access so the entries below the directory can be written.
		return makeDirectory(leaf, perm|0o700, true)
	case tar.TypeReg:
		return atomicWrite(leaf, body, perm)
	default:
		return replaceSymlink(leaf, hdr.Linkname)
	}
}

// replaceSymlink makes link a symlink to target, replacing an existing file or
// symlink and creating missing parent directories.
func replaceSymlink(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to replace symlink: %w", err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("failed to create symlink: %w", err)
	}
	return nil
}

// writeExtractError maps an extraction failure to the status the spec
// documents for it.
func (s *RESTServer) writeExtractError(w http.ResponseWriter, err error) {
	var corrupt flate.CorruptInputError
	switch {
	case errors.Is(err, pathutil.ErrPathEscapes):
		s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED",
			"archive entry outside the destination directory is forbidden")
	case errors.Is(err, errArchiveTooLarge):
		s.writeError(w, http.StatusRequestEntityTooLarge, "RESOURCE_EXHAUSTED",
			fmt.Sprintf("archive exceeds %d entries or %d bytes of file data", s.maxExtractEntries, s.maxExtractBytes))
	case errors.Is(err, errArchiveConflict), errors.Is(err, syscall.ENOTDIR):
		s.writeError(w, http.StatusConflict, "CONFLICT",
			"an archive entry conflicts with an existing file or directory")
	case errors.Is(err, errBadArchive), errors.Is(err, tar.ErrHeader), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum), errors.As(err, &corrupt):
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	default:
		s.writeFSError(w, err)
	}
}

func (s *RESTServer) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}

	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "NOT_FOUND", "path does not exist within the sandbox")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	recursive, ok := s.boolQuery(w, r, "recursive")
	if !ok {
		return
	}

	// With --root-dir=/ the root is the whole filesystem; clearing it would
	// remove the daemon, the runtime and every mount.
	if target == string(filepath.Separator) {
		s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED",
			"refusing to delete the filesystem root")
		return
	}

	if target == s.resolvedRoot {
		if !recursive {
			s.writeError(w, http.StatusConflict, "CONFLICT",
				"cannot delete the sandbox root; pass recursive=true to clear its contents")
			return
		}
		// For the sandbox root directory itself, clear its contents rather
		// than deleting the volume mount entity.
		dirEntries, err := os.ReadDir(target)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		for _, entry := range dirEntries {
			if err := os.RemoveAll(filepath.Join(target, entry.Name())); err != nil {
				s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var removeErr error
	if recursive {
		removeErr = os.RemoveAll(target)
	} else {
		removeErr = os.Remove(target)
	}
	if removeErr != nil {
		switch {
		case errors.Is(removeErr, syscall.ENOTEMPTY) || errors.Is(removeErr, syscall.EEXIST):
			s.writeError(w, http.StatusConflict, "CONFLICT",
				"directory is not empty; pass recursive=true to remove it")
		case os.IsPermission(removeErr):
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		default:
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", removeErr.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Actions of POST /v1/files/{path}:{action}.
const (
	actionMove = "move"
	actionCopy = "copy"
)

// tmpPattern names the temporary entries sandboxd creates beside a target.
const tmpPattern = ".sandboxd-tmp-*"

// errUnsupportedFileType marks a copy source that is neither a regular file,
// a directory, nor a symlink, such as a socket or a named pipe.
var errUnsupportedFileType = errors.New("unsupported file type")

// handleFileAction serves POST /v1/files/{path}:move and :copy. A net/http
// pattern cannot match a literal suffix after a wildcard, so the action is
// split off the path value here.
func (s *RESTServer) handleFileAction(w http.ResponseWriter, r *http.Request) {
	rawPath := r.PathValue("path")
	sep := strings.LastIndexByte(rawPath, ':')
	action := ""
	if sep >= 0 {
		action = rawPath[sep+1:]
	}
	if action != actionMove && action != actionCopy {
		s.writeError(w, http.StatusNotFound, "NOT_FOUND",
			fmt.Sprintf("unknown action; use :%s or :%s", actionMove, actionCopy))
		return
	}

	overwrite, ok := s.boolQuery(w, r, "overwrite")
	if !ok {
		return
	}
	destParam := r.URL.Query().Get("destination")
	if destParam == "" {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "destination is required")
		return
	}
	// A symlink in the final component is the entry to move or copy, not a
	// pointer to it.
	src, ok := s.resolveUserPath(w, rawPath[:sep], pathutil.SanitizeEntryPath)
	if !ok {
		return
	}
	dst, ok := s.resolveUserPath(w, destParam, pathutil.SanitizeEntryPath)
	if !ok {
		return
	}

	srcInfo, err := os.Lstat(src)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	if src == dst {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "source and destination are the same path")
		return
	}
	if srcInfo.IsDir() && isWithin(src, dst) {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "cannot move or copy a directory into itself")
		return
	}

	switch dstInfo, err := os.Lstat(dst); {
	case err == nil && !overwrite:
		s.writeError(w, http.StatusConflict, "CONFLICT", "destination exists; pass overwrite=true to replace a file")
		return
	case err == nil && (dstInfo.IsDir() || srcInfo.IsDir()):
		s.writeError(w, http.StatusConflict, "CONFLICT", "overwrite replaces a file with a file, never a directory")
		return
	case err != nil && !errors.Is(err, os.ErrNotExist):
		s.writeFSError(w, err)
		return
	}

	err = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err == nil {
		err = s.moveOrCopy(action, src, dst)
	}
	if errors.Is(err, errUnsupportedFileType) {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveOrCopy runs action on src and dst, which are resolved paths. Sources are
// read through a handle on the sandbox root, so a path swapped for a symlink
// during the operation cannot lead outside it.
func (s *RESTServer) moveOrCopy(action, src, dst string) error {
	root, err := os.OpenRoot(s.resolvedRoot)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if action == actionMove {
		return moveEntry(os.Rename, root, src, dst)
	}
	return copyEntry(root, src, dst)
}

// isWithin reports whether path lies strictly inside dir. Both must be clean
// absolute paths.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// moveEntry renames src to dst. rename(2) fails with EXDEV when they sit on
// different filesystems, for example when a volume is mounted under the
// sandbox root. Then it copies src and removes the original.
func moveEntry(rename func(oldPath, newPath string) error, root *os.Root, src, dst string) error {
	err := rename(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// Claim the source under a private name first: a request that replaces
	// src while the copy runs must not be deleted with the original.
	staged, err := stageEntry(src)
	if err != nil {
		return err
	}
	if err := copyEntry(root, staged, dst); err != nil {
		if _, statErr := os.Lstat(src); errors.Is(statErr, os.ErrNotExist) {
			_ = os.Rename(staged, src)
		}
		return err
	}
	return removeTree(staged)
}

// stageEntry renames src to a fresh private name in the same directory, which
// cannot fail with EXDEV, and returns that name.
func stageEntry(src string) (string, error) {
	staged := tmpPath(filepath.Dir(src))
	if err := os.Rename(src, staged); err != nil {
		return "", err
	}
	return staged, nil
}

// tmpPath returns an unused path for a temporary entry in dir.
func tmpPath(dir string) string {
	return filepath.Join(dir, strings.Replace(tmpPattern, "*", strconv.FormatUint(rand.Uint64(), 36), 1))
}

// copyEntry copies the file, symlink, or directory tree at src to dst. It
// builds a directory in a temporary sibling and renames it into place, so a
// failed copy leaves no partial dst.
func copyEntry(root *os.Root, src, dst string) error {
	rel, err := filepath.Rel(root.Name(), src)
	if err != nil {
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyLeaf(root, rel, dst, info)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), tmpPattern)
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	if err := copyTree(root, rel, tmp, info.Mode().Perm()); err != nil {
		_ = removeTree(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = removeTree(tmp)
		return fmt.Errorf("failed to move directory into place: %w", err)
	}
	return nil
}

// removeTree is os.RemoveAll after making every directory owner-writable, so a
// read-only one does not block removal.
func removeTree(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			if info, err := d.Info(); err == nil {
				_ = os.Chmod(p, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	return os.RemoveAll(path)
}

// copyTree copies the entries of directory rel, a path below root, into the
// existing directory dst, then sets dst to mode. The mode comes last so a
// read-only source directory does not block writing its children.
func copyTree(root *os.Root, rel, dst string, mode os.FileMode) error {
	dir, err := root.Open(rel)
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from, to := filepath.Join(rel, entry.Name()), filepath.Join(dst, entry.Name())
		info, err := root.Lstat(from)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := copyLeaf(root, from, to, info); err != nil {
				return err
			}
			continue
		}
		if err := os.Mkdir(to, 0o700); err != nil {
			return err
		}
		if err := copyTree(root, from, to, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return os.Chmod(dst, mode)
}

// copyLeaf copies the regular file or symlink at rel below root to dst. It
// recreates a symlink rather than following it, so a copy never reads outside
// the sandbox root through a link inside a copied tree.
func copyLeaf(root *os.Root, rel, dst string, info os.FileInfo) error {
	switch {
	case info.Mode().IsRegular():
		// A process in the sandbox can swap the file after the caller's Lstat.
		// OpenFile refuses a swapped-in symlink that leaves the sandbox root,
		// O_NONBLOCK keeps a swapped-in named pipe from blocking the open, and
		// the Stat below rejects anything that is not a regular file.
		f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !opened.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", errUnsupportedFileType, filepath.Base(rel))
		}
		return atomicWrite(dst, f, opened.Mode().Perm())
	case info.Mode()&os.ModeSymlink != 0:
		target, err := root.Readlink(rel)
		if err != nil {
			return err
		}
		return atomicSymlink(target, dst)
	default:
		return fmt.Errorf("%w: %s", errUnsupportedFileType, filepath.Base(rel))
	}
}

// atomicSymlink makes dst a symlink to target, replacing an existing file or
// symlink in one rename.
func atomicSymlink(target, dst string) error {
	tmp := tmpPath(filepath.Dir(dst))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *RESTServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		s.writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "sandboxd is shutting down")
		return
	}
	s.writeJSON(w, http.StatusOK, HealthResponse{
		Status:        "ok",
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
	})
}
