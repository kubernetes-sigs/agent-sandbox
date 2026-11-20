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

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
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

func TestRunAsPolicy(t *testing.T) {
	got, err := runAsPolicy(&config{})
	require.NoError(t, err)
	require.Nil(t, got, "no flag refuses every named user")

	got, err = runAsPolicy(&config{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999", runAsDefaultUser: "1000:2000"})
	require.NoError(t, err)
	require.Equal(t, &server.RunAsPolicy{
		UIDs:       server.IDRange{Min: 1000, Max: 1999},
		GIDs:       server.IDRange{Min: 2000, Max: 2999},
		DefaultUID: 1000,
		DefaultGID: 2000,
	}, got)

	for _, cfg := range []config{
		{runAsUIDRange: "1000-1999"},
		{runAsGIDRange: "2000-2999"},
		{runAsDefaultUser: "1000:2000"},
		// Without a default user, a request that names no user would run as
		// the daemon.
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999"},
		{runAsUIDRange: "1000", runAsGIDRange: "2000-2999", runAsDefaultUser: "1000:2000"},
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2999-2000", runAsDefaultUser: "1000:2000"},
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999", runAsDefaultUser: "1000"},
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999", runAsDefaultUser: "a:b"},
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999", runAsDefaultUser: "999:2000"},
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999", runAsDefaultUser: "1000:3000"},
		{runAsUIDRange: "0-1999", runAsGIDRange: "0-2999", runAsDefaultUser: "0:2000"},
		{runAsUIDRange: "0-1999", runAsGIDRange: "0-2999", runAsDefaultUser: "1000:0"},
		// A mounted token file is often readable by every user in the sandbox,
		// and whoever reads it can run commands as every allowed user.
		{runAsUIDRange: "1000-1999", runAsGIDRange: "2000-2999", runAsDefaultUser: "1000:2000", authTokenFile: "/etc/sandboxd/token"},
	} {
		_, err = runAsPolicy(&cfg)
		require.Error(t, err, cfg)
	}
}
