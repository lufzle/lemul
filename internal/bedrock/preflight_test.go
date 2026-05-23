package bedrock

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
)

type fakeAvail struct {
	status types.AuthorizationStatus
	err    error
	gotID  string
}

func (f *fakeAvail) GetFoundationModelAvailability(_ context.Context, in *bedrock.GetFoundationModelAvailabilityInput, _ ...func(*bedrock.Options)) (*bedrock.GetFoundationModelAvailabilityOutput, error) {
	f.gotID = aws.ToString(in.ModelId)
	if f.err != nil {
		return nil, f.err
	}
	return &bedrock.GetFoundationModelAvailabilityOutput{AuthorizationStatus: f.status}, nil
}

type fakeInvoke struct {
	err    error
	gotID  string
	called int
}

func (f *fakeInvoke) InvokeModel(_ context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	f.called++
	f.gotID = aws.ToString(in.ModelId)
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockruntime.InvokeModelOutput{}, nil
}

func apiErr(code, msg string) error {
	return &smithy.GenericAPIError{Code: code, Message: msg}
}

// The case that matters most, verified against a genuinely denied account:
// a denied account answers InvokeModel with `ValidationException: Operation not
// allowed`, which by error code alone is indistinguishable from a malformed
// model ID. Sending that customer to "check the pin and the region" is a dead
// end -- their real fix is the First Time Use form (section 13).
func TestDeniedAccountGetsModelAccessAdvice(t *testing.T) {
	avail := &fakeAvail{status: types.AuthorizationStatusNotAuthorized}
	inv := &fakeInvoke{err: apiErr("ValidationException", "Operation not allowed")}

	rep := preflight(context.Background(), avail, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
	})

	if rep.OK() {
		t.Fatal("report says OK for a denied account")
	}
	advice := rep.Results[0].Advice
	if !strings.Contains(advice, "First Time Use") {
		t.Errorf("advice does not mention the FTU form:\n  %s", advice)
	}
	if strings.Contains(advice, "check the pin and the region") {
		t.Errorf("advice sends the customer down the wrong path:\n  %s", advice)
	}
	if !strings.Contains(advice, "no self-service recovery") &&
		!strings.Contains(advice, "NO self-service recovery") {
		t.Errorf("advice omits that a denial needs a support case:\n  %s", advice)
	}
}

// "Operation not allowed" must map correctly even when layer 1 could not answer,
// since the availability lookup can fail for a valid inference profile ID.
func TestOperationNotAllowedMapsWithoutLayerOne(t *testing.T) {
	avail := &fakeAvail{err: errors.New("no such foundation model")}
	inv := &fakeInvoke{err: apiErr("ValidationException", "Operation not allowed")}

	rep := preflight(context.Background(), avail, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
	})
	if a := rep.Results[0].Advice; !strings.Contains(a, "First Time Use") {
		t.Errorf("advice = %q, want the model-access path", a)
	}
}

// An IAM problem and a model-access problem share the AccessDeniedException
// code and have completely different fixes.
func TestIAMDenialIsNotConfusedWithModelAccess(t *testing.T) {
	inv := &fakeInvoke{err: apiErr("AccessDeniedException",
		"User: arn:aws:sts::1:assumed-role/x is not authorized to perform: bedrock:InvokeModel")}

	rep := preflight(context.Background(), nil, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
	})
	advice := rep.Results[0].Advice
	if !strings.Contains(advice, "task role") {
		t.Errorf("advice does not point at the task role policy:\n  %s", advice)
	}
	if strings.Contains(advice, "First Time Use") {
		t.Errorf("an IAM failure was misreported as a model-access failure:\n  %s", advice)
	}
}

func TestInferenceProfileAdvice(t *testing.T) {
	inv := &fakeInvoke{err: apiErr("ValidationException",
		"Invocation of model ID anthropic.claude-opus-5 with on-demand throughput isn't supported. "+
			"Retry your request with the ID or ARN of an inference profile that contains this model.")}

	rep := preflight(context.Background(), nil, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "anthropic.claude-opus-5", Required: true},
	})
	if a := rep.Results[0].Advice; !strings.Contains(a, "inference profile") {
		t.Errorf("advice = %q, want the inference-profile hint", a)
	}
}

// Throttling is a quota problem, not a configuration one; reporting it as
// missing access would send someone to fill in a form they already completed.
func TestThrottlingIsNotReportedAsMissingAccess(t *testing.T) {
	inv := &fakeInvoke{err: apiErr("ThrottlingException", "Too many requests")}
	rep := preflight(context.Background(), nil, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
	})
	a := rep.Results[0].Advice
	if !strings.Contains(a, "quota") {
		t.Errorf("advice = %q, want a quota hint", a)
	}
	if strings.Contains(a, "First Time Use") {
		t.Errorf("throttling was misreported as missing model access:\n  %s", a)
	}
}

// Layer 2 is authoritative. A model that invokes cleanly is usable even when
// layer 1 could not look it up -- and the inference-profile IDs we pin are
// exactly the shape layer 1 cannot resolve.
func TestInvokeSucceedsDespiteUnknownAvailability(t *testing.T) {
	avail := &fakeAvail{err: errors.New("ValidationException: bad model id")}
	inv := &fakeInvoke{}

	rep := preflight(context.Background(), avail, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
	})
	if !rep.OK() {
		t.Fatal("a model that invoked cleanly was reported unusable")
	}
	if got := rep.Results[0].Authorization; got != string(types.AuthorizationStatusAuthorized) {
		t.Errorf("authorization = %q, want AUTHORIZED once the invoke proved it", got)
	}
}

// The converse, which is the whole reason layer 2 exists: layer 1 can report
// AUTHORIZED for a model that cannot actually be invoked.
func TestAuthorizedButNotInvocableFails(t *testing.T) {
	avail := &fakeAvail{status: types.AuthorizationStatusAuthorized}
	inv := &fakeInvoke{err: apiErr("ValidationException",
		"Invocation of model ID x with on-demand throughput isn't supported.")}

	rep := preflight(context.Background(), avail, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "anthropic.claude-opus-5", Required: true},
	})
	if rep.OK() {
		t.Fatal("AUTHORIZED was accepted as proof of entitlement; only InvokeModel is")
	}
}

// Opus and Haiku block startup; Sonnet only warns.
func TestOnlyRequiredPinsFailTheReport(t *testing.T) {
	inv := &fakeInvoke{err: apiErr("ValidationException", "nope")}

	optional := preflight(context.Background(), nil, inv, "us-east-2", []Pin{
		{Role: RoleSonnet, ModelID: "us.anthropic.claude-sonnet-4-6", Required: false},
	})
	if !optional.OK() {
		t.Error("an unusable optional pin failed the report")
	}
	if len(optional.Problems()) != 1 {
		t.Error("an unusable optional pin was not surfaced as a problem")
	}

	required := preflight(context.Background(), nil, inv, "us-east-2", []Pin{
		{Role: RoleHaiku, ModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0", Required: true},
	})
	if required.OK() {
		t.Error("an unusable required pin passed the report")
	}
}

// Layer 1 wants a foundation model ID; the pins are inference profile IDs.
func TestBaseModelIDStripsRegionPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"us.anthropic.claude-opus-5":   "anthropic.claude-opus-5",
		"eu.anthropic.claude-opus-5":   "anthropic.claude-opus-5",
		"apac.anthropic.claude-opus-5": "anthropic.claude-opus-5",
		"anthropic.claude-opus-5":      "anthropic.claude-opus-5",
	} {
		if got := baseModelID(in); got != want {
			t.Errorf("baseModelID(%q) = %q, want %q", in, got, want)
		}
	}

	avail := &fakeAvail{status: types.AuthorizationStatusAuthorized}
	inv := &fakeInvoke{}
	preflight(context.Background(), avail, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5"},
	})
	if avail.gotID != "anthropic.claude-opus-5" {
		t.Errorf("availability was asked for %q, want the base model ID", avail.gotID)
	}
	if inv.gotID != "us.anthropic.claude-opus-5" {
		t.Errorf("invoke used %q, want the pinned inference profile ID", inv.gotID)
	}
}

func TestDefaultPinsMarkSonnetOptional(t *testing.T) {
	for _, p := range DefaultPins() {
		want := p.Role != RoleSonnet
		if p.Required != want {
			t.Errorf("pin %s: Required = %v, want %v", p.Role, p.Required, want)
		}
		if !strings.HasPrefix(p.ModelID, "us.") {
			t.Errorf("pin %s (%s) is not a cross-region inference profile ID", p.Role, p.ModelID)
		}
	}
}

// Bedrock reuses error codes across unrelated causes, so the message has to be
// consulted before the code. Both of these were found against a real account
// (112324749796) that answered them for different models in the same run, and
// both were mapped to the wrong fix before that.

// A 404 whose message is about the use case form is NOT "wrong region", and the
// fix is self-service rather than a support case -- this account had simply
// never submitted the form.
func TestUseCaseFormNeverSubmittedIsNotAPinProblem(t *testing.T) {
	inv := &fakeInvoke{err: apiErr("ResourceNotFoundException",
		"Model use case details have not been submitted for this account. Fill out the Anthropic "+
			"use case details form before using the model. If you have already filled out the form, "+
			"try again in 15 minutes.")}

	rep := preflight(context.Background(), nil, inv, "us-east-2", []Pin{
		{Role: RoleHaiku, ModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0", Required: true},
	})
	a := rep.Results[0].Advice
	if strings.Contains(a, "wrong region") || strings.Contains(a, "pin is wrong") {
		t.Errorf("a use-case-form failure was reported as a region or pin problem:\n  %s", a)
	}
	if !strings.Contains(a, "use case details form") {
		t.Errorf("advice does not name the form:\n  %s", a)
	}
	if !strings.Contains(a, "15 minutes") {
		t.Errorf("advice omits the propagation delay, which invites a false retry:\n  %s", a)
	}
	if strings.Contains(a, "support case") {
		t.Errorf("a never-submitted form is self-service; advice should not mention support:\n  %s", a)
	}
}

// A 403 saying the model is not offered to this account is not a model-access
// problem: no form and no IAM change fixes it.
func TestModelNotOfferedIsNotAModelAccessProblem(t *testing.T) {
	avail := &fakeAvail{status: types.AuthorizationStatusAuthorized}
	inv := &fakeInvoke{err: apiErr("AccessDeniedException",
		"anthropic.claude-opus-5 is not available for this account. You can explore other available "+
			"models on Amazon Bedrock. For additional access options, contact AWS Sales")}

	rep := preflight(context.Background(), avail, inv, "us-east-2", []Pin{
		{Role: RoleOpus, ModelID: "us.anthropic.claude-opus-5", Required: true},
	})
	a := rep.Results[0].Advice
	if strings.Contains(a, "First Time Use") {
		t.Errorf("a not-offered model was reported as missing model access:\n  %s", a)
	}
	if strings.Contains(a, "task role") {
		t.Errorf("a not-offered model was reported as an IAM problem:\n  %s", a)
	}
	if !strings.Contains(a, "not offered") {
		t.Errorf("advice does not say the model is unavailable to the account:\n  %s", a)
	}
}
