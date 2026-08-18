// Package config loads local credentials for the command-line tools.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadLocalEnv reads ~/.anchor/env and sets any variable not already present in
// the environment.
//
// It exists so the demo commands work without remembering to source a file
// first. Forgetting is easy, the failure is a bare "required" error, and the
// worst moment to meet it is halfway through recording a demo. Variables already
// set win, so an explicit `FOO=bar go run ...` still overrides the file.
//
// Only the local developer path uses this. The deployed Lambda reads its
// credential from AWS Secrets Manager and never touches the filesystem.
func LoadLocalEnv() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.Open(filepath.Join(home, ".anchor", "env"))
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Strip one layer of matching quotes, which is how the file is written.
		if len(val) >= 2 {
			if (val[0] == '\'' && val[len(val)-1] == '\'') ||
				(val[0] == '"' && val[len(val)-1] == '"') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" || val == "" {
			continue
		}
		if _, present := os.LookupEnv(key); !present {
			_ = os.Setenv(key, val)
		}
	}
}
