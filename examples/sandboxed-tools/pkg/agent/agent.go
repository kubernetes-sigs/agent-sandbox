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

// Package agent implements the agentic loop of the sandboxed-tools example:
// it sends the conversation to an LLM, and executes the tool calls the LLM
// requests inside an Agent Sandbox, creating (and re-creating) the sandbox
// on demand. It is shared by the interactive CLI (cmd/sandboxed-tools-cli)
// and other front ends.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	agentsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/sessions"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

// SandboxInactivityTimeout is how long we keep sandboxes around after we last used them.
const SandboxInactivityTimeout = 5 * time.Minute

// DefaultToolTimeout bounds how long a single tool invocation may run before
// it is cancelled, so a hung command (e.g. "sleep 99999") fails fast instead
// of blocking the agent loop indefinitely.
const DefaultToolTimeout = 2 * time.Minute

// systemPrompt seeds every new session.
const systemPrompt = "You are a helpful AI assistant with access to a sandboxed environment. " +
	"You can use the available tools (like run_command to execute shell commands, ls to list files, read to read files, and write to write files) to answer user questions or perform tasks. " +
	"Always explain what you are doing."

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

// GetRESTConfig loads the kubernetes client configuration, preferring
// in-cluster configuration and falling back to the local kubeconfig.
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

// NewSandboxClient initializes a new SandboxClient using the provided rest.Config.
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

	// HomeDir is the home directory; we mount a EmptyDir volume here.
	// We currently only snapshot and restore this directory.
	HomeDir string

	// sessionStore is the store for session state.
	sessionStore sessions.Store

	// messages is a list of all the messages in the current session chat.
	messages []llm.Message

	// sandboxID is the ID of the sandbox we use.
	// Note that the sandbox does not always exist (for example, when idle)
	sandboxID types.NamespacedName

	// activeSandbox is the sandbox currently provisioned for this session,
	// or nil when no sandbox is running (e.g. before the first tool call).
	activeSandbox *Sandbox
}

// Messages returns a copy of the messages in the session so far.
func (s *Session) Messages() []llm.Message {
	return slices.Clone(s.messages)
}

// AddMessages appends messages to the session, persisting them to the session store.
func (s *Session) AddMessages(ctx context.Context, messages ...llm.Message) error {
	if err := s.sessionStore.AppendMessages(ctx, s.Name, messages...); err != nil {
		return fmt.Errorf("failed to persist messages: %w", err)
	}
	s.messages = append(s.messages, messages...)

	return nil
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

// ExtendLifecycle updates the ShutdownTime of the Sandbox in Kubernetes to now + inactivityTimeout.
func (s *Sandbox) ExtendLifecycle(ctx context.Context, inactivityTimeout time.Duration) error {
	agentsClient := s.session.client.agentsClient

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest spec
		latest, err := agentsClient.AgentsV1beta1().Sandboxes(s.id.Namespace).Get(ctx, s.id.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Update ShutdownTime
		latest.Spec.ShutdownTime = &metav1.Time{Time: time.Now().Add(inactivityTimeout)}

		_, err = agentsClient.AgentsV1beta1().Sandboxes(s.id.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		return nil
	})
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
				s.podName = latest.Name
				break readyLoop
			}
		}
	}
	return nil
}

// ExecCommand executes a command inside the sandbox container with specified options.
// If Stdout or Stderr are nil in tools.ExecCommandOptions, they are captured internally and returned in the tools.ExecCommandResult.
// If the command returns a non-zero exit code, that is _not_ treated as an error; the exit code is returned in the result.
func (s *Sandbox) ExecCommand(ctx context.Context, opts tools.ExecCommandOptions) (*tools.ExecCommandResult, error) {
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
		if exitErr, ok := errors.AsType[exec.ExitError](err); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			return nil, fmt.Errorf("kubernetes exec failed: %w", err)
		}
	}

	res := &tools.ExecCommandResult{
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

	opts := tools.ExecCommandOptions{
		Command: []string{"tar", "-zxf", "-", "-C", s.session.HomeDir},
		Stdin:   f,
	}
	res, err := s.ExecCommand(ctx, opts)
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

	backupFile, err := os.OpenFile(tmpFilename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create backup file %s: %w", tmpFilename, err)
	}
	defer backupFile.Close()

	// Clean up the temp file if something goes wrong
	shouldDeleteTempFile := true
	defer func() {
		if shouldDeleteTempFile {
			if err := os.Remove(tmpFilename); err != nil {
				log.Error(err, "unable to remove temporary backup file")
			}
		}
	}()

	opts := tools.ExecCommandOptions{
		Command: []string{"tar", "-zcf", "-", "-C", s.session.HomeDir, "."},
		Stdout:  backupFile,
	}
	res, err := s.ExecCommand(ctx, opts)
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

	// Don't delete the temp file; we successfully created the backup
	shouldDeleteTempFile = false

	// Prune backups, keeping only the last 5
	if err := s.session.PruneBackups(ctx, 5); err != nil {
		log.Error(err, "failed to prune old backups")
	}

	log.Info("saved filesystem state to new backup", "backup", backupFilename)
	return nil
}

// CreateSandbox creates a Sandbox resource for the session.
func (h *Harness) CreateSandbox(ctx context.Context, session *Session) (*Sandbox, error) {
	agentsClient := h.sandboxClient.agentsClient

	id := session.sandboxID
	image := h.opts.Image
	homeDir := h.opts.HomeDir

	policy := sandboxv1beta1.ShutdownPolicyDelete
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id.Name,
			Namespace: id.Namespace,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
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
										MountPath: homeDir,
									},
								},
								Env: []corev1.EnvVar{
									{
										Name:  "HOME",
										Value: homeDir,
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
			Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   &metav1.Time{Time: time.Now().Add(SandboxInactivityTimeout)},
				ShutdownPolicy: &policy,
			},
		},
	}

	_, err := agentsClient.AgentsV1beta1().Sandboxes(id.Namespace).Create(ctx, sb, metav1.CreateOptions{})
	if err != nil {
		// Note: we need to handle the case when the sandbox already exists,
		// we want to confirm the sandbox configuration matches before proceeding.
		return nil, fmt.Errorf("failed to create Sandbox: %w", err)
	}

	sandbox := &Sandbox{
		session: session,
		id:      id,
		created: true,
	}

	h.sandboxClient.mutex.Lock()
	h.sandboxClient.sandboxes[sandbox.NamespacedName()] = sandbox
	h.sandboxClient.mutex.Unlock()

	return sandbox, nil
}

// DeleteSandbox deletes the Sandbox resource.
func (c *SandboxClient) DeleteSandbox(ctx context.Context, sb *Sandbox) error {
	if !sb.created {
		return nil
	}
	id := sb.NamespacedName()
	// NotFound means the sandbox already expired (ShutdownPolicy Delete),
	// which is success for our purposes.
	if err := c.agentsClient.AgentsV1beta1().Sandboxes(id.Namespace).Delete(ctx, id.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
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

// RunOptions are the options for running the agent.
type RunOptions struct {
	Namespace string
	Image     string

	// HomeDir is the home directory inside the sandbox.
	// This is currently the only path that we persist between execs in the sandbox.
	HomeDir string

	// ModelName is the name of the model to use with the LLM.
	ModelName string

	// ToolTimeout bounds how long a single tool invocation may run before it
	// is cancelled. <= 0 disables the timeout. Default: DefaultToolTimeout.
	ToolTimeout time.Duration
}

// InitDefaults populates the options from environment variables and defaults.
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

	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = os.Getenv("MODEL")
	}
	if modelName == "" {
		modelName = "gemini-3.5-flash"
	}
	o.ModelName = modelName

	o.ToolTimeout = DefaultToolTimeout
}

// TurnEvents receives progress callbacks while RunTurn processes a user
// message. Implementations render them to a local console, or stream them
// to a remote client (e.g. as ACP session/update notifications).
type TurnEvents interface {
	// AssistantMessage is called with the assistant's final text reply for
	// the turn.
	AssistantMessage(ctx context.Context, text string)

	// ApproveToolCall reports whether a requested tool call may execute.
	// Returning false sends a denial to the LLM instead of running the
	// tool; returning an error aborts the turn (for example, if the
	// approver has disconnected).
	ApproveToolCall(ctx context.Context, call llm.ToolCall) (bool, error)

	// ToolCallStarted is called immediately before an approved tool executes.
	ToolCallStarted(ctx context.Context, call llm.ToolCall)

	// ToolCallFinished is called after a tool executes; err is non-nil if
	// the tool failed.
	ToolCallFinished(ctx context.Context, call llm.ToolCall, err error)
}

// Harness wires the LLM, the tool registry, and the sandbox client together
// into an agentic loop.
type Harness struct {
	// llmClient is the client we use to talk to the llm.
	llmClient llm.ChatClient

	// toolsRegistry holds all the tools we have available to the llm.
	toolsRegistry *tools.Registry

	// sandboxClient is the client we use to interact with sandboxes.
	sandboxClient *SandboxClient

	// opts contains the options for the run command.
	opts RunOptions
}

// NewHarness constructs a Harness.
func NewHarness(llmClient llm.ChatClient, sandboxClient *SandboxClient, toolsRegistry *tools.Registry, opts RunOptions) *Harness {
	return &Harness{
		llmClient:     llmClient,
		sandboxClient: sandboxClient,
		toolsRegistry: toolsRegistry,
		opts:          opts,
	}
}

// BuildSession creates a Session named sessionName, loading any existing
// history from the session store.
func (h *Harness) BuildSession(ctx context.Context, sessionStore sessions.Store, sessionName string) (*Session, error) {
	// Session names become Kubernetes object names and filesystem path
	// components, so validate here even though front ends generally do too.
	if err := sessions.ValidateSessionName(sessionName); err != nil {
		return nil, fmt.Errorf("invalid session name %q: %w", sessionName, err)
	}

	session := &Session{
		Name:         sessionName,
		client:       h.sandboxClient,
		HomeDir:      h.opts.HomeDir,
		sessionStore: sessionStore,
	}

	session.sandboxID = types.NamespacedName{
		Name:      session.Name,
		Namespace: h.opts.Namespace,
	}

	messages, err := sessionStore.LoadSession(ctx, session.Name)
	if err != nil {
		return nil, fmt.Errorf("loading session: %w", err)
	}
	session.messages = messages

	return session, nil
}

// EnsureSystemPrompt seeds a new session with the system prompt. It is a
// no-op for resumed sessions that already have history.
func (h *Harness) EnsureSystemPrompt(ctx context.Context, session *Session) error {
	if len(session.messages) > 0 {
		return nil
	}
	prompt := systemPrompt
	return session.AddMessages(ctx, llm.Message{Role: "system", Content: &prompt})
}

// RunTurn processes one user message: it calls the LLM, executes (approved)
// tool calls in the session's sandbox, and repeats until the LLM produces a
// final text reply, which is delivered via events.AssistantMessage.
func (h *Harness) RunTurn(ctx context.Context, session *Session, input string, events TurnEvents) error {
	log := klog.FromContext(ctx)

	userMsg := llm.Message{Role: "user", Content: &input}
	if err := session.AddMessages(ctx, userMsg); err != nil {
		return fmt.Errorf("adding user message: %w", err)
	}

	shouldSnapshot := false
	for {
		req := llm.ChatCompletionRequest{
			Model:    h.opts.ModelName,
			Messages: session.messages,
			Tools:    h.toolsRegistry.All(),
		}

		assistantResponse, err := h.llmClient.CreateChatCompletion(ctx, req)
		if err != nil {
			return fmt.Errorf("failed to call LLM: %w", err)
		}

		if len(assistantResponse.Choices) == 0 {
			return fmt.Errorf("LLM returned no choices: %v", assistantResponse)
		}

		assistantMessage := assistantResponse.Choices[0].Message
		if err := session.AddMessages(ctx, assistantMessage); err != nil {
			return fmt.Errorf("adding assistant message: %w", err)
		}

		log.V(1).Info("got message from LLM", "msg", assistantMessage)

		// We keep iterating with the LLM as long as there are tool calls to respond to
		if len(assistantMessage.ToolCalls) == 0 {
			events.AssistantMessage(ctx, valueOf(assistantMessage.Content))

			// We take a snapshot at these "boundaries", rather than after every tool call
			if shouldSnapshot && session.activeSandbox != nil {
				log.Info("snapshotting filesystem from sandbox...", "sandbox.name", session.activeSandbox.SandboxName())
				if err := session.activeSandbox.SnapshotFS(ctx); err != nil {
					log.Error(err, "failed to snapshot filesystem")
				}
			}

			return nil
		}

		for _, tc := range assistantMessage.ToolCalls {
			approved, err := events.ApproveToolCall(ctx, tc)
			if err != nil {
				return fmt.Errorf("requesting tool call approval: %w", err)
			}

			var toolMsg llm.Message
			if !approved {
				log.Info("tool call denied by user", "tool", tc.Function.Name)
				content := fmt.Sprintf("The user denied permission to run tool %q.", tc.Function.Name)
				toolMsg = llm.Message{Role: "tool", ToolCallID: tc.ID, Content: &content}
			} else {
				sandbox, err := h.ensureSandbox(ctx, session)
				if err != nil {
					return err
				}

				events.ToolCallStarted(ctx, tc)
				result, err := h.toolsRegistry.Call(ctx, sandbox, tc)
				events.ToolCallFinished(ctx, tc, err)

				// The filesystem may have changed (even on error), so
				// snapshot at the next boundary to keep the persisted
				// filesystem state consistent with the session state.
				shouldSnapshot = true

				if err != nil {
					log.Error(err, "error calling tool", "tool", tc.Function.Name)
					content := fmt.Sprintf("Error calling tool %q: %v", tc.Function.Name, err)
					toolMsg = llm.Message{Role: "tool", ToolCallID: tc.ID, Content: &content}
				} else {
					toolMsg = result
				}

				// The tool call could take a while, so extend the lifecycle.
				// This might need more careful handling later; what if the command takes 20 minutes to run?
				if err := sandbox.ExtendLifecycle(ctx, SandboxInactivityTimeout); err != nil {
					log.Error(err, "extending sandbox lifecycle")
				}
			}

			if err := session.AddMessages(ctx, toolMsg); err != nil {
				return fmt.Errorf("adding tool response: %w", err)
			}
		}

		// Here we continue the LLM loop, with the tool responses at the tail of the chat thread.
	}
}

// ensureSandbox returns the session's running sandbox, creating it (and
// restoring its filesystem from the latest snapshot) if it does not exist
// or was cleaned up due to inactivity.
func (h *Harness) ensureSandbox(ctx context.Context, session *Session) (*Sandbox, error) {
	log := klog.FromContext(ctx)

	if sb := session.activeSandbox; sb != nil {
		if err := sb.ExtendLifecycle(ctx, SandboxInactivityTimeout); err != nil {
			if k8serrors.IsNotFound(err) {
				log.Info("Active sandbox was deleted or expired, will recreate it", "sandbox.name", session.sandboxID.Name)
				session.activeSandbox = nil
			} else {
				return nil, fmt.Errorf("extending sandbox TTL: %w", err)
			}
		}
	}

	if session.activeSandbox == nil {
		log.Info("launching sandbox for tool execution...")

		sb, err := h.CreateSandbox(ctx, session)
		if err != nil {
			return nil, fmt.Errorf("failed to create sandbox: %w", err)
		}

		if err := sb.WaitForReady(ctx); err != nil {
			log.Error(err, "sandbox not ready")
			_ = h.sandboxClient.DeleteSandbox(context.WithoutCancel(ctx), sb)
			return nil, err
		}

		log.V(1).Info("sandbox ready", "sandbox.name", sb.SandboxName())

		log.Info("restoring filesystem to sandbox...", "sandbox.name", sb.SandboxName())
		if err := sb.RestoreFS(ctx); err != nil {
			log.Error(err, "failed to restore filesystem; starting with a fresh sandbox instead", "sandbox.name", sb.SandboxName())
		}

		session.activeSandbox = sb
	}

	return session.activeSandbox, nil
}

// valueOf is a helper that safely gets a value from a pointer,
// if the pointer is nil it returns the default (zero) value.
func valueOf[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
