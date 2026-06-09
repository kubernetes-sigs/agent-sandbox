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

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// Store defines the interface for chat session persistence.
type Store interface {
	// LoadSession retrieves all messages for the given session ID.
	// If the session does not exist, it returns a nil slice and no error.
	LoadSession(ctx context.Context, sessionID string) ([]llm.Message, error)

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

// LoadSession reads all messages from the JSONL session file.
func (s *FileStore) LoadSession(ctx context.Context, sessionID string) ([]llm.Message, error) {
	log := klog.FromContext(ctx)

	if err := ValidateSessionName(sessionID); err != nil {
		return nil, err
	}
	p := filepath.Join(s.basedir, sessionID, "sessions", "latest.jsonl")
	log.V(2).Info("loading session", "path", p)
	file, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open session file %s: %w", p, err)
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
			return nil, fmt.Errorf("failed to decode message from session file: %w", err)
		}
		messages = append(messages, msg)
	}

	return messages, nil
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

	p := filepath.Join(s.basedir, sessionID, "sessions", "latest.jsonl")
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
