package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCLIHelp(t *testing.T) {
	const argumentsEnv = "CONTENTKIT_CLI_SUBPROCESS_ARGS"
	if encoded, child := os.LookupEnv(argumentsEnv); child {
		var args []string
		if err := json.Unmarshal([]byte(encoded), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"contentkit"}, args...)
		main()
		os.Exit(0)
	}

	for _, tc := range []struct {
		name  string
		args  []string
		usage string
	}{
		{"root", []string{"--help"}, "Usage of contentkit serve:"},
		{"serve", []string{"serve", "--help"}, "Usage of contentkit serve:"},
		{"media-config", []string{"media-config", "--help"}, "Usage of contentkit media-config:"},
		{"invalid-flag", []string{"--not-a-real-option"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			encoded, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIHelp$")
			cmd.Env = append(os.Environ(), argumentsEnv+"="+string(encoded))
			// No configuration exists here: help must finish before loading it
			// or initializing any server resources.
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			if tc.usage == "" {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
					t.Fatalf("invalid flag exit: %v; output: %s", err, out)
				}
				if !bytes.Contains(out, []byte("flag provided but not defined")) {
					t.Fatalf("missing flag error: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("help exit: %v; output: %s", err, out)
			}
			if !bytes.Contains(out, []byte(tc.usage)) {
				t.Fatalf("missing usage: %s", out)
			}
			if bytes.Contains(out, []byte(`"level":"ERROR"`)) {
				t.Fatalf("help logged an error: %s", out)
			}
		})
	}
}
