// Command cortex — `cortex down` wire-up.
//
// This file binds the `cortex down` cobra command to the orchestration
// in internal/infra. It owns the interactive confirmation prompt for
// --purge; the actual "stop containers" and "remove volumes" logic
// lives in infra.Down which shells to DockerRunner.ComposeDown.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Volume Topology and Persistence"
//   docs/spec/cortex-spec.md SC-020 (log.d is untouched by --purge)
//   docs/spec/cortex-spec.md US-10 BDD row 52 (cortex down stops stack)
package main

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/nixlim/cortex/internal/infra"
	"github.com/spf13/cobra"
)

// runDown is the RunE for `cortex down`. The purge flag is captured in
// commands.go newDownCmd() and forwarded here so this file stays free
// of cobra-flag bookkeeping.
func runDown(cmd *cobra.Command, _ []string, purge bool) error {
	composeFile := filepath.Join("docker", "docker-compose.yaml")

	opts := infra.DownOptions{
		ComposeFile: composeFile,
		Purge:       purge,
		Docker:      infra.ExecDocker{},
	}
	if purge {
		opts.Confirm = interactiveConfirm(cmd.OutOrStdout(), cmd.InOrStdin())
	}

	if err := infra.Down(cmd.Context(), opts); err != nil {
		return err
	}

	if purge {
		fmt.Fprintln(cmd.OutOrStdout(),
			"cortex: managed stack stopped; named volumes removed. "+
				"~/.cortex/log.d/ is unchanged.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(),
			"cortex: managed stack stopped; named volumes preserved.")
	}
	return nil
}

// interactiveConfirm returns a Confirm callback that writes the prompt
// to w and reads a single line of response from r. A reply of "y" or
// "yes" (case-insensitive) is consent; anything else — including empty
// input, EOF, or whitespace — is treated as refusal. Read errors other
// than io.EOF are surfaced to the caller so infra.Down can classify
// them as PURGE_CANCELLED with the original error chained.
//
// Conservative default: unrecognised input is "no". This matches the
// "[y/N]" style prompt the spec asks for and means an operator who
// pipes /dev/null into `cortex down --purge` gets a safe refusal
// rather than an accidental volume wipe.
func interactiveConfirm(w io.Writer, r io.Reader) func(string) (bool, error) {
	return func(prompt string) (bool, error) {
		if _, err := fmt.Fprint(w, prompt); err != nil {
			return false, err
		}
		reader := bufio.NewReader(r)
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

