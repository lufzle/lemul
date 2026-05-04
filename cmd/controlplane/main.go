// Command controlplane serves the MANAGEMENT API: organizations, workspaces,
// sessions, the explorer verbs, and the runner and workspace CONTROL tunnels.
// The behaviour lives in internal/mgmtapi.
//
// It is half of the control plane, not all of it. The session data path is
// cmd/relay, which holds no database and carries only PTY bytes -- and the two
// never call each other. The binary keeps the name an operator already deploys
// and terraform already names; the package says which half it is.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lufzle/lemul/internal/creds"
	"github.com/lufzle/lemul/internal/directory"
	"github.com/lufzle/lemul/internal/logging"
	"github.com/lufzle/lemul/internal/mgmtapi"
	"github.com/lufzle/lemul/internal/statekey"
	"github.com/lufzle/lemul/internal/store"
)

func main() {
	var (
		addr      = flag.String("addr", ":9000", "listen address")
		publicURL = flag.String("public-url", "", "ws:// base URL agents dial back to (defaults to ws://localhost<addr>)")
		// Where the session relay listens. This process never calls it -- the
		// value is passed on to placed tasks, which dial it for their data
		// tunnel, and to clients, which are told it by endpoint negotiation.
		relayURL = flag.String("relay-url", os.Getenv("LEMUL_RELAY_URL"),
			"ws:// base URL of the session relay (cmd/relay); required for anything to attach")
		databaseURL = flag.String("database-url", os.Getenv("LEMUL_DATABASE_URL"),
			"postgres DSN for this cell (required)")
		// The global tier. A cell is its own database in a real deployment, so
		// this is a separate DSN -- but a single-cell development setup keeps both
		// tiers in one database, and defaulting to the cell's DSN is what makes
		// that one flag instead of two.
		directoryURL = flag.String("directory-url", os.Getenv("LEMUL_DIRECTORY_URL"),
			"postgres DSN for the global directory (empty reuses -database-url)")
		cellID      = flag.String("cell", mgmtapi.DefaultCellID, "id of the cell this control plane serves")
		region      = flag.String("region", "", "AWS region for workspace tasks, also recorded on the cell")
		stateDir    = flag.String("state-dir", "", "directory holding the signing key (empty = beside the binary's cwd)")
		applySchema = flag.Bool("apply-schema", false,
			"create tables and policies before starting (development convenience)")
		// A separate DSN because the application role cannot do DDL and must not
		// hold BYPASSRLS, while the role that CAN is exactly the one store.Open
		// refuses. Two roles, so two connection strings.
		schemaURL = flag.String("schema-url", os.Getenv("LEMUL_SCHEMA_URL"),
			"DDL-capable postgres DSN used only by -apply-schema (empty reuses -database-url)")
		printRunnerEnv = flag.String("print-runner-env", "",
			"print an organization's runner configuration as env vars and exit (takes its slug or its uuid)")
		sessionCmd = flag.String("session-cmd", "claude", "command a new session runs (space separated)")
		startWait  = flag.Duration("start-timeout", 90*time.Second, "how long to wait for a workspace task to dial in")
		reconnect  = flag.Duration("reconnect-grace", 10*time.Second, "how long a placed task may be disconnected before it is replaced")
		image      = flag.String("image", "", "image passed to the driver (ignored by the local driver)")
		// Defaulted from the environment like -database-url and the four -auth-*
		// flags, because a deployment configures this the same way it configures
		// those. They did NOT, and the failure was silent in the worst way: a
		// compose file that sets LEMUL_GATEWAY_URL looks configured, the control
		// plane starts fine, and the only symptom is the console reporting
		// inference as "Bedrock" -- which is the label for NO GATEWAY CONFIGURED
		// and reads like a statement about a backend. Sessions then get no model
		// at all.
		gatewayURL = flag.String("gateway-url", os.Getenv("LEMUL_GATEWAY_URL"),
			"gateway workspace tasks broker model traffic to")
		gatewayKey = flag.String("gateway-key", os.Getenv("LEMUL_GATEWAY_KEY"),
			"gateway credential (never reaches a session)")
		otelEndpoint = flag.String("otel-endpoint", "", "OTLP base URL for workspace telemetry (empty disables it)")
		otelHeaders  = flag.String("otel-headers", "", "OTLP headers, e.g. Authorization=Basic xxx")
		// protobuf, not json: OpenObserve's OTLP JSON parser rejects part of
		// Claude Code's payload with `invalid type: map, expected f64`, and the
		// failure is silent without CLAUDE_CODE_OTEL_DIAG_STDERR.
		otelProtocol = flag.String("otel-protocol", "http/protobuf", "OTLP protocol")
		otelTraces   = flag.Bool("otel-traces", false, "also export Claude Code's beta traces")
		bedrockPre   = flag.Bool("bedrock-preflight", false, "workspace tasks check their pinned Bedrock models at start")
		pins         = flag.String("pins", "", "comma-separated role=modelID pins; empty uses the defaults")
		// Bearer-token validation for the management API. Both default to the
		// environment so the console, the CLI and this process can be pointed at
		// one identity provider from a single generated env file
		// (auth-stack/seed.ts). Both are REQUIRED -- see below.
		authIssuer = flag.String("auth-issuer", os.Getenv("LEMUL_AUTH_ISSUER"),
			"OIDC issuer for management API tokens (required)")
		authAudience = flag.String("auth-audience", os.Getenv("LEMUL_AUTH_AUDIENCE"),
			"API resource indicator tokens must be minted for (required)")
		authJWKSURL = flag.String("auth-jwks-url", os.Getenv("LEMUL_AUTH_JWKS_URL"),
			"where to fetch signing keys (empty derives it as <issuer>/jwks)")
		// Advertised at GET /v1/auth/config so that `ourcli login` can discover
		// how to sign in from the address it was already given, instead of the
		// user exporting deployment facts into their shell.
		authCLIClientID = flag.String("auth-cli-client-id", os.Getenv("LEMUL_CLI_CLIENT_ID"),
			"public device-flow client id advertised to lem")
		// Not advertised, unlike the CLI's: the console is configured with its own
		// client id already. It is here only so ID tokens minted for it are
		// accepted at PUT /v1/identity.
		authConsoleClientID = flag.String("auth-console-client-id", os.Getenv("LEMUL_CONSOLE_CLIENT_ID"),
			"console OAuth client id whose ID tokens are accepted at PUT /v1/identity")
		// Derives every workspace and attach credential (internal/creds). It has
		// to outlive the process: see signingKey.
		signingKey = flag.String("signing-key", os.Getenv("LEMUL_SIGNING_KEY"),
			"key deriving workspace and attach credentials (empty reads or creates one beside -state)")
	)
	flag.Parse()
	logging.Setup("controlplane")

	key, err := statekey.LoadOrCreate(*signingKey, *stateDir)
	if err != nil {
		logging.Fatal("resolving the signing key", "error", err)
	}

	// Answered without opening anything: an operator needs this to configure a
	// runner, and making them stand up a database first would be gratuitous.
	// Both halves of a runner's configuration, because it needs both and they
	// are the two things nothing else can produce: the uuid is in the database
	// and the credential is a function of the signing key.
	//
	// Emitted as env vars rather than as a bare token so the whole thing is one
	// `eval`, and because the runner already reads both from the environment --
	// that is how Terraform delivers them into its task definition. Printed to
	// stdout with no logging framing, so it pipes cleanly.
	if *printRunnerEnv != "" {
		signer, err := creds.NewSigner(key)
		if err != nil {
			logging.Fatal("building the signer", "error", err)
		}
		// Takes the slug a person actually has, as well as the uuid the
		// credential is derived from. Resolving a slug needs the directory, so
		// the uuid form still works with no database at all -- which is the
		// case where an operator holds only the signing key.
		id, err := runnerTokenSubject(*printRunnerEnv, *directoryURL, *databaseURL)
		if err != nil {
			logging.Fatal("resolving the organization", "error", err)
		}
		// `export`, not a bare assignment. `eval "$(...)"` on the latter sets
		// shell variables the runner never sees, and the symptom is the runner
		// reporting -tenant missing while the operator is looking at the value
		// they just eval'd. Single-quoted because that is correct for any value,
		// including ones a future field might contain.
		fmt.Printf("export LEMUL_TENANT_ID='%s'\nexport LEMUL_RUNNER_TOKEN='%s'\n",
			id, signer.RunnerToken(id).Secret())
		return
	}

	// Refuse to start rather than warn and continue, in the same voice as
	// store.AssertNoBypassRLS -- and for the same reason. This process is
	// multi-tenant; a deployment that cannot tell two customers apart has
	// nothing left to enforce, and "the console was only on loopback" is not a
	// property anybody can check after the fact.
	//
	// There was an unauthenticated mode until Phase 4. It existed so the test
	// suite could run without an identity provider, and it was reachable by
	// OMITTING a flag. Tests now sign their own tokens (internal/authtest).
	if *authIssuer == "" || *authAudience == "" {
		logging.Fatal("-auth-issuer and -auth-audience are both required: a control plane " +
			"that does not validate tokens serves every organization to anyone who can " +
			"reach it. See auth-stack/README.md for a local identity provider")
	}

	if *databaseURL == "" {
		logging.Fatal("-database-url is required: the control plane's state is a " +
			"postgres cell, and there is no in-process fallback")
	}
	dsn := *directoryURL
	if dsn == "" {
		dsn = *databaseURL
	}
	ctx := context.Background()

	// Schema first, and on a DIFFERENT connection.
	//
	// The role the control plane runs as holds no DDL privileges -- it is the
	// role that must not hold BYPASSRLS either -- so applying the schema through
	// it is impossible by construction, and store.Open refuses the only role
	// that could. That is why this takes its own DSN and runs before anything
	// else opens a pool.
	if *applySchema {
		ddl := *schemaURL
		if ddl == "" {
			ddl = *databaseURL
		}
		if err := directory.ApplySchema(ctx, ddl); err != nil {
			logging.Fatal("applying the directory schema", "error", err)
		}
		if err := store.ApplySchema(ctx, ddl); err != nil {
			logging.Fatal("applying the cell schema", "error", err)
		}
		// And grant the application role access to what was just created.
		//
		// Without this the flag is a trap rather than a convenience: the tables
		// exist, the control plane starts, and the first query fails with
		// "permission denied" -- and the fix is a GRANT that has to run AFTER
		// the schema, which is a window this process does not leave open. The
		// role is read out of the application DSN, so there is nothing extra to
		// configure and nothing to keep in sync.
		if ddl != *databaseURL {
			if err := grantAppRole(ctx, ddl, *databaseURL); err != nil {
				logging.Fatal("granting the application role", "error", err)
			}
		}
		slog.Info("schema applied")
	}

	st, err := store.Open(ctx, *databaseURL)
	if err != nil {
		logging.Fatal("opening the cell database", "error", err)
	}
	defer st.Close()
	dir, err := directory.Open(ctx, dsn)
	if err != nil {
		logging.Fatal("opening the directory database", "error", err)
	}
	defer dir.Close()
	// Every organization this control plane creates is placed in this cell, and
	// the row has to exist before the first one can reference it.
	if err := dir.EnsureCell(ctx, *cellID, *region, "inline:-database-url"); err != nil {
		logging.Fatal("registering the cell", "error", err)
	}

	pub := *publicURL
	if pub == "" {
		pub = "ws://localhost" + *addr
	}

	s, err := mgmtapi.New(mgmtapi.Options{
		Store:          st,
		Directory:      dir,
		CellID:         *cellID,
		SessionCmd:     strings.Fields(*sessionCmd),
		StartTimeout:   *startWait,
		ReconnectGrace: *reconnect,
		PublicURL:      pub,
		RelayURL:       *relayURL,
		Image:          *image,

		GatewayURL:          *gatewayURL,
		GatewayKey:          *gatewayKey,
		OTelEndpoint:        *otelEndpoint,
		OTelHeaders:         *otelHeaders,
		OTelProtocol:        *otelProtocol,
		OTelTraces:          *otelTraces,
		BedrockPreflight:    *bedrockPre,
		Region:              *region,
		Pins:                *pins,
		AuthIssuer:          *authIssuer,
		AuthAudience:        *authAudience,
		AuthJWKSURL:         *authJWKSURL,
		AuthCLIClientID:     *authCLIClientID,
		AuthConsoleClientID: *authConsoleClientID,
		SigningKey:          key,
	})
	if err != nil {
		logging.Fatal("building the control plane", "error", err)
	}
	slog.Info("management API requires a bearer token",
		"issuer", *authIssuer, "audience", *authAudience)
	// Said at boot rather than discovered as a client attaching to a listener
	// with no attach handler. This process works perfectly well without it --
	// everything except attaching does -- which is exactly what makes the
	// omission quiet.
	if *relayURL == "" {
		slog.Warn("no -relay-url: sessions can be created but nothing can attach to them",
			"fix", "run cmd/relay and pass its ws:// address")
	} else {
		slog.Info("session relay", "url", *relayURL)
	}
	// Said at boot rather than left for the first operator to hit, because the
	// symptom is `lem login` failing on a control plane that looks correctly
	// configured from every other angle.
	if *authCLIClientID == "" {
		slog.Warn("no -auth-cli-client-id, so `lem login` cannot discover how to sign in")
	}

	// Section 2.4's auto-stop cascade. Started here rather than inside New so
	// tests can build a server without a sweeper running under them.
	s.Start(context.Background())

	srv := &http.Server{
		Addr:    *addr,
		Handler: s.Handler(),
		// No write timeout: these are long-lived WebSockets, and yamux
		// keepalive is what detects a dead peer (section 2.2).
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("listening", "addr", *addr, "cell", *cellID)
	logging.Fatal("http server stopped", "error", srv.ListenAndServe())
}

// runnerTokenSubject turns what an operator typed into the organization uuid the
// runner credential is derived from.
//
// A uuid passes straight through, so `-print-runner-token <uuid>` needs no
// database -- the value is a function of the signing key alone. Anything else
// is taken as a slug and resolved against the directory, because the slug is
// what a person has: it is in the URL they were looking at and in what `lem
// orgs` printed, while the uuid appears in a log line they would have to go
// find.
func runnerTokenSubject(arg, directoryDSN, databaseDSN string) (string, error) {
	if _, err := uuid.Parse(arg); err == nil {
		return arg, nil
	}
	dsn := directoryDSN
	if dsn == "" {
		dsn = databaseDSN
	}
	if dsn == "" {
		return "", fmt.Errorf("%q is not a uuid, and resolving a slug needs "+
			"-directory-url or -database-url", arg)
	}
	ctx := context.Background()
	dir, err := directory.Open(ctx, dsn)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	org, err := dir.OrgBySlug(ctx, arg)
	if err != nil {
		return "", fmt.Errorf("no organization with slug %q: %w", arg, err)
	}
	return org.ID, nil
}

// grantAppRole gives the role in appDSN access to the tables just created.
//
// Part of -apply-schema, which is a development convenience: in a deployment an
// operator runs the DDL and the grants together with a credential the control
// plane never holds. Doing it here removes the one ordering an operator cannot
// get right from a single command -- the grant has to follow the schema, and
// the process that applies the schema is the one that then needs the grant.
func grantAppRole(ctx context.Context, ddlDSN, appDSN string) error {
	u, err := neturl.Parse(appDSN)
	if err != nil {
		return fmt.Errorf("parsing the application DSN: %w", err)
	}
	role := u.User.Username()
	if role == "" {
		return errors.New("the application DSN names no user, so there is nobody to grant to")
	}
	pool, err := pgxpool.New(ctx, ddlDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Quoted as an identifier rather than interpolated: a role name is not a
	// value, so it cannot be a parameter, and that is exactly the shape that
	// turns into an injection if it is pasted in raw.
	q := pgx.Identifier{role}.Sanitize()
	for _, stmt := range []string{
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + q,
		`GRANT USAGE ON SCHEMA public TO ` + q,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	slog.Info("granted the application role", "role", role)
	return nil
}
