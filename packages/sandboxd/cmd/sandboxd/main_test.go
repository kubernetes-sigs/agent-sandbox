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

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthTokenDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	hexSum := hex.EncodeToString(sum[:])
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("tok\n"), 0o600))

	got, err := authTokenDigest(&config{})
	require.NoError(t, err)
	require.Nil(t, got, "no flag leaves auth off")

	got, err = authTokenDigest(&config{authTokenSHA256: hexSum})
	require.NoError(t, err)
	require.Equal(t, sum[:], got)

	got, err = authTokenDigest(&config{authTokenFile: file})
	require.NoError(t, err)
	require.Equal(t, sum[:], got)

	_, err = authTokenDigest(&config{authTokenSHA256: hexSum, authTokenFile: file})
	require.Error(t, err)
}
