package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/sandbox"
	"durable-jobs/internal/scripts"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox", flag.ContinueOnError)
	flags.SetOutput(stderr)
	build := flags.Bool("build-image", false, "explicitly build the local trusted runner image")
	jobTemplate := flags.Bool("job", false, "print a pinned campaign_migration v1 job from -input; does not submit or execute")
	scriptPath := flags.String("script", "", "Python file defining transform(record)")
	inputPath := flags.String("input", "", "JSON object file")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *build {
		if *scriptPath != "" || *inputPath != "" || *jobTemplate {
			fmt.Fprintln(stderr, "build-image does not accept script/input")
			return 2
		}
		buildCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := sandbox.BuildImage(buildCtx, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "Local sandbox image is ready.")
		return 0
	}
	if *jobTemplate {
		if *scriptPath != "" || *inputPath == "" {
			fmt.Fprintln(stderr, "-job requires -input and disallows -script")
			return 2
		}
		input, err := readBoundedFile(*inputPath, sandbox.MaxInputBytes)
		if err != nil {
			fmt.Fprintln(stderr, "invalid input file")
			return 2
		}
		jobCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		runner, err := sandbox.New(jobCtx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		body, err := scriptJob(input, runner.ImageID())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if _, err := fmt.Fprintln(stdout, string(body)); err != nil {
			return 1
		}
		return 0
	}
	if *scriptPath == "" || *inputPath == "" {
		fmt.Fprintln(stderr, "both -script and -input are required")
		return 2
	}
	script, scriptErr := readBoundedFile(*scriptPath, sandbox.MaxScriptBytes)
	input, inputErr := readBoundedFile(*inputPath, sandbox.MaxInputBytes)
	request := sandbox.Request{Script: string(script), Input: input}
	if scriptErr != nil || inputErr != nil || request.Validate() != nil {
		fmt.Fprintln(stderr, "invalid or oversized script/input; script <=16 KiB, JSON object input <=64 KiB")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	runner, err := sandbox.New(ctx)
	result := sandbox.Result{Status: "failed", CleanupConfirmed: true}
	if err != nil {
		result.FailureCode = err.Error() // New returns only stable, safe codes.
	} else {
		result = runner.Run(ctx, request)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(stderr, "cannot write sandbox result")
		return 1
	}
	if result.Status != "succeeded" {
		return 1
	}
	return 0
}

func scriptJob(input []byte, imageID string) ([]byte, error) {
	payload, err := json.Marshal(scripts.Payload{Script: scripts.CampaignName, Version: scripts.CampaignVersion,
		SourceSHA256: scripts.CampaignSHA256, ImageID: imageID, Input: input})
	if err != nil {
		return nil, errors.New("invalid JSON input")
	}
	if _, err := scripts.Parse(payload); err != nil {
		return nil, err
	}
	body, err := json.Marshal(jobs.SubmitRequest{Type: "run_script", Payload: payload})
	if err != nil || len(body) > 64<<10 {
		return nil, errors.New("job envelope exceeds the API's 64 KiB limit")
	}
	return body, nil
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, errors.New("invalid_input_file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot_read_input")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("input_read_failed_or_oversized")
	}
	return data, nil
}
