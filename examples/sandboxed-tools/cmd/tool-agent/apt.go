package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type AptGetInstall struct {
	Packages []string
	Yes      bool
}

func (c *AptGetInstall) Run(ctx context.Context, opt RunOptions) error {
	args := []string{"apt-get", "install"}
	if c.Yes {
		args = append(args, "--yes")
	}
	args = append(args, c.Packages...)

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = opt.Stdout
	cmd.Stderr = opt.Stderr
	return cmd.Run()
}

func (c *AptGetInstall) PostRun(ctx context.Context) error {
	return recordInstalledPackages(c.Packages)
}

// recordInstalledPackages reads the existing package list, deduplicates, and writes them back
func recordInstalledPackages(packages []string) error {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/clawtainer"
	}

	filePath := filepath.Join(home, ".installed-packages.txt")

	existing := make(map[string]bool)
	if data, err := os.ReadFile(filePath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				existing[line] = true
			}
		}
	}

	for _, pkg := range packages {
		existing[pkg] = true
	}

	var all []string
	for pkg := range existing {
		all = append(all, pkg)
	}
	sort.Strings(all)

	var sb strings.Builder
	for _, pkg := range all {
		sb.WriteString(pkg)
		sb.WriteString("\n")
	}

	if err := os.MkdirAll(home, 0755); err != nil {
		return fmt.Errorf("failed to create home directory %s: %w", home, err)
	}

	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write installed packages list: %w", err)
	}

	return nil
}
