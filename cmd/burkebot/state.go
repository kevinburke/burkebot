package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// ProcessedPREntry is the per-PR record written by burkebot-run.
type ProcessedPREntry struct {
	HeadSHA           string `json:"head_sha"`
	BaseSHA           string `json:"base_sha"`
	RecreateRequested bool   `json:"recreate_requested"`
}

// ProcessedPRState maps PR number (as string) to the recorded entry.
type ProcessedPRState map[string]ProcessedPREntry

// loadProcessedPRs reads the processed-prs.json state file.
// Returns an empty map if the file does not exist.
func loadProcessedPRs(path string) (ProcessedPRState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(ProcessedPRState), nil
	}
	if err != nil {
		return nil, err
	}
	var state ProcessedPRState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if state == nil {
		state = make(ProcessedPRState)
	}
	return state, nil
}

// saveProcessedPRs atomically writes the state file via temp + rename.
func saveProcessedPRs(path string, state ProcessedPRState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "processed-prs.*.json")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// startBurkebotService starts the named systemd service without blocking.
func startBurkebotService(logger *slog.Logger, service string) error {
	cmd := exec.Command("systemctl", "start", "--no-block", service)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("systemctl start failed", "error", err, "output", string(output))
		return fmt.Errorf("systemctl start: %w: %s", err, output)
	}
	return nil
}
