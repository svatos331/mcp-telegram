# Cloud Run deployment

HTTP mode is always protected by the embedded OAuth 2.1 server. Each OAuth
authorization creates an independent Telegram MTProto session and a durable
refresh-token family. There is no unauthenticated HTTP mode.

## One-time setup

```bash
PROJECT=my-project
REGION=europe-west1
DATABASE=mcp-telegram
SA=mcp-telegram@${PROJECT}.iam.gserviceaccount.com

gcloud firestore databases create --project=${PROJECT} --database=${DATABASE} \
  --location=${REGION} --type=firestore-native
gcloud iam service-accounts create mcp-telegram --project=${PROJECT}
gcloud projects add-iam-policy-binding ${PROJECT} \
  --member=serviceAccount:${SA} --role=roles/datastore.user \
  --condition="expression=resource.name == 'projects/${PROJECT}/databases/${DATABASE}',title=mcp-telegram-database"

printf '%s' "$MCP_TELEGRAM_API_HASH" | \
  gcloud secrets create mcp-telegram-api-hash --data-file=-
head -c 32 /dev/urandom | base64 | tr -d '\n' | \
  gcloud secrets create mcp-telegram-token-key --data-file=-
for s in mcp-telegram-api-hash mcp-telegram-token-key; do
  gcloud secrets add-iam-policy-binding "$s" \
    --member=serviceAccount:${SA} --role=roles/secretmanager.secretAccessor
done
```

Copy `deploy/cloudrun.yaml.example` to the untracked
`deploy/cloudrun.yaml`, fill in the Firestore project/database, allowlist, API ID, and issuer URL.
Keep the `.yaml` extension: `--env-vars-file` picks its parser from the file
name, and a path ending in `.env` is read as dotenv (`KEY=value`) rather than
as the YAML mapping this file uses.
Do not add `PORT`: Cloud Run injects it into the ingress container and the
application binds to `:$PORT`. Cloud Run does not expand `$PORT` inside another
environment variable, so `MCP_HTTP_ADDR=$PORT` is not a valid substitute.
`MCP_HTTP_ADDR` remains available as an explicit override outside Cloud Run;
the ordinary local default stays `127.0.0.1:8080`.

## Deploy

```bash
gcloud run deploy mcp-telegram \
  --source . \
  --region=${REGION} \
  --service-account=${SA} \
  --max-instances=1 \
  --env-vars-file=deploy/cloudrun.yaml \
  --set-secrets=MCP_TELEGRAM_API_HASH=mcp-telegram-api-hash:latest,MCP_AUTH_TOKEN_KEYS=mcp-telegram-token-key:latest \
  --allow-unauthenticated
```

`--allow-unauthenticated` exposes the OAuth protocol and login page; the MCP
endpoint itself rejects requests without a valid bearer token. Keep
`--max-instances=1`: two instances must never connect the same MTProto session
at once because Telegram can revoke the duplicated auth key.

Do not point an uptime check at `/healthz`. Cloud Run's frontend reserves that
exact path and answers it with its own 404 before the request reaches the
container, and the server exposes no other unauthenticated health route:
`/health` and `/healthz/` fall through to the bearer-gated `/` and return 401.
Probe `/.well-known/oauth-protected-resource` instead, which the application
serves and which reflects its configuration.

The service enforces a 1 MiB MCP request body, 32 KiB headers, 128 concurrent
HTTP requests, a ten-minute MCP session timeout, and a per-user limit of ten
new sessionless MCP sessions per minute (burst three). `MCP_AUTH_TRUSTED_PROXY_HOPS=0`
ignores all forwarding headers. Increase it only after verifying every address
in the actual proxy chain.

## Token and storage model

- Authorization codes have a random durable `jti` and can be redeemed once.
- Refresh tokens carry a family id and generation. Rotation is an atomic CAS.
  Reuse or concurrent replay of an old generation revokes the whole family,
  its Telegram session, and the live pooled client.
- A storage failure returns `503`; it does not consume or rotate the grant.
- Session encryption requires both the server master key and the random
  per-authorization key carried by the OAuth token.
- Firestore uses `sessions-v3`, `revoked-v3` and `oauth-v3-grants` collections.
  Session/tombstone IDs are `<userID>.<sid>.bin`; grant IDs are the family ID.
  Sessions contain `data` (ciphertext bytes) and `updated_at` (timestamp).
  Tombstones contain `updated_at`; grants contain `data` (the grant JSON bytes)
  and `expires_at` (timestamp). No client token or per-session key is persisted.
- Grant writes compare the document version within a Firestore transaction.
  Expiry remains checked by the application; a delayed TTL deletion must never
  be treated as an authorization check. The existing sweeper handles cleanup.
- The run command keeps the Firestore client open during the bounded HTTP drain
  and releases it after the HTTP/auth/user-pool shutdown sequence. Startup
  failures also release the client.

## Storage backend changes and rollback

The GCS backend is removed. Keep the issuer and master-key secret versions
unchanged when updating an existing Firestore deployment.

After Firestore accepts writes, an old GCS snapshot is stale: traffic-only
rollback can revive revoked grants or lose new logins. Stop the service and
reconcile current state before using an older backend; never point an old
revision at a stale bucket blindly.

## Firestore contract tests

Run `task test-firestore` or
`go test -tags=firestoreintegration -race -count=1 -p=1 ./...`.
Tagged `TestMain` functions use Testcontainers to own one Docker emulator per test
binary, or reuse an explicit `FIRESTORE_EMULATOR_HOST` without stopping it. Docker
is required for automatic startup; gcloud and Java run inside the pinned
multi-platform image, not on the host. `-p=1` keeps the JVMs sequential. Ordinary
`go test ./...` does not start containers or require Docker.

CI and `task check` run the tagged Go suite uncached with `-count=1` so results
come from the current emulator run. Tests never use real Firestore when the
emulator variable is absent.

## Older session-format breaking upgrade

Pre-v3 formats intentionally have no migration path. Before deployment, record a
rollback image digest and preserve the old storage long enough to roll back.
After committing to the new release:

1. Remove old objects under `sessions/`, `revoked/`, and `oauth-v2/` only after
   the rollback window closes.
2. Every user reconnects the MCP client and completes QR login (and 2FA when
   enabled).
3. Verify code exchange, refresh rotation, revoke, two parallel clients, MCP
   reconnect, and offline logout against a real Telegram account.

Rollback means deploying the recorded previous image with its previous env and
storage prefixes. Do not point an older binary at the v3 prefixes.

## Release gate

Do not promote unless Linux/macOS tests, race tests, vet, lint, `govulncheck`,
module verification, Windows cross-build, container build/scan, and the manual
Telegram smoke all pass. Also verify that the v3 collections contain only current
documents and that rollback instructions/image digest are recorded.
