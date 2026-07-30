// Command preflight checks that a set of pinned Bedrock models is usable from
// wherever it runs.
//
// It is a standalone binary as well as a library because it serves two
// audiences: our own onboarding runbook, where a customer runs it in their
// account before anything else exists, and the workspace task, which runs the
// same checks with the sandbox task role.
//
// Usage:
//
//	AWS_PROFILE=… preflight -region us-east-2
//	preflight -pins opus=us.anthropic.claude-opus-5,haiku=us.anthropic.claude-haiku-4-5-20251001-v1:0
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/lufzle/lemul-cc/internal/bedrock"
)

func main() {
	var (
		region  = flag.String("region", "", "AWS region (defaults to the environment's)")
		pinSpec = flag.String("pins", "", "comma-separated role=modelID pins; empty uses the defaults")
		timeout = flag.Duration("timeout", 2*time.Minute, "overall timeout")
	)
	flag.Parse()

	pins := bedrock.DefaultPins()
	if *pinSpec != "" {
		var err error
		if pins, err = bedrock.ParsePins(*pinSpec); err != nil {
			fmt.Fprintln(os.Stderr, "preflight:", err)
			os.Exit(2)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var opts []func(*awscfg.LoadOptions) error
	if *region != "" {
		opts = append(opts, awscfg.WithRegion(*region))
	}
	cfg, err := awscfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "preflight: load AWS config:", err)
		os.Exit(2)
	}
	if cfg.Region == "" {
		fmt.Fprintln(os.Stderr, "preflight: no region; pass -region or set AWS_REGION")
		os.Exit(2)
	}

	rep := bedrock.Preflight(ctx, cfg, pins)
	fmt.Print(rep)

	if !rep.OK() {
		fmt.Fprintln(os.Stderr, "\npreflight FAILED: a required model is not usable")
		os.Exit(1)
	}
	if len(rep.Problems()) > 0 {
		fmt.Println("\npreflight passed with warnings: an optional model is not usable")
		return
	}
	fmt.Println("\npreflight passed")
}
