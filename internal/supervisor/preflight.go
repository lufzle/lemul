package supervisor

import (
	"context"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/lufzle/lemul/internal/bedrock"
	"github.com/lufzle/lemul/internal/tunnel"
)

// preflightReport runs the Bedrock check once per task and caches the result.
//
// Once per task, not once per tunnel connect: the tunnel reconnects on every
// relay redeploy and network blip, and re-invoking a model each time would be
// noise. The cached report is re-sent on every connect instead, so a control
// plane that restarted still learns this workspace's verdict.
//
// Once per TASK rather than cached longer, though -- a task restart re-checks.
// Model access is often granted after a first failure, and that is the whole
// recovery path, so a verdict that outlived the task would keep a fixed account
// looking broken.
func (s *Supervisor) preflightReport(ctx context.Context) tunnel.PreflightReport {
	s.preflightOnce.Do(func() {
		s.preflight = s.runPreflight(ctx)
	})
	return s.preflight
}

func (s *Supervisor) runPreflight(ctx context.Context) tunnel.PreflightReport {
	rep := tunnel.PreflightReport{
		WorkspaceID: s.opt.WorkspaceID,
		CheckedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	// A workspace that is not using Bedrock has nothing to check. Reporting
	// "skipped" rather than staying silent matters: the control plane treats an
	// absent report as "not yet" and a skipped one as "not applicable", and
	// conflating them would either block local sessions or fail open in
	// production.
	if !s.opt.BedrockPreflight {
		s.log.Info("bedrock preflight skipped: workspace is not using bedrock")
		rep.Skipped = true
		return rep
	}

	pins := s.opt.Pins
	if len(pins) == 0 {
		pins = bedrock.DefaultPins()
	}

	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var opts []func(*awscfg.LoadOptions) error
	if s.opt.Region != "" {
		opts = append(opts, awscfg.WithRegion(s.opt.Region))
	}
	cfg, err := awscfg.LoadDefaultConfig(cctx, opts...)
	if err != nil {
		// No credentials at all is a configuration failure like any other, and
		// it is blocking: without them no session can reach a model.
		s.log.Error("bedrock preflight could not load AWS config", "error", err)
		rep.Blocking = true
		rep.Models = []tunnel.PreflightModel{{
			Role:     "credentials",
			Required: true,
			Error:    err.Error(),
			Advice: "the task could not load AWS credentials. On Fargate these come from the " +
				"container credential provider; note it requires TEMPORARY credentials, and a " +
				"long-term IAM key fails here with a misleading 'could not load credentials' error",
		}}
		return rep
	}
	rep.Region = cfg.Region

	res := bedrock.Preflight(cctx, cfg, pins)
	rep.Blocking = res.Blocking()
	for _, r := range res.Results {
		m := tunnel.PreflightModel{
			Role:          r.Role,
			ModelID:       r.ModelID,
			Required:      r.Required,
			Authorization: r.Authorization,
			Invocable:     r.Invocable,
			ErrorCode:     r.ErrorCode(),
			Advice:        r.Advice,
		}
		if r.Err != nil {
			m.Error = r.Err.Error()
		}
		rep.Models = append(rep.Models, m)
	}

	if rep.Blocking {
		s.log.Error("bedrock preflight FAILED", "report", res.String())
	} else {
		s.log.Info("bedrock preflight ok", "region", res.Region)
	}
	return rep
}
