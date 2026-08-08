# ADR-0025: Terminate mTLS on the Fleet Adapter ControlStream

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** API Designer, Security Reviewer, Distributed Systems Reviewer
- **Related:** RFC-0001 §9.2.7 (transport security / mTLS), §9.5.1.2 (per-message robot authorization), §9.1.12 (FleetAdapter presence phase); backlog §F (transport security); ADR-0021 (fail-closed gate semantics); `internal/controlstream/identity.go` (the SAN identity contract, unchanged by this ADR)

## Context

RFC-0001 §9.2.7 requires mutual TLS on every Fleet Adapter connection: the
ControlStream is the channel over which adapters report robot state and receive
action assignments, estops, and lease renewals, so it is a security boundary. The
reference control plane does not yet terminate it.

The facts as built:

1. **The listener is plaintext.** `internal/controlstream/runnable.go` builds the
   server with `grpc.NewServer()` and no transport credentials; `Start` logs a
   prominent "serving ControlStream WITHOUT mTLS (dev mode)" warning. The
   type doc already records that mTLS is backlog §F.

2. **Identity is derived only from a verified client-certificate chain.**
   `IdentityFromContext` (`identity.go`) returns a `Verified` `TLSIdentity` **only**
   when `tls.ConnectionState.VerifiedChains` is populated and a leaf SAN parses as
   `<adapter>.<namespace>.svc.cluster.local`. `PeerCertificates` alone is
   deliberately not trusted. With a plaintext listener there is no TLS auth info,
   so every connection yields the zero identity (`Verified: false`).

3. **Presence is gated on `identity.Verified`, and it is stuck.** In `server.go`
   the connectivity signal fires only for a verified adapter
   (`if s.Presence != nil && identity.Verified { s.Presence.AdapterConnected(...) }`,
   server.go:364; the pre-handshake guard at :354 refuses per-robot messages when
   `!identity.Verified`). Because the plaintext listener can never produce a
   verified identity, `AdapterConnected`/`AdapterHeartbeat` are never called, the
   `FleetAdapter` never advances to `Connected` (§9.1.12), and robots admitted
   through that adapter stay `Discovered`/`Initialising` — the presence→Ready path
   cannot complete. Transport security is therefore not only a §9.2.7 compliance
   gap; it blocks the core adapter lifecycle.

4. **`--fleet-adapter-insecure-authz` is orthogonal and does not fill the gap.**
   It only sets the `Server.Authorizer` to `nil`, activating the existing dev-mode
   path where every robot-scoped message is authorized regardless of identity
   (cmd/manager/main.go:476–483). It adds no transport, and it does **not**
   synthesize a verified identity, so even with it set, presence stays gated off.

5. **`AdapterHello` is self-reported and is not the boundary.** The adapter names
   itself in the handshake, but `identity.go` is explicit that this is not proof of
   anything; only the mTLS SAN is. Any design that authenticates from `AdapterHello`
   would be trusting client-supplied data.

Constraints: the SAN parsing in `identity.go` is correct and must not change — this
work supplies the verified transport *underneath* it. The change must follow the
repo's fail-closed security posture (cf. ADR-0021) and must not perturb the RA-1
status discipline (presence is already event-driven, never telemetry-driven).

## Decision

Terminate mTLS on the ControlStream listener, and make a secured listener the
default — a plaintext listener becomes an explicit dev-only opt-in.

- **Transport.** `NewGRPCRunnable` gains a `*tls.Config` parameter and stores it.
  In `Start`, when the config is non-nil the server is built with
  `grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))` and logs "serving
  ControlStream with mTLS"; when nil it keeps the existing plaintext path and the
  "WITHOUT mTLS (dev mode)" warning. The runnable does not construct crypto policy;
  it applies the config it is handed.

- **Server TLS policy (built in `cmd/manager/main.go`).** When TLS material is
  provided the config sets `ClientAuth: tls.RequireAndVerifyClientCert` (so the
  handshake fails closed for a missing or untrusted client cert, which is what
  populates `VerifiedChains` for the identity layer), a `ClientCAs` pool built from
  the configured client-CA PEM, the server `Certificates`, and
  `MinVersion: tls.VersionTLS13`.

- **Three manager flags:** `--fleet-adapter-tls-cert-file`,
  `--fleet-adapter-tls-key-file`, `--fleet-adapter-client-ca-file`. A `*tls.Config`
  is built only when all three are set.

- **Secured-only by default (fail closed).** If the ControlStream bind address is
  set but no TLS config could be built and `--fleet-adapter-insecure-authz` is
  false, the manager logs an error and does **not** register the ControlStream
  server (the feature stays off rather than serving plaintext). A plaintext
  listener is reachable **only** under `--fleet-adapter-insecure-authz`, which
  thereby becomes the single switch for the whole dev posture (no transport
  identity + no per-message authorization together).

- **Server-cert hot reload.** The server certificate is served through a
  `GetCertificate` closure that re-reads the keypair, so rotation (cert-manager
  renewal) takes effect without a manager restart — the same reload intent as the
  webhook serving cert. Kept minimal; the client-CA pool is read at startup.

- **Turnkey overlay `config/controlstream-tls/`.** A cert-manager `Certificate`
  for the server cert (`dnsNames: swarmada-controlstream.<ns>.svc` and
  `swarmada-controlstream.<ns>.svc.cluster.local`, matching the existing
  ControlStream `Service` name) issued by a CA `Issuer`; a strategic-merge patch on
  the manager `Deployment` mounting the server-cert `Secret` and the client-CA and
  setting the three flags; a `Service`; and a `kustomization.yaml` layering on
  `config/default`. This is the production counterpart to
  `config/overlays/quickstart-dev` (which stays dev-only, plaintext + insecure).

The SAN parsing in `identity.go` is untouched: once `RequireAndVerifyClientCert`
and the client-CA pool are in place, `VerifiedChains` is populated and
`IdentityFromContext` begins returning `Verified` identities for well-formed SANs
with no change to `parseAdapterSAN`.

## Alternatives considered

- **Synthesize a `Verified` identity from `AdapterHello` in insecure mode.** This
  would unblock presence without certificates, but it authenticates from
  client-supplied data — trivially spoofable, and directly contrary to the
  `identity.go` contract that only a verified SAN is the boundary. Rejected: it
  would turn a dev convenience into a way to forge adapter identity.

- **Plaintext by default, mTLS opt-in.** Smaller migration, but it makes the
  insecure posture the default for a security control — fail-open. A deployment
  that simply doesn't set the flags would silently run without transport security.
  Rejected: the safe state must be the default; opting *out* (via the existing
  dev-only flag) is the correct direction.

- **Application-layer tokens (bearer/JWT over plaintext).** Authenticates the
  adapter but leaves the channel unencrypted and unintegrity-protected, and is
  replayable without channel binding; §9.2.7 mandates mTLS specifically. Rejected:
  it does not provide the confidential, mutually-authenticated channel the spec
  requires, and adds a second credential system.

- **Defer §F again.** Rejected: besides leaving the §9.2.7 gap open, it leaves the
  presence→Ready path unreachable (context §3), so the adapter lifecycle stays
  broken in any deployment that expects robots to reach `Ready`.

## Consequences

- **The channel is authenticated and confidential.** Every accepted adapter
  presents a CA-issued client cert with a well-formed SAN; the per-message
  authorizer (§9.5.1.2) now has a real identity to check against, fail-closed.

- **Presence→Ready works.** A verified adapter reaches `Connected` and heartbeats
  (§9.1.12); robots admitted through it can progress to `Ready`. This is the
  primary functional win and must be covered by a test (verified-connection →
  `FleetAdapter Connected` + a robot reaching `Ready`).

- **New operational obligations.** Adapters must now be issued client certs with
  the `<adapter>.<namespace>.svc.cluster.local` SAN; the manager needs a server
  cert and the client-CA. The `config/controlstream-tls` overlay makes this
  turnkey (cert-manager), and hot reload avoids restart-on-rotation — but
  cert-manager is now an assumed dependency of that overlay (it is the first
  cert-manager consumer wired in `config/`; `config/default` still does not
  provision certs). Certs must be valid for the Service DNS because the server runs
  on every replica (`NeedLeaderElection() == false`), not the leader alone.

- **Behavior change for plaintext-default deployments — release-note it.** Any
  deployment that relied on the plaintext default (bind address set, no TLS
  material, no insecure flag) will now find the ControlStream server unregistered
  and must either provision TLS (recommended) or set
  `--fleet-adapter-insecure-authz` explicitly. The flag's documented meaning
  broadens: it is now also the only route to a plaintext listener. This is the
  intended fail-closed migration, not a regression.

- **Seams left for the future.** Client-CA rotation still needs a restart (only the
  server keypair hot-reloads); SPIFFE/short-lived-identity issuance and CRL/OCSP
  revocation are out of scope; multi-replica presence coordination is unchanged
  (still future work). The RA-1 status discipline is unaffected — presence remains
  driven by connect/heartbeat/stream-loss, never by telemetry frames.
