// Package bedrock verifies that a set of pinned models is actually usable.
//
// Three layers, because each catches a different failure (section 8):
//
//  1. GetFoundationModelAvailability, asserting authorizationStatus ==
//     AUTHORIZED. This catches the common "the account never completed model
//     access" case with a message that names the fix.
//  2. A real minimal InvokeModel. Only this proves entitlement end to end --
//     ListInferenceProfiles returning ACTIVE is NOT an entitlement check, as a
//     test account that listed 25 Anthropic profiles ACTIVE and could invoke
//     none demonstrated.
//  3. Error mapping. NOT_AUTHORIZED and "Operation not allowed" mean different
//     things and have different fixes, and the difference between "your IAM
//     policy is wrong" and "your account lacks model access" is the difference
//     between a two-minute fix and an AWS support case.
//
// Layer 1 is advisory and layer 2 is authoritative. The pins are inference
// profile IDs (us.anthropic.…), while the availability API wants a foundation
// model ID, so layer 1 can legitimately come back inconclusive -- that must not
// mask a model that in fact invokes cleanly.
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
)

// Roles a pinned model can play. Claude Code resolves each independently, and
// they are not equally important: it uses Haiku for cheap internal work on
// nearly every turn, so an unusable Haiku pin degrades every session.
const (
	RoleOpus   = "opus"
	RoleSonnet = "sonnet"
	RoleHaiku  = "haiku"
)

// Pin is one configured model.
type Pin struct {
	Role    string
	ModelID string
	// Required fails the preflight when the model is unusable. Opus (the
	// default model) and Haiku are required; Sonnet is not, because a session
	// only reaches it if the user switches, and refusing to start over it would
	// block accounts that are otherwise perfectly usable.
	Required bool
}

// DefaultPins is the pin set to fall back to when a tenant configures none.
//
// Unpinned is not an option: the docs warn that an unpinned deployment resolves
// to Claude Code's built-in Bedrock default and gets billed at Opus rates
// (section 3.1).
func DefaultPins() []Pin {
	return []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
		{Role: RoleHaiku, ModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0", Required: true},
		{Role: RoleSonnet, ModelID: "us.anthropic.claude-sonnet-4-6", Required: false},
	}
}

// Result is one model's verdict.
type Result struct {
	Pin
	// Authorization is the layer-1 answer, or "UNKNOWN" if it could not be
	// determined for this model ID.
	Authorization string
	// Invocable is the layer-2 answer: the only one that proves entitlement.
	Invocable bool
	Err       error
	// Advice is the actionable half of layer 3.
	Advice string
}

func (r Result) OK() bool { return r.Invocable }

// Report is the whole preflight.
type Report struct {
	Region  string
	Results []Result
}

// OK reports whether every required model is invocable.
func (rep Report) OK() bool {
	for _, r := range rep.Results {
		if r.Required && !r.OK() {
			return false
		}
	}
	return true
}

// Problems returns the results that should be surfaced, required or not.
func (rep Report) Problems() []Result {
	var out []Result
	for _, r := range rep.Results {
		if !r.OK() {
			out = append(out, r)
		}
	}
	return out
}

func (rep Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bedrock preflight (%s)\n", rep.Region)
	for _, r := range rep.Results {
		status := "FAIL"
		if r.OK() {
			status = "ok"
		} else if !r.Required {
			status = "WARN"
		}
		fmt.Fprintf(&b, "  %-5s %-6s %-45s auth=%s\n", status, r.Role, r.ModelID, r.Authorization)
		if r.Err != nil {
			fmt.Fprintf(&b, "        %v\n", r.Err)
		}
		if r.Advice != "" {
			fmt.Fprintf(&b, "        -> %s\n", r.Advice)
		}
	}
	return b.String()
}

// API surfaces are interfaces so the layers can be tested without AWS.
type availabilityAPI interface {
	GetFoundationModelAvailability(context.Context, *bedrock.GetFoundationModelAvailabilityInput, ...func(*bedrock.Options)) (*bedrock.GetFoundationModelAvailabilityOutput, error)
}

type invokeAPI interface {
	InvokeModel(context.Context, *bedrockruntime.InvokeModelInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

// Preflight checks every pin.
func Preflight(ctx context.Context, cfg aws.Config, pins []Pin) Report {
	return preflight(ctx, bedrock.NewFromConfig(cfg), bedrockruntime.NewFromConfig(cfg), cfg.Region, pins)
}

func preflight(ctx context.Context, avail availabilityAPI, inv invokeAPI, region string, pins []Pin) Report {
	rep := Report{Region: region}
	for _, p := range pins {
		rep.Results = append(rep.Results, checkOne(ctx, avail, inv, p))
	}
	return rep
}

func checkOne(ctx context.Context, avail availabilityAPI, inv invokeAPI, p Pin) Result {
	res := Result{Pin: p, Authorization: "UNKNOWN"}

	// Layer 1, advisory.
	if avail != nil {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out, err := avail.GetFoundationModelAvailability(cctx, &bedrock.GetFoundationModelAvailabilityInput{
			ModelId: aws.String(baseModelID(p.ModelID)),
		})
		cancel()
		if err == nil {
			res.Authorization = string(out.AuthorizationStatus)
		}
		// An error here is deliberately not fatal. The pins are inference
		// profile IDs and this API wants a foundation model ID, so a lookup can
		// fail for a model that invokes perfectly well. Layer 2 decides.
	}

	// Layer 2, authoritative.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := inv.InvokeModel(cctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(p.ModelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        []byte(minimalRequest),
	})
	if err == nil {
		res.Invocable = true
		if res.Authorization == "UNKNOWN" {
			// It invoked, so it is authorized whatever layer 1 could not tell us.
			res.Authorization = string(types.AuthorizationStatusAuthorized)
		}
		return res
	}

	res.Err = err
	res.Advice = advise(err, res.Authorization, p)
	return res
}

// minimalRequest is the smallest valid Anthropic-on-Bedrock request: one token
// out, a single short turn. Cheap enough to run on every start.
const minimalRequest = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,` +
	`"messages":[{"role":"user","content":"hi"}]}`

// baseModelID strips a cross-region inference profile prefix, since the
// availability API takes a foundation model ID. Unknown shapes are passed
// through untouched.
func baseModelID(modelID string) string {
	for _, prefix := range []string{"us.", "eu.", "apac.", "us-gov."} {
		if after, ok := strings.CutPrefix(modelID, prefix); ok {
			return after
		}
	}
	return modelID
}

// advise turns an AWS error into the fix. This is layer 3, and it is the whole
// difference between an onboarding that looks broken and one that tells the
// customer what to do.
func advise(err error, authorization string, p Pin) string {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return "could not reach Bedrock: check the region, VPC endpoint and network path (section 3.3)"
	}
	msg := ae.ErrorMessage()

	// Authorization status is consulted FIRST because it is the least ambiguous
	// signal we have. A denied account answers InvokeModel with
	// `ValidationException: Operation not allowed`, which is indistinguishable
	// by code from a malformed model ID -- and telling a customer whose real
	// problem is the First Time Use form to "check the pin and the region" sends
	// them down a dead end. Verified against a genuinely denied account.
	if authorization == string(types.AuthorizationStatusNotAuthorized) {
		return modelAccessAdvice(p)
	}

	switch ae.ErrorCode() {
	case "AccessDeniedException":
		// Two very different failures share this code, and they are told apart
		// by whether IAM refused the call or Bedrock refused the model.
		if strings.Contains(msg, "bedrock:InvokeModel") || strings.Contains(msg, "not authorized to perform") {
			return "the task role lacks bedrock:InvokeModel on this model. " +
				"Fix the sandbox task role policy (section 2.1); this is not a model-access problem"
		}
		return modelAccessAdvice(p)
	case "ValidationException":
		// "Operation not allowed" is what a denied account gets here. It is
		// called out by name in section 8 precisely because it looks like a
		// validation problem and is not one.
		if strings.Contains(msg, "Operation not allowed") ||
			strings.Contains(msg, "not authorized") || strings.Contains(msg, "access") {
			return modelAccessAdvice(p)
		}
		if strings.Contains(msg, "on-demand throughput") || strings.Contains(msg, "inference profile") {
			return "this model requires an inference profile. Pin the cross-region profile ID " +
				"(us.anthropic.…) rather than the bare foundation model ID"
		}
		return "the model ID is not valid in this region: check the pin and the region"
	case "ResourceNotFoundException":
		return "no such model in this region. Either the pin is wrong or the workspace is in " +
			"the wrong region: pins are region-specific"
	case "ThrottlingException", "TooManyRequestsException":
		return "throttled, which is a quota problem rather than a configuration one. " +
			"Preflight cannot confirm access; retry, and request a quota increase if it persists"
	}
	return "unexpected Bedrock error: " + ae.ErrorCode()
}

func modelAccessAdvice(p Pin) string {
	return fmt.Sprintf("the account does not have model access for %s. Submit the Anthropic "+
		"First Time Use form in the Bedrock console (once per account, or inherited from the AWS "+
		"Org management account; granted instantly). Write a substantive use-case description -- "+
		"a denial has NO self-service recovery and needs an AWS support case", p.ModelID)
}
