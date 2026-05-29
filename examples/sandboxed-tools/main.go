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

// sandboxed-tools demonstrates launching an agent sandbox only for tool execution
// and stopping it immediately after the tool execution completes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
	"k8s.io/klog/v2"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	agentsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

func main() {
	ctx := context.Background()

	// Set up signal handling
	signalCtx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	{
		klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
		klog.InitFlags(klogFlags)

		// Add some (but not all) klog flags
		klogFlags.VisitAll(func(f *flag.Flag) {
			switch f.Name {
			case "v":
				flag.Var(f.Value, f.Name, f.Usage)
			}
		})
	}

	var opts RunOptions
	opts.InitDefaults()
	flag.StringVar(&opts.SessionName, "session", opts.SessionName, "session name")
	flag.StringVar(&opts.Namespace, "namespace", opts.Namespace, "namespace")
	flag.StringVar(&opts.Image, "image", opts.Image, "image")
	flag.StringVar(&opts.HomeDir, "homedir", opts.HomeDir, "Home directory in the sandbox; this is currently the only directory that we persist with snapshot/restore.")
	flag.Parse()

	log := klog.FromContext(ctx)

	if err := run(signalCtx, opts); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "\n")
			log.V(1).Info("context cancelled")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "sandboxed-tools: %v\n", err)
		os.Exit(1)
	}
}

// SandboxClient is a simple low-level client for managing Sandbox resources directly.
type SandboxClient struct {
	agentsClient agentsclientset.Interface
	coreClient   corev1client.CoreV1Interface
	restConfig   *rest.Config

	// mutex guards the mutable values below
	mutex sync.Mutex

	// sandboxes is a map of sandboxes we have created and not yet deleted.
	sandboxes map[types.NamespacedName]*Sandbox
}

func GetRESTConfig() (*rest.Config, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		restConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
	}
	return restConfig, nil
}

// NewSandboxClient initializes a new SandboxClient using the provided rest.Config or loading from environment.
func NewSandboxClient(restConfig *rest.Config) (*SandboxClient, error) {
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes HTTP client: %w", err)
	}

	agentsCS, err := agentsclientset.NewForConfigAndClient(restConfig, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create client for kubernetes agent-sandbox types: %w", err)
	}

	coreClient, err := corev1client.NewForConfigAndClient(restConfig, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create core v1 client: %w", err)
	}

	return &SandboxClient{
		agentsClient: agentsCS,
		coreClient:   coreClient,
		restConfig:   restConfig,
		sandboxes:    make(map[types.NamespacedName]*Sandbox),
	}, nil
}

// Session represents an agentic "chat" session (a stream of messages / tools calls etc).
type Session struct {
	// Name is the identifier for the session
	Name string

	// client is the sandbox client to use to interact with the cluster
	client *SandboxClient

	// HomeDir is the home directory; we mount a tmpfs volume here.
	// We currently only snapshot and restore this directory.
	HomeDir string
}

// Sandbox represents an active sandbox instance.
type Sandbox struct {
	session *Session

	id types.NamespacedName

	podName string

	// created is true if we have created the sandbox (and not deleted it)
	created bool
}

// NamespacedName returns the namespace and name of the Sandbox resource.
func (s *Sandbox) NamespacedName() types.NamespacedName {
	return s.id
}

// SandboxName returns the name of the sandbox.
func (s *Sandbox) SandboxName() string {
	return s.id.Name
}

// PodNamespacedName returns the namespace and name of the underlying Pod.
func (s *Sandbox) PodNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: s.id.Namespace,
		Name:      s.podName,
	}
}

// WaitForReady polls the Sandbox resource until it becomes ready and resolves the underlying Pod name.
func (s *Sandbox) WaitForReady(ctx context.Context) error {
	timeout := time.After(3 * time.Minute)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	agentsClient := s.session.client.agentsClient

readyLoop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timed out waiting for Sandbox %s to become ready", s.SandboxName())
		case <-ticker.C:
			latest, err := agentsClient.AgentsV1beta1().Sandboxes(s.NamespacedName().Namespace).Get(ctx, s.NamespacedName().Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("error polling state of sandbox: %w", err)
			}
			ready := false
			for _, cond := range latest.Status.Conditions {
				if cond.Type == string(sandboxv1beta1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
					ready = true
					break
				}
			}
			if ready {
				if name, ok := latest.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]; ok && name != "" {
					s.podName = name
				} else {
					s.podName = latest.Name
				}
				break readyLoop
			}
		}
	}
	return nil
}

// ExecutionResult holds the captured stdout, stderr, and exit code of a command.
type ExecutionResult struct {
	// Stdout contains the captured standard output.
	// This is only populated if ExecOptions.Stdout was nil.
	Stdout string

	// Stderr contains the captured standard error.
	// This is only populated if ExecOptions.Stderr was nil.
	Stderr string

	// ExitCode is the exit status returned by the command.
	ExitCode int
}

// ExecOptions contains the options for executing a command inside the sandbox container.
type ExecOptions struct {
	// Command is the process name and arguments to run (e.g. []string{"sh", "-c", "uname -a"}).
	Command []string

	// Stdin is an optional reader to pipe into the command's standard input.
	Stdin io.Reader

	// Stdout is an optional writer where standard output will be written.
	// If nil, Stdout will be captured internally and returned as a string in the ExecutionResult.
	Stdout io.Writer

	// Stderr is an optional writer where standard error will be written.
	// If nil, Stderr will be captured internally and returned as a string in the ExecutionResult.
	Stderr io.Writer
}

// Exec executes a command inside the sandbox container with specified options.
// If Stdout or Stderr are nil in ExecOptions, they are captured internally and returned in the ExecutionResult.
func (s *Sandbox) Exec(ctx context.Context, opts ExecOptions) (*ExecutionResult, error) {
	coreClient := s.session.client.coreClient
	restConfig := s.session.client.restConfig

	podID := s.PodNamespacedName()

	if podID.Name == "" {
		return nil, fmt.Errorf("pod name not resolved yet")
	}

	stdout := opts.Stdout
	var stdoutBuf bytes.Buffer
	if stdout == nil {
		stdout = &stdoutBuf
	}

	stderr := opts.Stderr
	var stderrBuf bytes.Buffer
	if stderr == nil {
		stderr = &stderrBuf
	}

	req := coreClient.RESTClient().Post().
		Resource("pods").
		Name(podID.Name).
		Namespace(podID.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   opts.Command,
			Stdin:     opts.Stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})

	exitCode := 0
	if err != nil {
		var exitErr exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			return nil, fmt.Errorf("kubernetes exec failed: %w", err)
		}
	}

	res := &ExecutionResult{
		ExitCode: exitCode,
	}
	if opts.Stdout == nil {
		res.Stdout = stdoutBuf.String()
	}
	if opts.Stderr == nil {
		res.Stderr = stderrBuf.String()
	}

	return res, nil
}

// Run executes a shell command in the sandbox pod.
func (s *Sandbox) Run(ctx context.Context, command string) (*ExecutionResult, error) {
	opts := ExecOptions{
		Command: []string{"sh", "-c", command},
	}
	return s.Exec(ctx, opts)
}

// getBackupDir gets the backup directory for the session.
// It creates the directory if it doesn't exist.
func (s *Session) getBackupDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	dir := filepath.Join(home, ".local", "sandboxed-tools", s.Name, "fs")

	// Ensure the session's backup directory exists;
	// use mode 700 because it might contain sensitive data.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}
	return dir, nil
}

// FindLatestBackup searches for the latest backup tarball in the session's backup directory.
func (s *Session) FindLatestBackup() (string, error) {
	dir, err := s.getBackupDir()
	if err != nil {
		return "", err
	}

	matches, err := filepath.Glob(filepath.Join(dir, "backup-*.tar.gz"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	// Since Glob returns matches sorted alphabetically, and YYYYMMDDTHHMMSS
	// naturally sorts alphabetically in chronological order, the last match is the latest one!
	return matches[len(matches)-1], nil
}

// PruneBackups deletes older backups, keeping only the most recent keepCount backups.
func (s *Session) PruneBackups(ctx context.Context, keepCount int) error {
	log := klog.FromContext(ctx)
	dir, err := s.getBackupDir()
	if err != nil {
		return err
	}

	matches, err := filepath.Glob(filepath.Join(dir, "backup-*.tar.gz"))
	if err != nil {
		return err
	}

	if len(matches) <= keepCount {
		return nil
	}

	pruneCount := len(matches) - keepCount
	for i := range pruneCount {
		if err := os.Remove(matches[i]); err != nil {
			log.Error(err, "unable to prune old backup", "backup", matches[i])
		} else {
			log.Info("pruned old backup", "backup", matches[i])
		}
	}

	return nil
}

// RestoreFS restores the filesystem in the sandbox from the latest local backup tarball, if one exists.
func (s *Sandbox) RestoreFS(ctx context.Context) error {
	log := klog.FromContext(ctx)

	latestBackup, err := s.session.FindLatestBackup()
	if err != nil {
		return fmt.Errorf("failed to search for latest backup: %w", err)
	}
	if latestBackup == "" {
		// No previous backup found, start fresh
		return nil
	}

	log.Info("restoring filesystem from latest backup", "backup", latestBackup)
	f, err := os.Open(latestBackup)
	if err != nil {
		return fmt.Errorf("failed to open backup file %s: %w", latestBackup, err)
	}
	defer f.Close()

	opts := ExecOptions{
		Command: []string{"tar", "-zxf", "-", "-C", s.session.HomeDir},
		Stdin:   f,
	}
	res, err := s.Exec(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to execute restore: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restore failed with exit code %d: %s", res.ExitCode, res.Stderr)
	}

	return nil
}

// SnapshotFS archives the filesystem in the sandbox and saves it to a new timestamped backup inside the session's backup directory.
func (s *Sandbox) SnapshotFS(ctx context.Context) error {
	log := klog.FromContext(ctx)

	dir, err := s.session.getBackupDir()
	if err != nil {
		return err
	}

	timestamp := time.Now().Format("20060102T150405")
	backupFilename := filepath.Join(dir, fmt.Sprintf("backup-%s.tar.gz", timestamp))
	tmpFilename := backupFilename + ".tmp"

	backupFile, err := os.OpenFile(tmpFilename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create backup file %s: %w", tmpFilename, err)
	}
	defer backupFile.Close()

	opts := ExecOptions{
		Command: []string{"tar", "-zcf", "-", "-C", s.session.HomeDir, "."},
		Stdout:  backupFile,
	}
	res, err := s.Exec(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to execute snapshot: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("snapshot failed with exit code %d: %s", res.ExitCode, res.Stderr)
	}

	// Close the file explicitly before renaming
	if err := backupFile.Close(); err != nil {
		return fmt.Errorf("failed to close backup file %s: %w", tmpFilename, err)
	}

	if err := os.Rename(tmpFilename, backupFilename); err != nil {
		return fmt.Errorf("failed to rename temp backup file to final path: %w", err)
	}

	// Prune backups, keeping only the last 5
	if err := s.session.PruneBackups(ctx, 5); err != nil {
		log.Error(err, "failed to prune old backups")
	}

	log.Info("saved filesystem state to new backup", "backup", backupFilename)
	return nil
}

// CreateSandbox creates a Sandbox resource.
func (s *Session) CreateSandbox(ctx context.Context, image, namespace string) (*Sandbox, error) {
	agentsClient := s.client.agentsClient

	// TODO: Use shutdownPolicy (and maybe cache the sandbox between tool calls)

	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sandbox-tool-",
			Namespace:    namespace,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false),
					Containers: []corev1.Container{
						{
							Name:    "sandbox",
							Image:   image,
							Command: []string{"sleep", "infinity"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "home",
									MountPath: s.HomeDir,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "HOME",
									Value: s.HomeDir,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "home",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	created, err := agentsClient.AgentsV1beta1().Sandboxes(namespace).Create(ctx, sb, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create Sandbox: %w", err)
	}

	id := types.NamespacedName{
		Namespace: created.Namespace,
		Name:      created.Name,
	}
	sandbox := &Sandbox{
		session: s,
		id:      id,
		created: true,
	}

	s.client.mutex.Lock()
	s.client.sandboxes[sandbox.NamespacedName()] = sandbox
	s.client.mutex.Unlock()

	return sandbox, nil
}

// DeleteSandbox deletes the Sandbox resource.
func (c *SandboxClient) DeleteSandbox(ctx context.Context, sb *Sandbox) error {
	if !sb.created {
		return nil
	}
	id := sb.NamespacedName()
	if err := c.agentsClient.AgentsV1beta1().Sandboxes(id.Namespace).Delete(ctx, id.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete Sandbox: %w", err)
	}
	sb.created = false

	c.mutex.Lock()
	delete(c.sandboxes, sb.NamespacedName())
	c.mutex.Unlock()

	return nil
}

// DeleteAllSandboxes deletes all active Sandboxes tracked by this client.
func (c *SandboxClient) DeleteAllSandboxes(ctx context.Context) error {
	var errs []error

	c.mutex.Lock()
	sandboxes := make([]*Sandbox, 0, len(c.sandboxes))
	for _, ts := range c.sandboxes {
		sandboxes = append(sandboxes, ts)
	}
	c.mutex.Unlock()

	for _, sb := range sandboxes {
		if err := c.DeleteSandbox(ctx, sb); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// readLine reads a single line from os.Stdin.
func readLine() ([]byte, error) {
	var line []byte

	buf := make([]byte, 1)
	for {
		_, err := os.Stdin.Read(buf)
		if err != nil {
			klog.Infof("failed to read line: %v", err)
			return nil, fmt.Errorf("failed to read line: %w", err)
		}
		if buf[0] == '\n' {
			return line, nil
		}
		if buf[0] != '\r' {
			line = append(line, buf[0])
		}
	}
}

// RunOptions are the options for the run command.
type RunOptions struct {
	SessionName string
	Namespace   string
	Image       string

	// HomeDir is the home directory inside the sandbox.
	// This is currently the only path that we persist between execs in the sandbox.
	HomeDir string
}

func (o *RunOptions) InitDefaults() {
	o.Image = os.Getenv("SANDBOX_IMAGE")
	if o.Image == "" {
		o.Image = "debian:bookworm-slim"
	}

	o.Namespace = os.Getenv("SANDBOX_NAMESPACE")
	if o.Namespace == "" {
		o.Namespace = "default"
	}

	o.HomeDir = os.Getenv("SANDBOX_HOME_DIR")
	if o.HomeDir == "" {
		o.HomeDir = "/home/clawtainer"
	}
}

func run(ctx context.Context, opts RunOptions) error {
	log := klog.FromContext(ctx)

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY or OPENAI_API_KEY environment variable is required")
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	}

	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = os.Getenv("MODEL")
	}
	if modelName == "" {
		modelName = "gemini-3.5-flash"
	}

	if !isAlphaNumeric(opts.SessionName) {
		return fmt.Errorf("invalid session name %q: must be alphanumeric", opts.SessionName)
	}

	llmClient, err := llm.NewClient(baseURL, apiKey)
	if err != nil {
		return fmt.Errorf("failed to initialize llm client: %w", err)
	}

	restConfig, err := GetRESTConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes configuration: %w", err)
	}

	sandboxClient, err := NewSandboxClient(restConfig)
	if err != nil {
		return fmt.Errorf("failed to initialize sandbox client: %w", err)
	}
	defer func() {
		if err := sandboxClient.DeleteAllSandboxes(context.WithoutCancel(ctx)); err != nil {
			log.Error(err, "failed to delete all sandboxes")
		}
	}()

	session := &Session{
		Name:   opts.SessionName,
		client: sandboxClient,
	}

	runCmdTool := llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "run_command",
			Description: "Executes a shell command inside a sandbox and returns the stdout and stderr.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command to execute",
					},
				},
				"required": []string{"command"},
			},
		},
	}

	messages := []llm.Message{
		{
			Role: "system",
			Content: "You are a helpful AI assistant with access to a sandboxed environment. " +
				"You can use the run_command tool to execute shell commands to answer user questions or perform tasks. " +
				"Always explain what you are doing.",
		},
	}

	fmt.Println("================================================================================")
	fmt.Println("Welcome to the Sandboxed Tools example!")
	fmt.Printf("Using LLM Base URL: %s (Model: %s)\n", baseURL, modelName)
	fmt.Println("Key Concept: An Agent Sandbox is launched ONLY when a tool needs to be executed,")
	fmt.Println("             and is immediately deleted afterward.")
	fmt.Println("Type your message (or '/exit' or '/quit' to quit):")
	fmt.Println("================================================================================")

	for {
		fmt.Print("\nUser> ")
		lineBytes, err := readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("error reading standard input: %w", err)
		}

		input := strings.TrimSpace(string(lineBytes))
		if input == "" {
			continue
		}
		if strings.ToLower(input) == "/exit" || strings.ToLower(input) == "/quit" {
			break
		}

		messages = append(messages, llm.Message{Role: "user", Content: input})

		for {
			req := llm.ChatCompletionRequest{
				Model:    modelName,
				Messages: messages,
				Tools:    []llm.Tool{runCmdTool},
			}

			resp, err := llmClient.CreateChatCompletion(ctx, req)
			if err != nil {
				fmt.Printf("[LLM Error] Failed to call LLM: %v\n", err)
				break
			}

			if len(resp.Choices) == 0 {
				fmt.Println("[LLM Error] LLM returned no choices")
				break
			}

			msg := resp.Choices[0].Message
			messages = append(messages, msg)

			if len(msg.ToolCalls) == 0 {
				fmt.Printf("\nAgent> %s\n", msg.Content)
				break
			}

			for _, tc := range msg.ToolCalls {
				if tc.Function.Name == "run_command" {
					var args struct {
						Command string `json:"command"`
					}
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						fmt.Printf("[Tool Error] Failed to parse arguments: %v\n", err)
						messages = append(messages, llm.Message{
							Role:       "tool",
							ToolCallID: tc.ID,
							Content:    fmt.Sprintf("Failed to parse arguments: %v", err),
						})
						continue
					}

					fmt.Printf("\n[Tool Execution] LLM requested tool %q with command: %q\n", tc.Function.Name, args.Command)
					log.Info("launching sandbox for tool execution...")

					sb, err := session.CreateSandbox(ctx, opts.Image, opts.Namespace)
					if err != nil {
						fmt.Printf("[Sandbox Error] Failed to create sandbox: %v\n", err)
						messages = append(messages, llm.Message{
							Role:       "tool",
							ToolCallID: tc.ID,
							Content:    fmt.Sprintf("Sandbox creation failed: %v", err),
						})
						continue
					}

					if err := sb.WaitForReady(ctx); err != nil {
						log.Error(err, "sandbox not ready")
						_ = sandboxClient.DeleteSandbox(context.WithoutCancel(ctx), sb)
						return err
					}

					log.V(1).Info("sandbox ready", "sandbox.name", sb.SandboxName())

					log.Info("restoring filesystem to sandbox...", "sandbox.name", sb.SandboxName())
					if err := sb.RestoreFS(ctx); err != nil {
						log.Error(err, "failed to restore filesystem", "sandbox.name", sb.SandboxName())
						_ = sandboxClient.DeleteSandbox(context.WithoutCancel(ctx), sb)
						messages = append(messages, llm.Message{
							Role:       "tool",
							ToolCallID: tc.ID,
							Content:    fmt.Sprintf("Filesystem restore failed: %v", err),
						})
						continue
					}

					log.Info("executing command in sandbox", "sandbox.name", sb.SandboxName(), "command", args.Command)
					// TODO: Add a timeout to the tool execution?
					res, err := sb.Run(ctx, args.Command)

					// We don't snapshot if there was an error, but we do snapshot if the tool just
					// returned a non-zero exit code.
					if err == nil {
						log.Info("snapshotting filesystem from sandbox...", "sandbox.name", sb.SandboxName())
						if err := sb.SnapshotFS(ctx); err != nil {
							// Maybe this should be surfaced as an error ... but let's deal with this
							// when we cache the sandbox.
							log.Error(err, "failed to snapshot filesystem")
						}
					}

					log.Info("deleting sandbox", "sandbox.name", sb.SandboxName())
					if deleteErr := sandboxClient.DeleteSandbox(context.WithoutCancel(ctx), sb); deleteErr != nil {
						log.Error(deleteErr, "failed to delete sandbox", "sandbox.name", sb.SandboxName())
					} else {
						log.V(1).Info("sandbox deleted successfully.", "sandbox.name", sb.SandboxName())
					}

					var toolResult string
					if err != nil {
						toolResult = fmt.Sprintf("Execution error: %v", err)
					} else {
						toolResult = fmt.Sprintf("stdout:\n%s\nstderr:\n%s\nexit_code: %d", res.Stdout, res.Stderr, res.ExitCode)
					}
					fmt.Printf("[Tool Result] %s\n", toolResult)

					messages = append(messages, llm.Message{
						Role:       "tool",
						ToolCallID: tc.ID,
						Content:    toolResult,
					})
				} else {
					fmt.Printf("[Tool Error] Unknown tool requested: %s\n", tc.Function.Name)
					messages = append(messages, llm.Message{
						Role:       "tool",
						ToolCallID: tc.ID,
						Content:    fmt.Sprintf("Unknown tool %q", tc.Function.Name),
					})
				}
			}
		}
	}

	return nil
}

// isAlphaNumeric returns true if the string is purely alphanumeric.
func isAlphaNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
