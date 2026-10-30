package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/colinedwardwood/llmtap/internal/redact"
	replayfile "github.com/colinedwardwood/llmtap/internal/replay"
)

func runReplay(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "base URL to replay against")
	out := fs.String("out", "", "write replay results as redacted JSONL")
	timeout := fs.Duration("timeout", 2*time.Minute, "timeout per request")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 || *target == "" {
		return errors.New("replay: usage: llmtap replay FILE --target URL [--out FILE]")
	}
	base, err := url.Parse(*target)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return errors.New("replay: target must be an absolute http(s) URL")
	}
	entries, err := replayfile.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	recorder := replayfile.NewRecorder(*out, redact.Func(redact.ProfileDefault))
	client := &http.Client{Timeout: *timeout}
	for i, entry := range entries {
		if entry.RequestTruncated {
			return fmt.Errorf("replay entry %d: request was truncated during recording", i)
		}
		ref, err := url.Parse(entry.Path)
		if err != nil {
			return fmt.Errorf("replay entry %d path: %w", i, err)
		}
		endpoint := base.ResolveReference(ref)
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		req, err := http.NewRequestWithContext(ctx, entry.Method, endpoint.String(), bytes.NewBufferString(entry.Request))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if token := os.Getenv("LLMTAP_REPLAY_AUTHORIZATION"); token != "" {
			req.Header.Set("Authorization", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return fmt.Errorf("replay entry %d: %w", i, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			return readErr
		}
		result := entry
		result.Status = resp.StatusCode
		result.Response = string(body)
		if err := recorder.Append(result); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "%d\t%s\t%d\n", i, endpoint.Redacted(), resp.StatusCode)
	}
	return nil
}

func runDiff(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("diff: usage: llmtap diff LEFT.jsonl RIGHT.jsonl")
	}
	left, err := replayfile.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	right, err := replayfile.Load(fs.Arg(1))
	if err != nil {
		return err
	}
	diffs := replayfile.Diff(left, right)
	for _, diff := range diffs {
		_, _ = fmt.Fprintf(stdout, "entry %d %s\n  - %s\n  + %s\n",
			diff.Index, diff.Field, diff.Left, diff.Right)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("diff: %d difference(s)", len(diffs))
	}
	_, _ = fmt.Fprintln(stdout, "recordings match")
	return nil
}
