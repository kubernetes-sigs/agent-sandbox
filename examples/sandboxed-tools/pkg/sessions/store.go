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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// Store defines the interface for chat session persistence.
type Store interface {
	// LoadSession retrieves all messages for the given session ID.
	// If the session does not exist, it returns a nil slice and no error.
	LoadSession(ctx context.Context, sessionID string) ([]llm.Message, error)

	// AppendMessage appends a single message to the session's history.
	AppendMessage(ctx context.Context, sessionID string, message llm.Message) error
}

// FileStore is a JSONL implementation of the Store interface.
type FileStore struct {
	dir string
}

// NewFileStore creates a new FileStore with the given base directory.
func NewFileStore(dir string) *FileStore {
	return &FileStore{
		dir: dir,
	}
}

// ensureDir ensures that the session directory exists.
func (s *FileStore) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("failed to create session directory %s: %w", s.dir, err)
	}
	return nil
}

// LoadSession reads all messages from the JSONL session file.
func (s *FileStore) LoadSession(ctx context.Context, sessionID string) ([]llm.Message, error) {
	filename := filepath.Join(s.dir, sessionID+".jsonl")
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open session file %s: %w", filename, err)
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

// AppendMessage marshals and appends a single message to the session file.
func (s *FileStore) AppendMessage(ctx context.Context, sessionID string, msg llm.Message) error {
	if err := s.ensureDir(); err != nil {
		return err
	}

	filename := filepath.Join(s.dir, sessionID+".jsonl")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("failed to open session file %s: %w", filename, err)
	}
	defer file.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message to json: %w", err)
	}

	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write message to session file: %w", err)
	}

	return nil
}
