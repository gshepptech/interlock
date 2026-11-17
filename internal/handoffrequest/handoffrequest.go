// Package handoffrequest stores bounded requests for another agent or team to
// continue work. Full handoff packets belong to the task layer introduced later.
package handoffrequest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gshepptech/interlock/internal/lockfile"
	"github.com/gshepptech/interlock/internal/state"
)

const (
	MaxRequests = 100
	MaxNote     = 1024
)

var (
	ErrNotFound = errors.New("handoff request not found")
	ErrCapacity = errors.New("handoff request queue is full")
	ErrInvalid  = errors.New("invalid handoff request")
)

type TargetKind string

const (
	Agent TargetKind = "agent"
	Team  TargetKind = "team"
)

type Requester string

const (
	CLI       Requester = "cli"
	Dashboard Requester = "dashboard"
)

type Create struct {
	SourceSession string
	TargetKind    TargetKind
	Target        string
	Note          string
	Requester     Requester
}

type Request struct {
	Version       int        `json:"version"`
	ID            string     `json:"id"`
	SourceSession string     `json:"source_session"`
	TargetKind    TargetKind `json:"target_kind"`
	Target        string     `json:"target"`
	Note          string     `json:"note"`
	Requester     Requester  `json:"requester"`
	CreatedAt     time.Time  `json:"created_at"`
}

type Resolution struct {
	Version    int       `json:"version"`
	RequestID  string    `json:"request_id"`
	Resolver   Requester `json:"resolver"`
	ResolvedAt time.Time `json:"resolved_at"`
}

type Item struct {
	Request    Request
	Resolution *Resolution
}

func (i Item) Open() bool { return i.Resolution == nil }

type Store struct {
	dir   string
	now   func() time.Time
	newID func() (string, error)
}

func Open(root string) *Store {
	return &Store{
		dir: filepath.Join(root, ".interlock", "handoffs", "requests"),
		now: time.Now, newID: randomID,
	}
}

func (s *Store) Create(input Create) (Request, bool, error) {
	if err := validateCreate(input); err != nil {
		return Request{}, false, err
	}
	unlock, err := s.lock()
	if err != nil {
		return Request{}, false, err
	}
	defer unlock()

	items, files, err := s.listLocked()
	if err != nil {
		return Request{}, false, err
	}
	for _, item := range items {
		if item.Open() && sameRequest(item.Request, input) {
			return item.Request, false, nil
		}
	}
	if err := s.pruneLocked(items, files); err != nil {
		return Request{}, false, err
	}
	files, err = s.requestFiles()
	if err != nil {
		return Request{}, false, err
	}
	if len(files) >= MaxRequests {
		return Request{}, false, ErrCapacity
	}

	for range 4 {
		id, err := s.newID()
		if err != nil {
			return Request{}, false, err
		}
		request := Request{
			Version: state.CurrentVersion, ID: id, SourceSession: input.SourceSession,
			TargetKind: input.TargetKind, Target: input.Target, Note: input.Note,
			Requester: input.Requester, CreatedAt: s.now(),
		}
		err = writeExclusive(s.requestPath(id), request)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return request, err == nil, err
	}
	return Request{}, false, errors.New("generate unique handoff request id")
}

func (s *Store) Resolve(id string, resolver Requester) (Resolution, bool, error) {
	if !validID(id, 128) {
		return Resolution{}, false, fmt.Errorf("%w: invalid request id", ErrInvalid)
	}
	if !resolver.known() {
		return Resolution{}, false, fmt.Errorf("%w: invalid resolver", ErrInvalid)
	}
	unlock, err := s.lock()
	if err != nil {
		return Resolution{}, false, err
	}
	defer unlock()
	if _, err := readRequest(s.requestPath(id)); errors.Is(err, os.ErrNotExist) {
		return Resolution{}, false, ErrNotFound
	} else if err != nil {
		return Resolution{}, false, err
	}
	if existing, err := readResolution(s.resolutionPath(id)); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolution{}, false, err
	}
	resolution := Resolution{
		Version: state.CurrentVersion, RequestID: id,
		Resolver: resolver, ResolvedAt: s.now(),
	}
	if err := writeExclusive(s.resolutionPath(id), resolution); err != nil {
		return Resolution{}, false, err
	}
	return resolution, true, nil
}

func (s *Store) List() ([]Item, error) {
	items, _, err := s.listLocked()
	return items, err
}

func (s *Store) Show(id string) (Item, error) {
	if !validID(id, 128) {
		return Item{}, ErrNotFound
	}
	request, err := readRequest(s.requestPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	item := Item{Request: request}
	if resolution, err := readResolution(s.resolutionPath(id)); err == nil {
		item.Resolution = &resolution
	} else if !errors.Is(err, os.ErrNotExist) {
		return Item{}, err
	}
	return item, nil
}

func (s *Store) Inspect() ([]state.Issue, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read handoff request directory: %w", err)
	}
	var issues []state.Issue
	requests := map[string]bool{}
	type resolutionFile struct {
		path string
		item Resolution
	}
	var resolutions []resolutionFile
	for _, entry := range entries {
		path := filepath.Join(s.dir, entry.Name())
		var readErr error
		switch {
		case strings.HasSuffix(entry.Name(), ".request.json"):
			request, err := readRequest(path)
			readErr = err
			if err == nil {
				requests[request.ID] = true
			}
		case strings.HasSuffix(entry.Name(), ".resolution.json"):
			resolution, err := readResolution(path)
			readErr = err
			if err == nil {
				resolutions = append(resolutions, resolutionFile{path: path, item: resolution})
			}
		default:
			continue
		}
		if readErr != nil {
			issues = append(issues, state.Issue{Store: "handoffrequest", Path: path, Err: readErr})
		}
	}
	for _, resolution := range resolutions {
		if !requests[resolution.item.RequestID] {
			issues = append(issues, state.Issue{
				Store: "handoffrequest", Path: resolution.path,
				Err: fmt.Errorf("orphan resolution for missing request %s", resolution.item.RequestID),
			})
		}
	}
	return issues, nil
}

func (s *Store) listLocked() ([]Item, []string, error) {
	files, err := s.requestFiles()
	if err != nil {
		return nil, nil, err
	}
	items := make([]Item, 0, len(files))
	for _, file := range files {
		request, err := readRequest(file)
		if err != nil {
			continue
		}
		item := Item{Request: request}
		if resolution, err := readResolution(s.resolutionPath(request.ID)); err == nil {
			item.Resolution = &resolution
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b Item) int {
		return b.Request.CreatedAt.Compare(a.Request.CreatedAt)
	})
	return items, files, nil
}

func (s *Store) requestFiles() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read handoff request directory: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".request.json") {
			files = append(files, filepath.Join(s.dir, entry.Name()))
		}
	}
	return files, nil
}

func (s *Store) pruneLocked(items []Item, files []string) error {
	if len(files) < MaxRequests {
		return nil
	}
	resolved := slices.DeleteFunc(slices.Clone(items), func(item Item) bool { return item.Open() })
	slices.SortFunc(resolved, func(a, b Item) int {
		return a.Request.CreatedAt.Compare(b.Request.CreatedAt)
	})
	count := len(files)
	for _, item := range resolved {
		if count < MaxRequests {
			break
		}
		if err := os.Remove(s.requestPath(item.Request.ID)); err != nil {
			return fmt.Errorf("prune handoff request: %w", err)
		}
		if err := os.Remove(s.resolutionPath(item.Request.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prune handoff resolution: %w", err)
		}
		count--
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create handoff request directory: %w", err)
	}
	return lockfile.Acquire(filepath.Join(s.dir, ".lock"))
}

func (s *Store) requestPath(id string) string {
	return filepath.Join(s.dir, id+".request.json")
}

func (s *Store) resolutionPath(id string) string {
	return filepath.Join(s.dir, id+".resolution.json")
}

func readRequest(path string) (Request, error) {
	var request Request
	raw, err := os.ReadFile(path)
	if err != nil {
		return Request{}, err
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return Request{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := state.Validate("handoffrequest", path, request.Version); err != nil {
		return Request{}, err
	}
	expectedID, ok := fileID(path, ".request.json")
	if !ok || request.ID != expectedID || !validID(request.ID, 128) || !validID(request.SourceSession, 128) ||
		(request.TargetKind != Agent && request.TargetKind != Team) ||
		!validID(request.Target, 64) || !request.Requester.known() ||
		!utf8.ValidString(request.Note) || len(request.Note) > MaxNote {
		return Request{}, fmt.Errorf("handoff request %s has invalid fields", path)
	}
	return request, nil
}

func readResolution(path string) (Resolution, error) {
	var resolution Resolution
	raw, err := os.ReadFile(path)
	if err != nil {
		return Resolution{}, err
	}
	if err := json.Unmarshal(raw, &resolution); err != nil {
		return Resolution{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := state.Validate("handoffrequest", path, resolution.Version); err != nil {
		return Resolution{}, err
	}
	expectedID, ok := fileID(path, ".resolution.json")
	if !ok || resolution.RequestID != expectedID || !validID(resolution.RequestID, 128) || !resolution.Resolver.known() ||
		resolution.ResolvedAt.IsZero() {
		return Resolution{}, fmt.Errorf("handoff resolution %s has invalid fields", path)
	}
	return resolution, nil
}

func fileID(path, suffix string) (string, bool) {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(name, suffix)
	return id, validID(id, 128)
}

func writeExclusive(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode handoff record: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create handoff temp: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write handoff temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close handoff temp: %w", err)
	}
	if err := os.Link(name, path); err != nil {
		return err
	}
	return nil
}

func validateCreate(input Create) error {
	if !validID(input.SourceSession, 128) {
		return fmt.Errorf("%w: invalid source session id", ErrInvalid)
	}
	if input.TargetKind != Agent && input.TargetKind != Team {
		return fmt.Errorf("%w: target kind must be agent or team", ErrInvalid)
	}
	if !validID(input.Target, 64) {
		return fmt.Errorf("%w: invalid target", ErrInvalid)
	}
	if !input.Requester.known() {
		return fmt.Errorf("%w: invalid requester", ErrInvalid)
	}
	if !utf8.ValidString(input.Note) || len(input.Note) > MaxNote {
		return fmt.Errorf("%w: note must be valid UTF-8 and at most %d bytes", ErrInvalid, MaxNote)
	}
	return nil
}

func (r Requester) known() bool { return r == CLI || r == Dashboard }

func validID(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("-_.:", r) {
			continue
		}
		return false
	}
	return true
}

func sameRequest(request Request, input Create) bool {
	return request.SourceSession == input.SourceSession && request.TargetKind == input.TargetKind &&
		request.Target == input.Target && request.Note == input.Note
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate handoff request id: %w", err)
	}
	return "hr-" + hex.EncodeToString(raw[:]), nil
}
