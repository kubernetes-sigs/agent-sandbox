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

type AptGetUpdate struct {
}

// parseAptGetUpdate parses a command to check if it is an apt-get update command.
func parseAptGetUpdate(args []string) *AptGetUpdate {
	if len(args) == 2 && args[0] == "apt-get" && args[1] == "update" {
		return &AptGetUpdate{}
	}

	return nil
}

func (c *AptGetUpdate) Run(ctx context.Context, opt RunOptions) error {
	args := []string{"apt-get", "update"}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = opt.Stdout
	cmd.Stderr = opt.Stderr
	return cmd.Run()
}

func (c *AptGetUpdate) PostRun(ctx context.Context) error {
	return nil
}

type AptGetInstall struct {
	Packages []string
	Yes      bool
}

// parseAptGetInstall parses a command to check if it is an apt-get install command.
func parseAptGetInstall(args []string) *AptGetInstall {
	if len(args) == 0 {
		return nil
	}

	if len(args) >= 4 {
		if args[0] == "apt-get" && args[1] == "install" && args[2] == "--yes" {
			packageNames := args[3:]
			allValid := true
			for _, p := range packageNames {
				if !isPackageName(p) {
					allValid = false
					break
				}
			}
			if allValid {
				return &AptGetInstall{
					Packages: packageNames,
					Yes:      true,
				}
			}
		}
	}

	return nil
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
