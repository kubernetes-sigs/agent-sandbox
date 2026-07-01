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

// We keep sandboxes around for 5 minutes after we last used them.
const SandboxInactivityTimeout = 5 * time.Minute

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
}

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
		var exitErr exec.ExitError
		if errors.As(err, &exitErr) {
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

// CreateSandbox creates a Sandbox resource.
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

	// ModelName is the name of the model to use with the LLM.
	ModelName string
}

func (o *RunOptions) InitDefaults() {
	o.SessionName = os.Getenv("SESSION_NAME")
	if o.SessionName == "" {
		o.SessionName = "default"
	}

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

	if opts.HomeDir == "" {
		return fmt.Errorf("homeDir must not be empty")
	}

	if opts.SessionName == "" {
		return fmt.Errorf("sessionName is required")
	}

	if err := sessions.ValidateSessionName(opts.SessionName); err != nil {
		return fmt.Errorf("invalid sessionName %q: %w", opts.SessionName, err)
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

	toolsRegistry := tools.NewRegistry()
	toolsRegistry.Add(&tools.RunCommand{})

	toolsRegistry.Add(&tools.ListFilesTool{})
	toolsRegistry.Add(&tools.ReadFileTool{})
	toolsRegistry.Add(&tools.WriteFileTool{})

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}
	sessionsDir := filepath.Join(homeDir, ".local", "sandboxed-tools")
	sessionStore := sessions.NewFileStore(sessionsDir)

	harness := Harness{
		llmClient:     llmClient,
		sandboxClient: sandboxClient,
		toolsRegistry: toolsRegistry,
		opts:          opts,
	}

	session, err := harness.BuildSession(ctx, sessionStore)
	if err != nil {
		return fmt.Errorf("building session: %w", err)
	}

	return harness.RunSession(ctx, session)
}

type Harness struct {
	// llmClient is the client we use to talk to the llm.
	llmClient *llm.Client

	// toolsRegistry holds all the tools we have available to the llm.
	toolsRegistry *tools.Registry

	// sandboxClient is the client we use to interact with sandboxes.
	sandboxClient *SandboxClient

	// opts contains the options for the run command.
	opts RunOptions
}

func (h *Harness) BuildSession(ctx context.Context, sessionStore sessions.Store) (*Session, error) {
	session := &Session{
		Name:         h.opts.SessionName,
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

func (h *Harness) RunSession(ctx context.Context, session *Session) error {
	log := klog.FromContext(ctx)

	var activeSandbox *Sandbox

	if len(session.messages) == 0 {
		systemPrompt := "You are a helpful AI assistant with access to a sandboxed environment. " +
			"You can use the available tools (like run_command to execute shell commands, ls to list files, read to read files, and write to write files) to answer user questions or perform tasks. " +
			"Always explain what you are doing."

		sysMsg := llm.Message{
			Role:    "system",
			Content: &systemPrompt,
		}
		if err := session.AddMessages(ctx, sysMsg); err != nil {
			return fmt.Errorf("adding system prompt: %w", err)
		}

		fmt.Println("================================================================================")
		fmt.Println("Welcome to the Sandboxed Tools example!")
		fmt.Printf("Session Name: %s\n", h.opts.SessionName)
		fmt.Println("Type your message (or '/exit' or '/quit' to quit):")
		fmt.Println("================================================================================")
	} else {
		fmt.Println("================================================================================")
		fmt.Printf("Resumed session %q with %d messages in history:\n", h.opts.SessionName, len(session.messages))
		fmt.Println("================================================================================")
		for _, msg := range session.messages {
			if msg.Role == "user" {
				fmt.Printf("User> %s\n", valueOf(msg.Content))
			} else if msg.Role == "assistant" && msg.Content != nil && *msg.Content != "" {
				fmt.Printf("Agent> %s\n", *msg.Content)
			}
		}
	}

	sandboxID := session.sandboxID

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

		userMsg := llm.Message{Role: "user", Content: &input}
		if err := session.AddMessages(ctx, userMsg); err != nil {
			return fmt.Errorf("adding user message: %w", err)
		}

		shouldSnapshot := false
		for {
			llmTools := h.toolsRegistry.All()

			req := llm.ChatCompletionRequest{
				Model:    h.opts.ModelName,
				Messages: session.messages,
				Tools:    llmTools,
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

			// We keep iterating with LLM as long as there are tool calls to respond to
			if len(assistantMessage.ToolCalls) == 0 {
				fmt.Printf("\nAgent> %s\n", valueOf(assistantMessage.Content))

				// We take a snapshot at these "boundaries", rather than after every tool call
				if shouldSnapshot && activeSandbox != nil {
					log.Info("snapshotting filesystem from sandbox...", "sandbox.name", activeSandbox.SandboxName())
					if err := activeSandbox.SnapshotFS(ctx); err != nil {
						log.Error(err, "failed to snapshot filesystem")
					} else {
						shouldSnapshot = false
					}
				}

				break
			}

			if activeSandbox != nil {
				if err := activeSandbox.ExtendLifecycle(ctx, SandboxInactivityTimeout); err != nil {
					if k8serrors.IsNotFound(err) {
						log.Info("Active sandbox was deleted or expired, will recreate it", "sandbox.name", sandboxID.Name)
						activeSandbox = nil
					} else {
						return fmt.Errorf("extending sandbox TTL: %w", err)
					}
				}
			}

			if activeSandbox == nil {
				log.Info("launching sandbox for tool execution...")

				sb, err := h.CreateSandbox(ctx, session)
				if err != nil {
					return fmt.Errorf("failed to create sandbox: %w", err)
				}

				if err := sb.WaitForReady(ctx); err != nil {
					log.Error(err, "sandbox not ready")
					_ = h.sandboxClient.DeleteSandbox(context.WithoutCancel(ctx), sb)
					return err
				}

				log.V(1).Info("sandbox ready", "sandbox.name", sb.SandboxName())

				log.Info("restoring filesystem to sandbox...", "sandbox.name", sb.SandboxName())
				if err := sb.RestoreFS(ctx); err != nil {
					log.Error(err, "failed to restore filesystem; starting with a fresh sandbox instead", "sandbox.name", sb.SandboxName())
				}

				activeSandbox = sb
			}

			for _, tc := range assistantMessage.ToolCalls {
				// TODO: Add a timeout to the tool execution?
				result, err := h.toolsRegistry.Call(ctx, activeSandbox, tc)

				var toolMsg llm.Message
				if err != nil {
					// We send errors to the LLM, so we want to snapshot the filesystem
					// so that the filesystem state is consistent with the session state.
					shouldSnapshot = true

					log.Error(err, "error calling tool", "tool", tc.Function.Name)
					content := fmt.Sprintf("Error calling tool %q: %v", tc.Function.Name, err)
					toolMsg = llm.Message{
						Role:       "tool",
						ToolCallID: tc.ID,
						Content:    &content,
					}
				} else {
					// The tool succeeded, so snapshot.
					shouldSnapshot = true

					toolMsg = result
				}

				if err := session.AddMessages(ctx, toolMsg); err != nil {
					return fmt.Errorf("adding tool response: %w", err)
				}

				// The tool call could take a while, so extend the lifecycle.
				// This might need more careful handling later; what if the command takes 20 minutes to run?
				if err := activeSandbox.ExtendLifecycle(ctx, SandboxInactivityTimeout); err != nil {
					log.Error(err, "extending sandbox lifecycle")
				}
			}

			// Here we continue the LLM loop, with the tool responses at the tail of the chat thread.
		}
	}

	return nil
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
