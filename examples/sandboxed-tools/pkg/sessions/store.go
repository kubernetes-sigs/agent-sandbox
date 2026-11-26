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

package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// ErrNotFound is returned (wrapped) by Store.LoadSession when no session
// with the given ID has been persisted.
var ErrNotFound = errors.New("session not found")

// Metadata describes a session beyond its message history: where it came
// from and how it runs. It is written once, when the session is created.
type Metadata struct {
	// Parent is the name of the session that created this one, or "" for a
	// top-level session (one started by a user rather than by another
	// session).
	Parent string `json:"parent,omitempty"`

	// Toolset is the name of the toolset (tools + system prompt) the session
	// runs with, so that a resumed session gets the tools its history was
	// recorded against.
	Toolset string `json:"toolset,omitempty"`

	// CreatedAt is when the session was created.
	CreatedAt time.Time `json:"createdAt"`
}

// Store defines the interface for chat session persistence.
type Store interface {
	// NewSession creates a new session with the given ID and metadata.
	// It returns an error if a session with the given ID already exists.
	NewSession(ctx context.Context, sessionID string, metadata Metadata) error

	// LoadSession retrieves all messages and metadata for the given session ID.
	// If the session does not exist, it returns an error wrapping
	// ErrNotFound, so callers can tell "new session" from "typo" apart.
	// If the session has messages but no stored metadata (e.g. a session
	// recorded before metadata existed), an empty Metadata is returned.
	LoadSession(ctx context.Context, sessionID string) ([]llm.Message, *Metadata, error)

	// AppendMessages appends a set of messages to the session's history.
	AppendMessages(ctx context.Context, sessionID string, messages ...llm.Message) error
}

// FileStore is a JSONL implementation of the Store interface.
type FileStore struct {
	basedir string
}

var _ Store = &FileStore{}

// NewFileStore creates a new FileStore with the given base directory.
func NewFileStore(dir string) *FileStore {
	return &FileStore{
		basedir: dir,
	}
}

// ensureDir ensures that the session directory exists.
func (s *FileStore) ensureDir(sessionID string) error {
	dir := filepath.Join(s.basedir, sessionID, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create session directory %s: %w", dir, err)
	}
	return nil
}

// metadataPath is the session's metadata file, a sibling of the sessions/
// history directory.
func (s *FileStore) metadataPath(sessionID string) string {
	return filepath.Join(s.basedir, sessionID, "metadata.json")
}

// messagesPath is the session's JSONL message history file.
func (s *FileStore) messagesPath(sessionID string) string {
	return filepath.Join(s.basedir, sessionID, "sessions", "latest.jsonl")
}

// loadMetadata reads the session's metadata file, returning nil, nil if none
// was saved.
func (s *FileStore) loadMetadata(ctx context.Context, sessionID string) (*Metadata, error) {
	log := klog.FromContext(ctx)

	if err := ValidateSessionName(sessionID); err != nil {
		return nil, err
	}
	p := s.metadataPath(sessionID)
	log.V(2).Info("loading session metadata", "path", p)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read session metadata %s: %w", p, err)
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to decode session metadata %s: %w", p, err)
	}
	return &metadata, nil
}

// NewSession creates a new session and writes its metadata file. It returns an
// error if a session with the given ID already exists.
func (s *FileStore) NewSession(ctx context.Context, sessionID string, metadata Metadata) error {
	log := klog.FromContext(ctx)

	if err := ValidateSessionName(sessionID); err != nil {
		return err
	}

	for _, p := range []string{s.metadataPath(sessionID), s.messagesPath(sessionID)} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("session %q already exists", sessionID)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat %s: %w", p, err)
		}
	}

	if err := s.ensureDir(sessionID); err != nil {
		return err
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal session metadata to json: %w", err)
	}

	p := s.metadataPath(sessionID)
	log.V(2).Info("saving session metadata", "path", p)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("session %q already exists", sessionID)
		}
		return fmt.Errorf("failed to create session metadata %s: %w", p, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return fmt.Errorf("failed to write session metadata %s: %w", p, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		return fmt.Errorf("failed to close session metadata %s: %w", p, err)
	}
	return nil
}

// LoadSession reads all messages and metadata from the session store.
func (s *FileStore) LoadSession(ctx context.Context, sessionID string) ([]llm.Message, *Metadata, error) {
	log := klog.FromContext(ctx)

	if err := ValidateSessionName(sessionID); err != nil {
		return nil, nil, err
	}

	metadata, err := s.loadMetadata(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}

	p := s.messagesPath(sessionID)
	log.V(2).Info("loading session", "path", p)
	file, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			if metadata != nil {
				return nil, metadata, nil
			}
			return nil, nil, fmt.Errorf("session %q: %w", sessionID, ErrNotFound)
		}
		return nil, nil, fmt.Errorf("failed to open session file %s: %w", p, err)
	}
	defer file.Close()

	var messages []llm.Message
	decoder := json.NewDecoder(file)
	for {
		var msg llm.Message
		if err := decoder.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("failed to decode message from session file: %w", err)
		}
		messages = append(messages, msg)
	}

	if metadata == nil {
		metadata = &Metadata{}
	}

	return messages, metadata, nil
}

// AppendMessages marshals and appends a set of messages to the session file.
func (s *FileStore) AppendMessages(ctx context.Context, sessionID string, messages ...llm.Message) error {
	log := klog.FromContext(ctx)

	if err := ValidateSessionName(sessionID); err != nil {
		return err
	}

	if err := s.ensureDir(sessionID); err != nil {
		return err
	}

	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return fmt.Errorf("failed to marshal message to json: %w", err)
		}

		// Note: json.Encoder automatically adds a \n character, no need to add it manually
	}

	p := s.messagesPath(sessionID)
	log.V(2).Info("appending to session", "path", p)
	file, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("failed to open session file %s: %w", p, err)
	}
	defer file.Close()

	if _, err := b.WriteTo(file); err != nil {
		return fmt.Errorf("failed to write messages to session file: %w", err)
	}

	return nil
}
