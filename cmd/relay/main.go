// Command relay is the session data path.
//
// It carries PTY bytes between a client and a workspace task, and nothing else.
// No database, no directory, no runner: everything it needs to serve a request
// travels inside the signed attach credential the Management API minted.
//
//	relay -addr :9001 -state-dir ./.state
//
// The signing key must be the SAME one the Management API uses, because that is
// what makes a credential minted there verifiable here. Point both at one
// -state-dir locally; in production both read one secret.
//
// It is a separate binary rather than a flag on the control plane on purpose.
// The whole value of the split is that this process cannot read a session's
// directory listings, process lists or command lines -- and a combined mode
// would leave the split path as the one nothing routinely runs, which is the
// path the architecture depends on.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/lufzle/lemul/internal/logging"
	"github.com/lufzle/lemul/internal/relay"
	"github.com/lufzle/lemul/internal/statekey"
)

func main() {
	var (
		addr       = flag.String("addr", ":9001", "listen address")
		stateDir   = flag.String("state-dir", "", "directory holding the signing key (must match the control plane's)")
		signingKey = flag.String("signing-key", os.Getenv("LEMUL_SIGNING_KEY"),
			"key verifying workspace and attach credentials (empty reads one beside -state-dir)")
	)
	flag.Parse()
	logging.Setup("relay")

	// Read, never create.
	//
	// The control plane generates a key when it finds none, because it is the
	// thing that owns one. If this process did the same it would come up with a
	// key nothing else shares and reject every credential it was handed --
	// reported as "unauthorized" on a correctly minted token, which points at
	// the credential rather than at the configuration. Refusing to start says
	// what is actually wrong.
	key, err := statekey.Load(*signingKey, *stateDir)
	if err != nil {
		logging.Fatal("resolving the signing key", "error", err)
	}

	s, err := relay.New(relay.Options{SigningKey: key})
	if err != nil {
		logging.Fatal("building the relay", "error", err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: s.Handler(),
		// No write timeout: these are long-lived WebSockets, and yamux
		// keepalive is what detects a dead peer (section 2.2).
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("listening", "addr", *addr)
	logging.Fatal("http server stopped", "error", srv.ListenAndServe())
}
