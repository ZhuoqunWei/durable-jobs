package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"durable-jobs/internal/harness"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("harness", flag.ContinueOnError)
	flags.SetOutput(stderr)
	list := flags.Bool("list", false, "list named scenarios without using PostgreSQL")
	selection := flags.String("scenario", "all", "all or comma-separated scenario IDs")
	jsonOutput := flags.Bool("json", false, "print the structured report instead of the text summary")
	directory := flags.String("report-dir", ".local/harness", "directory for a new JSON report; empty disables saving")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	selected, err := harness.Select(*selection)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *list {
		for _, item := range selected {
			fmt.Fprintf(stdout, "%s\n  %s\n", item.ID, item.Expected)
		}
		return 0
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "cannot locate repository root")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	report, runErr := harness.Run(ctx, root, os.Getenv("DATABASE_URL"), selected)
	if runErr != nil && report.Problem == "" {
		report.Problem = runErr.Error()
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintln(stderr, "cannot write report output")
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "Durable Jobs reliability harness — %s (%.2fs)\n", strings.ToUpper(report.Status), report.Seconds)
		for _, result := range report.Results {
			fmt.Fprintf(stdout, "%-10s %-24s %.2fs\n  Expected: %s\n  Observed: %s; %d subtest outcomes\n", strings.ToUpper(result.Status), result.ID, result.Seconds, result.Expected, result.Status, len(result.Checks))
			for _, evidence := range result.Evidence {
				fmt.Fprintf(stdout, "  Fixture: %s\n", evidence)
			}
		}
		if report.Problem != "" {
			fmt.Fprintln(stdout, "Problem:", report.Problem)
		}
	}
	if *directory != "" {
		path, err := saveReport(*directory, report)
		if err != nil {
			fmt.Fprintln(stderr, "could not save report:", err)
			return 1
		}
		fmt.Fprintln(stderr, "Report:", path)
	}
	if runErr != nil {
		return 1
	}
	return 0
}

func saveReport(directory string, report harness.Report) (string, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	// A new private file on every run: no previous report is overwritten.
	file, err := os.CreateTemp(directory, "report-"+time.Now().UTC().Format("20060102T150405Z")+"-*.json")
	if err != nil {
		return "", err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(report)
	closeErr := file.Close()
	if encodeErr != nil {
		return "", encodeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return filepath.Abs(file.Name())
}
