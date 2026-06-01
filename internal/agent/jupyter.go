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

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

type JupyterServer struct {
	pb.UnimplementedJupyterServiceServer
	
	jupyterPort string
	jupyterURL  string
	cmd         *exec.Cmd
	running     bool
	mutex       sync.Mutex

	// Map from our session_id to Jupyter's kernel_id
	sessions map[string]string
}

func NewJupyterServer() *JupyterServer {
	return &JupyterServer{
		jupyterPort: "8888",
		jupyterURL:  "http://127.0.0.1:8888",
		sessions:    make(map[string]string),
	}
}

// ensureJupyterRunning starts a local tokenless Jupyter Server in the background if not already running
func (s *JupyterServer) ensureJupyterRunning() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.running {
		return nil
	}

	// Spawn jupyter server process
	// We disable authentication tokens for secure local-only Unix/localhost loopback connection inside the sandbox pod
	cmd := exec.Command("jupyter", "server",
		"--ip=127.0.0.1",
		fmt.Sprintf("--port=%s", s.jupyterPort),
		"--ServerApp.token=",
		"--ServerApp.password=",
		"--no-browser",
		"--allow-root",
		"--ServerApp.root_dir=/tmp",
		"--ServerApp.disable_check_xsrf=True",
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start jupyter server: %w. Make sure 'jupyter-server' is installed in the container", err)
	}

	s.cmd = cmd
	s.running = true

	// Wait for the server to become healthy
	var healthy bool
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		resp, err := http.Get(fmt.Sprintf("%s/api/kernels", s.jupyterURL))
		if err == nil {
			// Status 200 means API is ready.
			if resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				healthy = true
				break
			}
			resp.Body.Close()
		}
	}

	if !healthy {
		_ = s.cmd.Process.Kill()
		s.running = false
		return fmt.Errorf("jupyter server did not become healthy within timeout")
	}

	return nil
}

// CreateSession spawns a persistent Python interpreter session via Jupyter
func (s *JupyterServer) CreateSession(ctx context.Context, req *pb.CreateJupyterSessionRequest) (*pb.JupyterSession, error) {
	if err := s.ensureJupyterRunning(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to initialize jupyter runtime: %v", err)
	}

	kernelName := req.KernelName
	if kernelName == "" {
		kernelName = "python3"
	}

	// Create session payload
	sessionUUID := uuid.New().String()
	payload := map[string]interface{}{
		"kernel": map[string]interface{}{
			"name": kernelName,
		},
		"name": sessionUUID,
		"type": "notebook",
		"path": sessionUUID + ".ipynb",
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal payload: %v", err)
	}

	resp, err := http.Post(
		fmt.Sprintf("%s/api/sessions", s.jupyterURL),
		"application/json",
		bytes.NewBuffer(jsonPayload),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, status.Errorf(codes.Internal, "jupyter returned error status %d: %s", resp.StatusCode, string(body))
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to decode jupyter response: %v", err)
	}

	kernelData, ok := respData["kernel"].(map[string]interface{})
	if !ok {
		return nil, status.Error(codes.Internal, "invalid kernel response from jupyter")
	}

	kernelID, ok := kernelData["id"].(string)
	if !ok {
		return nil, status.Error(codes.Internal, "missing kernel ID in jupyter response")
	}

	s.mutex.Lock()
	s.sessions[sessionUUID] = kernelID
	s.mutex.Unlock()

	return &pb.JupyterSession{
		SessionId: sessionUUID,
		Status:    "idle",
	}, nil
}

// ExecuteCode runs Python commands inside an active session, returning rich outputs
func (s *JupyterServer) ExecuteCode(ctx context.Context, req *pb.ExecuteJupyterCodeRequest) (*pb.ExecuteJupyterCodeResponse, error) {
	s.mutex.Lock()
	kernelID, ok := s.sessions[req.SessionId]
	s.mutex.Unlock()

	if !ok {
		return nil, status.Errorf(codes.NotFound, "session %s not found", req.SessionId)
	}

	// Connect to the kernel's WebSocket channels
	wsURL := fmt.Sprintf("ws://127.0.0.1:%s/api/kernels/%s/channels", s.jupyterPort, kernelID)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to connect to kernel websocket: %v", err)
	}
	defer conn.Close()

	// Send execution request
	msgID := uuid.New().String()
	execMsg := map[string]interface{}{
		"header": map[string]interface{}{
			"msg_id":   msgID,
			"username": "username",
			"session":  req.SessionId,
			"msg_type": "execute_request",
			"version":  "5.3",
		},
		"parent_header": map[string]interface{}{},
		"metadata":      map[string]interface{}{},
		"content": map[string]interface{}{
			"code":             req.Code,
			"silent":           false,
			"store_history":    true,
			"user_expressions": map[string]interface{}{},
			"allow_stdin":      false,
			"stop_on_error":    true,
		},
		"buffers": []interface{}{},
	}

	if err := conn.WriteJSON(execMsg); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send execute request: %v", err)
	}

	// Collect outputs
	var stdout, stderr string
	var outputs []*pb.JupyterOutput
	var execStatus = "ok"

	// Channel loop to read WS messages
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "websocket read error: %v", err)
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to unmarshal message: %v", err)
		}

		// Ensure the message corresponds to our request parent_header
		parentHeader, _ := msg["parent_header"].(map[string]interface{})
		parentMsgID, _ := parentHeader["msg_id"].(string)
		if parentMsgID != msgID {
			continue
		}

		msgType, _ := msg["msg_type"].(string)
		content, _ := msg["content"].(map[string]interface{})

		switch msgType {
		case "status":
			executionState, _ := content["execution_state"].(string)
			if executionState == "idle" {
				// Execution complete!
				return &pb.ExecuteJupyterCodeResponse{
					Status:  execStatus,
					Stdout:  stdout,
					Stderr:  stderr,
					Outputs: outputs,
				}, nil
			}

		case "stream":
			streamName, _ := content["name"].(string)
			text, _ := content["text"].(string)
			if streamName == "stdout" {
				stdout += text
			} else if streamName == "stderr" {
				stderr += text
			}

		case "execute_result", "display_data":
			data, _ := content["data"].(map[string]interface{})
			
			// Process plaintext results
			if textPlain, ok := data["text/plain"].(string); ok {
				outputs = append(outputs, &pb.JupyterOutput{
					Type: "text/plain",
					Data: []byte(textPlain),
				})
			}
			
			// Process PNG outputs (plots/images)
			if imagePNG, ok := data["image/png"].(string); ok {
				outputs = append(outputs, &pb.JupyterOutput{
					Type: "image/png",
					Data: []byte(imagePNG), // Base64 bytes typically returned by Jupyter
				})
			}

		case "error":
			execStatus = "error"
			ename, _ := content["ename"].(string)
			evalue, _ := content["evalue"].(string)
			tracebackSlice, _ := content["traceback"].([]interface{})
			
			var traceback string
			for _, tb := range tracebackSlice {
				if t, ok := tb.(string); ok {
					traceback += t + "\n"
				}
			}
			stderr += fmt.Sprintf("%s: %s\n%s", ename, evalue, traceback)
		}
	}
}

// Stop terminates the jupyter server when agent exits
func (s *JupyterServer) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.running && s.cmd != nil {
		_ = s.cmd.Process.Kill()
		s.running = false
	}
}
