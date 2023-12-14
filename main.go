package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/crissyfield/troll-a/pkg/detect"
	"github.com/crissyfield/troll-a/pkg/detect/preset"
	"github.com/crissyfield/troll-a/pkg/fetch"
	"github.com/crissyfield/troll-a/pkg/mime"
	"github.com/crissyfield/troll-a/pkg/warc"

	"github.com/crissyfield/troll-a/internal/cli"
)

var (
	// Version will be set during build.
	Version = "(unknown)"

	// Configuration
	configQuiet       = false
	configJSON        = false
	configJobs        = uint(8)
	configRetry       = cli.RetryStrategy{Val: cli.RetryStrategyValNever}
	configRulesPreset = cli.RulesPreset{Val: preset.Secret}
	configEnclosed    = false
)

// main is the main entry point of the command.
func main() {
	// Define command
	var cmd = &cobra.Command{
		Use:               "troll-a [flags] url",
		Short:             "Drill into WARC web archives",
		Args:              cobra.ExactArgs(1),
		Version:           Version,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		Run:               runCommand,
	}

	// Settings
	cmd.Flags().BoolVarP(&configQuiet, "quiet", "q", configQuiet, `suppress success message`)
	cmd.Flags().BoolVarP(&configJSON, "json", "s", configJSON, `change output format of detected secrets to JSON`)
	cmd.Flags().UintVarP(&configJobs, "jobs", "j", configJobs, `number of concurrent jobs to detect secrets`)
	cmd.Flags().BoolVarP(&configEnclosed, "enclosed", "e", configEnclosed, `only report secrets that are clearly enclosed by their context`)

	cmd.Flags().VarP(&configRetry, "retry", "r", `retry strategy to use. This could be one of the following:
never:       This strategy will fail after the first fetch failure
             and will not attempt to retry.
constant:    This strategy will attempt to retry up to 5 times,
             with a 5 second delay after each attempt.
exponential: This strategy will attempt to retry for 15 minutes,
             with an exponentially increasing delay after each
             attempt.
always:      This strategy will attempt to retry forever, with no
             delay at all after each attempt.
No other values are allowed.`)

	cmd.Flags().VarP(&configRulesPreset, "preset", "p", `rules preset to use. This could be one of the following:
all:         All known rules will be applied, which can result in
             a significant amount of noise for large data sets.
most:        Most of the rules are applied, skipping the biggest
             culprits for false positives.
secret:      Only those rules are applied that are most likely to
             result in an actual leak of a secret.
No other values are allowed.`)

	// Execute
	if err := cmd.Execute(); err != nil {
		// Error has already been printed, just exit
		os.Exit(1)
	}
}

// runCommand is called when the command is used.
func runCommand(_ *cobra.Command, args []string) {
	// Create detector on given rules preset
	detector := detect.NewDetector(configRulesPreset.Val, configEnclosed)

	// Open reader for URL
	r, err := fetch.URL(
		args[0],
		fetch.WithTimeout(4*time.Hour),
		fetch.WithBackoff(configRetry.Val),
	)

	if err != nil {
		cli.Error(`Error: Failed to fetch WARC file ["%s"]`, err)
		os.Exit(1) //nolint
	}

	defer r.Close()

	// Create buffer channel
	type buffer struct {
		TargetURI string
		Content   []byte
	}

	bufferCh := make(chan *buffer)

	// Spawn go routines to check buffers for secrets
	eg, ctx := errgroup.WithContext(context.Background())

	for j := uint(0); j < configJobs; j++ {
		eg.Go(func() error {
			for buf := range bufferCh {
				// Detect secrets
				findings, err := detector.Detect(bytes.NewBuffer(buf.Content))
				if err != nil {
					return fmt.Errorf("detect secrets: %w", err)
				}

				// Print findings
				for _, f := range findings {
					if configJSON {
						// JSON
						_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
							"secret":  f.Secret,
							"rule":    f.RuleID,
							"uri":     buf.TargetURI,
							"line":    f.Location.StartLine,
							"column":  f.Location.StartColumn,
							"context": f.Location.Line(string(buf.Content)),
						})
					} else {
						// Terminal
						cli.Info(
							`Detected: secret="%s" rule="%s" uri="%s" line=%d column=%d`,
							f.Secret,
							f.RuleID,
							buf.TargetURI,
							f.Location.StartLine,
							f.Location.StartColumn,
						)
					}
				}
			}

			return nil
		})
	}

	// Traverse WARC file
	err = warc.Traverse(r, func(r *warc.Record) error {
		select {
		case <-ctx.Done():
			// Break traversal if jobs have stopped
			return warc.ErrBreakTraversal

		default:
			// Bail if wrong type or payload
			if (r.Type != warc.RecordTypeResponse) || !mime.IsText(r.IdentifiedPayloadType) {
				return nil
			}

			// Read full record content
			content, err := io.ReadAll(r.Content)
			if err != nil {
				return fmt.Errorf("read record content: %w", err)
			}

			// Hand over to processing
			bufferCh <- &buffer{
				TargetURI: r.TargetURI,
				Content:   content,
			}
		}

		return nil
	})

	if err != nil {
		cli.Error(`Error: Failed to process WARC file ["%s"]`, err)
		os.Exit(1) //nolint
	}

	// Clean up
	close(bufferCh)

	err = eg.Wait()
	if err != nil {
		cli.Error(`Error: Failed to detect secrets ["%s"]`, err)
		os.Exit(1) //nolint
	}

	cli.Info("Success: Processed %s", args[0])
}
