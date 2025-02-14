package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"
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
	configEnclosed    = false
	configTimeout     = 30 * time.Minute
	configFilter      = ""
	configRulesPreset = cli.RulesPreset{Val: preset.Secret}
	configRetry       = cli.RetryStrategy{Val: cli.RetryStrategyValNever}
)

// buffer wraps the content and its target URI.
type buffer struct {
	TargetURI string
	Content   []byte
}

// main is the main entry point of the command.
func main() {
	// Define command
	var cmd = &cobra.Command{
		Use: `troll-a [flags] [url]

This tool allows to extract (potential) secrets such as passwords, API keys, and tokens
from WARC (Web ARChive) files. Extracted information is output as structured text org
JSON, which simplifies further processing of the data.

"url" can be either a regular HTTP or HTTPS reference ("https://domain/path"), an Amazon
S3 reference ("s3://bucket/path"), a file path (either "file:///path" or simply "path"),
or a dash ("-") to read from STDIN. If "url" is missing data is read from STDIN. If the
input data is compressed with either GZip, BZip2, XZ, or ZStd it is automatically
decompressed. ZStd with a prepended custom dictionary (as used by "*.megawarc.warc.zstd")
is also handled transparently.

This tool uses rules from the Gitleaks project (https://gitleaks.io) to detect secrets.`,
		Short:             "Drill into WARC web archives",
		Args:              cobra.MaximumNArgs(1),
		Version:           Version,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		Run:               runCommand,
	}

	// Settings
	cmd.Flags().BoolVarP(&configQuiet, "quiet", "q", configQuiet, `suppress success message(s)`)
	cmd.Flags().BoolVarP(&configJSON, "json", "s", configJSON, `output detected secrets as JSON`)
	cmd.Flags().UintVarP(&configJobs, "jobs", "j", configJobs, `detect secrets with this many concurrent jobs`)
	cmd.Flags().BoolVarP(&configEnclosed, "enclosed", "e", configEnclosed, `only report secrets that are enclosed within their context`)
	cmd.Flags().DurationVarP(&configTimeout, "timeout", "t", configTimeout, `fetching timeout (does not apply to files)`)

	cmd.Flags().StringVarP(&configFilter, "filter", "f", configFilter, `filter for the target URL of each WARC record. Only WARC
records that match the given regular expression (using RE2
syntax) will be checked for secrets. An empty filter will
match everything.`)

	cmd.Flags().VarP(&configRulesPreset, "preset", "p", `rules preset to use. This could be one of the following:
all:         All known rules will be applied, which can
             result in a significant amount of noise for
             large data sets.
most:        Most of the rules are applied, skipping the
             biggest culprits for false positives.
secret:      Only rules are applied that are most likely
             to result in an actual leak of a secret.
No other values are allowed.`)

	cmd.Flags().VarP(&configRetry, "retry", "r", `retry strategy to use. This could be one of the following:
never:       This strategy will fail after the first fetch
             failure and will not attempt to retry.
constant:    This strategy will attempt to retry up to 5
             times, with a 5s delay after each attempt.
exponential: This strategy will attempt to retry for 15
             minutes, with an exponentially increasing
             delay after each attempt.
always:      This strategy will attempt to retry forever,
             with no delay at all after each attempt.
No other values are allowed.`)

	// Version should include regular expression engine
	cmd.SetVersionTemplate(`{{printf "%s version %s" .Name .Version}}-` + detect.AbstractRegexpEngine)

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

	// Create filter regular expression
	var filter detect.AbstractRegexp

	if configFilter != "" {
		f, err := detect.CompileRegexp(configFilter)
		if err != nil {
			cli.Error(`Error: Invalid filter regular expression ["%s"]`, err)
			os.Exit(1) //nolint
		}

		filter = f
	}

	// Read from STDIN if no parameter is given
	var inputURL string

	if len(args) > 0 {
		inputURL = args[0]
	}

	// Open reader for URL
	fr, err := fetch.Open(
		inputURL,
		fetch.WithTimeout(configTimeout),
		fetch.WithBackoff(configRetry.Val),
	)

	if err != nil {
		cli.Error(`Error: Failed to fetch WARC file ["%s"]`, err)
		os.Exit(1) //nolint
	}

	defer fr.Close()

	// Decompress, if necessary
	dr, err := fetch.NewDecompressionReader(fr)
	if err != nil {
		cli.Error(`Error: Failed to decompress WARC file ["%s"]`, err)
		os.Exit(1) //nolint
	}

	defer dr.Close()

	// Channel for communication between WARC traversal and secret detection
	bufferCh := make(chan *buffer)

	// Spawn go routines to check buffers for secrets
	eg, ctx := errgroup.WithContext(context.Background())

	for j := uint(0); j < configJobs; j++ {
		eg.Go(NewSecretsDetectorFunc(bufferCh, detector, configJSON))
	}

	// Traverse WARC file
	var recordCount atomic.Uint64

	err = warc.Traverse(dr, NewWARCTraversalFunc(ctx.Done(), filter, bufferCh, &recordCount))
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

	// Dump success message
	if !configQuiet {
		cli.Success("Success: Processed %s (%d records)", inputURL, recordCount.Load())
	}
}

// NewSecretsDetectorFunc returns a new function that reads buffers from channel in and processes them using
// the given detector. If asJSON is set found secrets will be written to STDOUT in JSON format, otherwise found
// secrets will be written semi-structured to STDOUT.
func NewSecretsDetectorFunc(in <-chan *buffer, detector *detect.Detector, asJSON bool) func() error {
	return func() error {
		// Read next buffer
		for b := range in {
			// Detect secrets
			findings, err := detector.Detect(bytes.NewBuffer(b.Content))
			if err != nil {
				return fmt.Errorf("detect secrets: %w", err)
			}

			for _, f := range findings {
				// Print findings
				if asJSON {
					// JSON
					_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
						"secret":  f.Secret,
						"rule":    f.RuleID,
						"uri":     b.TargetURI,
						"line":    f.Location.StartLine,
						"column":  f.Location.StartColumn,
						"context": f.Location.Line(string(b.Content)),
					})
				} else {
					// Terminal
					cli.Info(
						`Detected: secret="%s" rule="%s" uri="%s" line=%d column=%d`,
						f.Secret,
						f.RuleID,
						b.TargetURI,
						f.Location.StartLine,
						f.Location.StartColumn,
					)
				}
			}
		}

		return nil
	}
}

// NewWARCTraversalFunc ...
func NewWARCTraversalFunc(done <-chan struct{}, filter detect.AbstractRegexp, out chan<- *buffer, count *atomic.Uint64) func(*warc.Record) error {
	return func(r *warc.Record) error {
		select {
		case <-done:
			// Break traversal if jobs have stopped
			return warc.ErrBreakTraversal

		default:
			// Bail if wrong type or payload
			if (r.Type != warc.RecordTypeResponse) || (!mime.IsText(r.IdentifiedPayloadType) && !mime.IsText(r.HTTPContentType)) {
				return nil
			}

			// Bail if filter is not matched
			if (filter != nil) && !filter.MatchString(r.TargetURI) {
				return nil
			}

			// Read full record content
			content, err := io.ReadAll(r.Content)
			if err != nil {
				return fmt.Errorf("read record content: %w", err)
			}

			// Hand over to processing
			out <- &buffer{
				TargetURI: r.TargetURI,
				Content:   content,
			}

			// Increment record count, if given
			if count != nil {
				count.Add(1)
			}
		}

		return nil
	}
}
