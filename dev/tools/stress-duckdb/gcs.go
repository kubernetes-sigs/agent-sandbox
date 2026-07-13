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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type gcsListResponse struct {
	Prefixes []string `json:"prefixes"`
	Items    []struct {
		Name string `json:"name"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

type finishedJSON struct {
	Passed   bool   `json:"passed"`
	Result   string `json:"result"`
	Revision string `json:"revision"`
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

func gcsGet(ctx context.Context, objectPath string) ([]byte, error) {
	u := "https://storage.googleapis.com/" + gcsBucket + "/" + objectPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return body, nil
}

func gcsExists(ctx context.Context, objectPath string) bool {
	u := "https://storage.googleapis.com/" + gcsBucket + "/" + objectPath
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func listBuildIDs(ctx context.Context, pr int, job string) ([]string, error) {
	prefix := jobPrefix(pr, job)
	var builds []string
	pageToken := ""
	for {
		q := url.Values{
			"prefix":    {prefix},
			"delimiter": {"/"},
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		u := "https://storage.googleapis.com/storage/v1/b/" + gcsBucket + "/o?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		var list gcsListResponse
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list builds: %s", resp.Status)
		}
		for _, p := range list.Prefixes {
			// pr-logs/.../JOB/<buildID>/
			p = strings.TrimSuffix(p, "/")
			id := p[strings.LastIndex(p, "/")+1:]
			if _, err := strconv.ParseUint(id, 10, 64); err == nil {
				builds = append(builds, id)
			}
		}
		if list.NextPageToken == "" {
			break
		}
		pageToken = list.NextPageToken
	}
	sort.Slice(builds, func(i, j int) bool {
		// Numeric descending: newest prow build ids are larger.
		ai, _ := strconv.ParseUint(builds[i], 10, 64)
		aj, _ := strconv.ParseUint(builds[j], 10, 64)
		return ai > aj
	})
	return builds, nil
}

// latestSuccessfulBuild returns the newest build id for the PR/job that
// finished successfully and has stress-test/summary.json.
func latestSuccessfulBuild(ctx context.Context, pr int, job string) (string, error) {
	// Prefer prow's latest-build.txt pointer, then fall back to listing.
	candidates := []string{}
	if raw, err := gcsGet(ctx, jobPrefix(pr, job)+"latest-build.txt"); err == nil {
		id := strings.TrimSpace(string(raw))
		if id != "" {
			candidates = append(candidates, id)
		}
	}
	listed, err := listBuildIDs(ctx, pr, job)
	if err != nil {
		if len(candidates) == 0 {
			return "", fmt.Errorf("listing builds for PR %d job %s: %w", pr, job, err)
		}
	} else {
		for _, id := range listed {
			if len(candidates) == 0 || id != candidates[0] {
				candidates = append(candidates, id)
			}
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no builds found for PR %d job %s", pr, job)
	}

	var errs []string
	for _, id := range candidates {
		ok, reason := buildHasStressArtifacts(ctx, pr, job, id)
		if ok {
			return id, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %s", id, reason))
	}
	return "", fmt.Errorf("no successful stress-test run found for PR %d:\n  %s", pr, strings.Join(errs, "\n  "))
}

func buildHasStressArtifacts(ctx context.Context, pr int, job, buildID string) (bool, string) {
	prefix := jobPrefix(pr, job) + buildID + "/"
	raw, err := gcsGet(ctx, prefix+"finished.json")
	if err != nil {
		return false, "no finished.json (still running?)"
	}
	var fin finishedJSON
	if err := json.Unmarshal(raw, &fin); err != nil {
		return false, "bad finished.json"
	}
	if !fin.Passed && fin.Result != "SUCCESS" {
		return false, fmt.Sprintf("result=%s", fin.Result)
	}
	if !gcsExists(ctx, prefix+"artifacts/stress-test/summary.json") {
		return false, "no artifacts/stress-test/summary.json"
	}
	return true, ""
}
