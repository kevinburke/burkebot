package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Project represents one managed repo/task set.
type Project struct {
	// Name is a short slug used in URLs (e.g. "returns").
	Name string `json:"name"`

	// Repo is the GitHub repo (e.g. "kevinburke/returns").
	Repo string `json:"repo"`

	// AuditDir is the path to audit bundles for this project.
	AuditDir string `json:"audit_dir"`

	// StateFile is the path to the processed-prs.json for this project.
	StateFile string `json:"state_file"`

	// Service is the systemd service name to start for reruns.
	// Defaults to "burkebot.service" if empty.
	Service string `json:"service"`

	// RepoDir is the local checkout path used for ad hoc prompt runs.
	// If empty, the dashboard derives it from Repo or Name.
	RepoDir string `json:"repo_dir"`
}

// ServiceName returns the systemd service to use for this project.
func (p *Project) ServiceName() string {
	if p.Service != "" {
		return p.Service
	}
	return "burkebot.service"
}

// RepoBaseName returns the local checkout directory name for the project.
func (p *Project) RepoBaseName() string {
	if p.RepoDir != "" {
		return filepath.Base(p.RepoDir)
	}
	if p.Repo != "" {
		return filepath.Base(strings.TrimSpace(p.Repo))
	}
	return p.Name
}

// RepoDirectory resolves the local checkout path for ad hoc prompt runs.
func (p *Project) RepoDirectory(repoRoot string) string {
	if p.RepoDir != "" {
		return p.RepoDir
	}
	base := p.RepoBaseName()
	if repoRoot == "" || base == "" {
		return ""
	}
	return filepath.Join(repoRoot, base)
}

// loadProjects reads a projects.json config file. If the file does not exist,
// returns nil. The caller should fall back to flag-based single-project config.
func loadProjects(path string) ([]Project, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return projects, nil
}

// discoverProjects scans a data directory for project subdirectories.
// Each subdirectory that contains an audit/ subdir is treated as a project.
// This allows zero-config discovery when the layout follows convention:
//
//	dataDir/
//	  returns/
//	    audit/
//	    processed-prs.json
//	  finance/
//	    audit/
//	    processed-prs.json
func discoverProjects(dataDir string) ([]Project, error) {
	entries, err := os.ReadDir(dataDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var projects []Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		auditDir := filepath.Join(dataDir, e.Name(), "audit")
		if info, err := os.Stat(auditDir); err != nil || !info.IsDir() {
			continue
		}
		projects = append(projects, Project{
			Name:      e.Name(),
			AuditDir:  auditDir,
			StateFile: filepath.Join(dataDir, e.Name(), "processed-prs.json"),
		})
	}
	return projects, nil
}
