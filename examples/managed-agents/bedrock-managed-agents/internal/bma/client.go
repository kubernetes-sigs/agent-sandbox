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

// Package bma is a minimal SigV4-signed REST client for the Amazon Bedrock
// Managed Agents (powered by OpenAI) preview API. The API authenticates
// with AWS SigV4 (signing service "bedrock-mantle") and requires the
// caller's account ID in the x-amzn-account-id header.
package bma

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// defaultService is the SigV4 signing service of the BMA endpoint.
	defaultService  = "bedrock-mantle"
	apiBasePath     = "/openai/v1"
	maxErrorBody    = 4096
	maxResponseBody = 1 << 20
)

// StatusError carries a non-2xx response so callers can classify failures
// without string matching.
type StatusError struct {
	StatusCode int
	Body       string
	RequestId  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("bma: HTTP %d: %s", e.StatusCode, e.Body)
}

// IsNotFound reports whether err is a 404 from BMA.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.StatusCode == 404
}

// IsUnauthorized reports whether err is a 401/403 from BMA, so callers can
// fail fast on bad credentials instead of backing off forever.
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.StatusCode == 401 || se.StatusCode == 403)
}

// Client is a SigV4-signed REST client for the BMA preview API.
type Client struct {
	cfg       aws.Config
	signer    *v4.Signer
	endpoint  string
	accountID string
	http      *http.Client
}

// Options configures NewClient. Endpoint defaults to
// https://bedrock-mantle.<Region>.api.aws; AccountID, when empty, is
// resolved via STS GetCallerIdentity; HTTPClient defaults to a 30s-timeout
// client.
type Options struct {
	Region     string // required, e.g. "us-east-1"
	Endpoint   string
	AccountID  string
	HTTPClient *http.Client
	// AWSConfig overrides the loaded aws.Config (tests inject static
	// credentials); nil loads the default credential chain.
	AWSConfig *aws.Config
}

// NewClient builds the BMA client. With no AWSConfig it loads the default
// AWS credential chain (profile, environment, or instance role).
func NewClient(ctx context.Context, opts Options) (*Client, error) {
	if opts.Region == "" {
		return nil, errors.New("bma: Region is required")
	}
	cfg := opts.AWSConfig
	if cfg == nil {
		loaded, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(opts.Region))
		if err != nil {
			return nil, fmt.Errorf("bma: loading AWS config: %w", err)
		}
		cfg = &loaded
	}
	if cfg.Region == "" {
		cfg.Region = opts.Region
	}
	endpoint := strings.TrimSuffix(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://%s.%s.api.aws", defaultService, opts.Region)
	}
	accountID := opts.AccountID
	if accountID == "" {
		// The x-amzn-account-id header is required; resolve it once from the
		// credential's own identity rather than asking the user for it.
		out, err := sts.NewFromConfig(*cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return nil, fmt.Errorf("bma: resolving account ID via STS: %w", err)
		}
		accountID = *out.Account
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		cfg:       *cfg,
		signer:    v4.NewSigner(),
		endpoint:  endpoint,
		accountID: accountID,
		http:      httpClient,
	}, nil
}

// Do sends one SigV4-signed request. path is relative to /openai/v1. A
// non-2xx response comes back as *StatusError; on success out (may be nil)
// is JSON-decoded from the response body.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body io.Reader, out any) error {
	u := c.endpoint + apiBasePath + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	// SigV4 signs the payload hash, so the body must be buffered.
	var payload []byte
	if body != nil {
		var err error
		payload, err = io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("bma: reading request body: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-amzn-account-id", c.accountID)

	sum := sha256.Sum256(payload)
	creds, err := c.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("bma: retrieving credentials: %w", err)
	}
	if err := c.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]),
		defaultService, c.cfg.Region, time.Now()); err != nil {
		return fmt.Errorf("bma: signing request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bma: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("bma: reading response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// Errors keep their status and request ID even with an oversized
		// body; only the diagnostic text is trimmed.
		if len(respBody) > maxErrorBody {
			respBody = respBody[:maxErrorBody]
		}
		return &StatusError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			RequestId:  resp.Header.Get("x-amzn-requestid"),
		}
	}
	if len(respBody) > maxResponseBody {
		// Truncated JSON would decode to a confusing error; fail explicitly.
		return fmt.Errorf("bma: response from %s exceeds %d bytes", path, maxResponseBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("bma: decoding response: %w", err)
		}
	}
	return nil
}
