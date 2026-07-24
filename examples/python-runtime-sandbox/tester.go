// Copyright 2025 The Kubernetes Authors.
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

// tester is a Go port of tester.py: it exercises the same endpoints on the
// python-runtime-sandbox server (main.py) and fails the same way (prints the
// error, exits 1) so it can be dropped in wherever tester.py is used today.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func fail(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	os.Exit(1)
}

func doGet(target string) (*http.Response, []byte) {
	resp, err := http.Get(target)
	if err != nil {
		fail("An error occurred: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fail("An error occurred reading response body: %v", err)
	}
	return resp, body
}

func doPostJSON(target string, payload any) (*http.Response, []byte) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		fail("An error occurred encoding payload: %v", err)
	}
	resp, err := http.Post(target, "application/json", bytes.NewReader(encoded))
	if err != nil {
		fail("An error occurred: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fail("An error occurred reading response body: %v", err)
	}
	return resp, body
}

func doPostUpload(target, filename string, content []byte) (*http.Response, []byte) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		fail("An error occurred creating form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		fail("An error occurred writing form file: %v", err)
	}
	if err := w.Close(); err != nil {
		fail("An error occurred closing multipart writer: %v", err)
	}

	resp, err := http.Post(target, w.FormDataContentType(), &buf)
	if err != nil {
		fail("An error occurred: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fail("An error occurred reading response body: %v", err)
	}
	return resp, body
}

func decodeJSON(body []byte, v any) {
	if err := json.Unmarshal(body, v); err != nil {
		fail("An error occurred decoding JSON: %v (body: %s)", err, body)
	}
}

// shlexQuote mirrors Python's shlex.quote: wraps s in single quotes (escaping
// any embedded single quotes) unless every character is already shell-safe.
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("@%+=:,./-_", r):
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func testHealthCheck(baseURL string) {
	target := baseURL + "/"
	fmt.Println("--- Testing Health Check endpoint ---")
	fmt.Printf("Sending GET request to %s\n", target)
	resp, body := doGet(target)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during health check: status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Status string `json:"status"`
	}
	decodeJSON(body, &result)
	fmt.Println("Health check successful!")
	fmt.Printf("Response JSON: %s\n", body)
	if result.Status != "ok" {
		fail("An error occurred during health check: status field was %q, want \"ok\"", result.Status)
	}
}

func testExecute(baseURL string) {
	target := baseURL + "/execute"
	payload := map[string]string{"command": "echo 'hello world'"}
	fmt.Println("\n--- Testing Execute endpoint ---")
	fmt.Printf("Sending POST request to %s with payload: %v\n", target, payload)
	resp, body := doPostJSON(target, payload)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during execute command: status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Stdout string `json:"stdout"`
	}
	decodeJSON(body, &result)
	fmt.Println("Execute command successful!")
	fmt.Printf("Response JSON: %s\n", body)
	if result.Stdout != "hello world\n" {
		fail("An error occurred during execute command: stdout was %q, want \"hello world\\n\"", result.Stdout)
	}
}

func testListFiles(baseURL string) {
	target := baseURL + "/list/."
	fmt.Println("\n--- Testing List Files endpoint ---")
	fmt.Printf("Sending GET request to %s\n", target)
	resp, body := doGet(target)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during list files: status %d: %s", resp.StatusCode, body)
	}
	var result []any
	decodeJSON(body, &result)
	fmt.Println("List files successful!")
	fmt.Printf("Response JSON: %s\n", body)
}

func testExists(baseURL string) {
	target := baseURL + "/exists/."
	fmt.Println("\n--- Testing Exists endpoint ---")
	fmt.Printf("Sending GET request to %s\n", target)
	resp, body := doGet(target)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during exists check: status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Path   string `json:"path"`
		Exists bool   `json:"exists"`
	}
	decodeJSON(body, &result)
	fmt.Println("Exists check successful!")
	fmt.Printf("Response JSON: %s\n", body)
	if result.Path != "" || !result.Exists {
		fail("An error occurred during exists check: got path=%q exists=%v, want path=\"\" exists=true", result.Path, result.Exists)
	}

	target = baseURL + "/exists/does_not_exist"
	fmt.Printf("Sending GET request to %s\n", target)
	resp, body = doGet(target)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during exists check: status %d: %s", resp.StatusCode, body)
	}
	decodeJSON(body, &result)
	fmt.Println("Exists check (negative) successful!")
	fmt.Printf("Response JSON: %s\n", body)
	if result.Path != "does_not_exist" || result.Exists {
		fail("An error occurred during exists check: got path=%q exists=%v, want path=\"does_not_exist\" exists=false", result.Path, result.Exists)
	}
}

func testPathTraversal(baseURL string) {
	unsafePath := "../../etc/passwd"
	target := baseURL + "/exists/" + url.PathEscape(unsafePath)
	fmt.Println("\n--- Testing Path Traversal ---")
	fmt.Printf("Sending GET request to %s\n", target)
	resp, body := doGet(target)
	fmt.Printf("Response status code: %d\n", resp.StatusCode)
	fmt.Printf("Response JSON: %s\n", body)
	var result struct {
		Message string `json:"message"`
	}
	decodeJSON(body, &result)
	if resp.StatusCode != http.StatusForbidden || result.Message != "Access denied" {
		fail("An error occurred during path traversal check: got status=%d message=%q, want status=403 message=\"Access denied\"", resp.StatusCode, result.Message)
	}
	fmt.Println("Path traversal blocked successfully!")
}

func testAbsolutePathTraversal(baseURL string) {
	unsafePath := "/../etc/passwd"
	target := baseURL + "/exists/" + url.PathEscape(unsafePath)
	fmt.Println("\n--- Testing Absolute Path Traversal ---")
	fmt.Printf("Sending GET request to %s\n", target)
	resp, body := doGet(target)
	fmt.Printf("Response status code: %d\n", resp.StatusCode)
	fmt.Printf("Response JSON: %s\n", body)
	var result struct {
		Message string `json:"message"`
	}
	decodeJSON(body, &result)
	if resp.StatusCode != http.StatusForbidden || result.Message != "Access denied" {
		fail("An error occurred during absolute path traversal check: got status=%d message=%q, want status=403 message=\"Access denied\"", resp.StatusCode, result.Message)
	}
	fmt.Println("Absolute path traversal blocked successfully!")
}

func testUpload(baseURL string) {
	filename := "test_upload.txt"
	fileContent := []byte("Hello world from upload test")

	targetUpload := baseURL + "/upload"
	fmt.Println("\n--- Testing Upload endpoint ---")
	fmt.Printf("Sending POST request to %s\n", targetUpload)
	resp, body := doPostUpload(targetUpload, filename, fileContent)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during upload verification: status %d: %s", resp.StatusCode, body)
	}
	var uploadResult struct {
		Message string `json:"message"`
	}
	decodeJSON(body, &uploadResult)
	fmt.Println("Upload successful!")
	fmt.Printf("Response JSON: %s\n", body)
	if !strings.Contains(uploadResult.Message, "uploaded successfully") {
		fail("An error occurred during upload verification: message %q did not contain \"uploaded successfully\"", uploadResult.Message)
	}

	// 1. Verify file exists
	targetExists := baseURL + "/exists/" + filename
	fmt.Printf("Checking if file exists via GET %s\n", targetExists)
	_, body = doGet(targetExists)
	var existsResult struct {
		Exists bool `json:"exists"`
	}
	decodeJSON(body, &existsResult)
	if !existsResult.Exists {
		fail("An error occurred during upload verification: uploaded file does not exist")
	}
	fmt.Println("File existence verified successfully!")

	// 2. Download the file and verify content
	targetDownload := baseURL + "/download/" + filename
	fmt.Printf("Downloading file via GET %s\n", targetDownload)
	_, downloaded := doGet(targetDownload)
	if !bytes.Equal(downloaded, fileContent) {
		fail("An error occurred during upload verification: downloaded content %q did not match uploaded content %q", downloaded, fileContent)
	}
	fmt.Println("Downloaded file content verified successfully!")

	// 3. Clean up the uploaded file
	targetExecute := baseURL + "/execute"
	fmt.Printf("Cleaning up uploaded file via POST %s\n", targetExecute)
	doPostJSON(targetExecute, map[string]string{"command": "rm " + filename})
	fmt.Println("File cleanup completed successfully!")
}

func testUploadPathTraversal(baseURL string) {
	target := baseURL + "/upload"
	fmt.Println("\n--- Testing Upload Path Traversal ---")
	fmt.Printf("Sending POST request to %s with unsafe filename\n", target)
	resp, body := doPostUpload(target, "../../unsafe_upload.txt", []byte("malicious payload"))
	fmt.Printf("Response status code: %d\n", resp.StatusCode)
	fmt.Printf("Response JSON: %s\n", body)
	var result struct {
		Message string `json:"message"`
	}
	decodeJSON(body, &result)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(result.Message, "Access denied") {
		fail("An error occurred during upload path traversal check: got status=%d message=%q, want status=403 message containing \"Access denied\"", resp.StatusCode, result.Message)
	}
	fmt.Println("Upload path traversal blocked successfully!")
}

// testMLLibraries checks that pandas, scikit-learn, and lightgbm import and
// run inside the sandbox. lightgbm in particular requires the OpenMP runtime
// (libgomp), which is not present in the slim base image by default, so this
// catches runtime-image regressions such as a missing libgomp.so.1.
func testMLLibraries(baseURL string) {
	target := baseURL + "/execute"
	script := "import pandas as pd, lightgbm as lgb; " +
		"from sklearn.linear_model import LinearRegression; " +
		"m = LinearRegression().fit([[0], [1]], [0, 1]); " +
		"print('ml-ok', int(round(m.predict([[2]])[0])))"
	payload := map[string]string{"command": "python -c " + shlexQuote(script)}

	fmt.Println("\n--- Testing ML libraries ---")
	fmt.Printf("Sending POST request to %s\n", target)
	resp, body := doPostJSON(target, payload)
	if resp.StatusCode != http.StatusOK {
		fail("An error occurred during ML libraries check: status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	decodeJSON(body, &result)
	fmt.Println("ML libraries check completed!")
	fmt.Printf("Response JSON: %s\n", body)
	if result.ExitCode != 0 {
		fail("An error occurred during ML libraries check: %s", result.Stderr)
	}
	if !strings.Contains(result.Stdout, "ml-ok 2") {
		fail("An error occurred during ML libraries check: stdout %q did not contain \"ml-ok 2\"", result.Stdout)
	}
	fmt.Println("ML libraries imported and ran successfully!")
}

func main() {
	if len(os.Args) != 3 {
		fmt.Println("Usage: go run tester.go <server_ip> <server_port>")
		os.Exit(1)
	}
	baseURL := fmt.Sprintf("http://%s:%s", os.Args[1], os.Args[2])

	testHealthCheck(baseURL)
	testExecute(baseURL)
	testListFiles(baseURL)
	testExists(baseURL)
	testPathTraversal(baseURL)
	testAbsolutePathTraversal(baseURL)
	testUpload(baseURL)
	testUploadPathTraversal(baseURL)
	testMLLibraries(baseURL)
}
