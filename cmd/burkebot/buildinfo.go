package main

import (
	"os/exec"
	"runtime/debug"
	"strings"
	"time"
)

// buildInfo holds version control information read from the Go binary.
type buildInfo struct {
	Version   string
	Commit    string
	BuildDate string
	Modified  bool
}

// pageContext is embedded in every template data struct to provide
// common fields needed by the shared footer.
type pageContext struct {
	RenderStart time.Time
}

// newPageContext returns a pageContext with RenderStart set to now.
func newPageContext() pageContext {
	return pageContext{RenderStart: time.Now()}
}

// readBuildInfo extracts VCS info from the Go binary's debug metadata,
// falling back to git commands if the binary lacks embedded VCS info.
func readBuildInfo() buildInfo {
	info := buildInfo{Version: Version}
	bi, ok := debug.ReadBuildInfo()
	if ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.time":
				info.BuildDate = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if info.Commit == "" {
		info.Commit, info.BuildDate, info.Modified = readGitInfo()
	}
	return info
}

// readGitInfo shells out to git to get commit, date, and dirty status.
func readGitInfo() (commit, buildDate string, modified bool) {
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "show", "--no-patch", "--format=%cI", "HEAD").Output(); err == nil {
		buildDate = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
		modified = len(strings.TrimSpace(string(out))) > 0
	}
	return
}
