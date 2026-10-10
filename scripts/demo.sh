#!/usr/bin/env bash
# The crash-recovery demo, in a terminal. It starts a small stack of its own (its own database, ports 8090, 9988 and
# 9989), starts an agent run that calls a side-effecting tool four times, kills the worker with SIGKILL while a tool
# call is in flight, starts a new worker, and shows that the run finished and the effect was applied once per step.
#
#   make up            # Postgres and Redis, once
#   scripts/demo.sh    # about 40 seconds
#   scripts/demo.sh stop
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && set -a && . ./.env && set +a
PGU=${POSTGRES_USER:-spillway}; PGP=${POSTGRES_PASSWORD:-spillway}; PGPORT=${POSTGRES_PORT:-5433}; RPORT=${REDIS_PORT:-6380}
RUN=/tmp/spillway-demo; mkdir -p "$RUN"

stop() {
  for f in "$RUN"/*.pid; do [ -f "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null || true; rm -f "$f"; done
}
if [ "${1:-}" = stop ]; then stop; echo "stopped"; exit 0; fi
stop
b() { printf '\n\033[1m%s\033[0m\n' "$*"; }

b "Building"
go build -o bin/spillway ./cmd/spillway
go build -o bin/fakeprovider ./cmd/fakeprovider
go build -o bin/fakereceiver ./cmd/fakereceiver

b "Preparing a database for the demo"
docker compose exec -T postgres psql -U "$PGU" -d postgres -tAc "DROP DATABASE IF EXISTS spillway_demo" >/dev/null
docker compose exec -T postgres createdb -U "$PGU" spillway_demo
export DATABASE_URL="postgres://$PGU:$PGP@localhost:$PGPORT/spillway_demo?sslmode=disable"
export REDIS_URL="redis://localhost:$RPORT/2"
export SPILLWAY_CONFIG=config/demo.yaml OPENAI_API_KEY=demo LOG_LEVEL=warn
export SPILLWAY_WEBHOOK_SECRET=demo-webhook-secret
export ADMIN_SESSION_SECRET=demo-session-secret-demo-session-secret
export SEED_ADMIN_USER=admin SEED_ADMIN_PASSWORD=demo-admin SEED_VIEWER_USER=viewer SEED_VIEWER_PASSWORD=demo-viewer
export SPILLWAY_SECRET_KEY=$(openssl rand -base64 32)
bin/spillway migrate >/dev/null
bin/spillway seed >/dev/null
KEY=$(bin/spillway keys create --name demo | awk '/key:/{print $2}')

bg() { name=$1; shift; "$@" >"$RUN/$name.log" 2>&1 & echo $! >"$RUN/$name.pid"; }
bg provider bin/fakeprovider -addr :9989 -delay 700ms -tool effect -tool-calls 4
bg receiver bin/fakereceiver -addr :9988 -delay 3s
bg api bin/spillway serve --role=api --addr=:8090
for i in $(seq 1 40); do curl -sf localhost:8090/healthz >/dev/null && break; sleep 0.25; done
bg worker bin/spillway serve --role=worker --workers 2
bin/spillway tools add effect --endpoint http://127.0.0.1:9988/effect --description "Applies a side effect" >/dev/null

export SPILLWAY_URL=http://localhost:8090 SPILLWAY_API_KEY=$KEY
b "1. Start a run: the agent calls the tool four times"
ID=$(bin/spillway runs create --tools effect "Apply the four changes in order" | awk '/^id:/{print $2}')
echo "run $ID"

b "2. Wait for the third tool call to be in flight, then kill -9 the worker"
for i in $(seq 1 120); do
  n=$(bin/spillway runs steps "$ID" | awk '$3=="tool_call" && $4=="started"' | wc -l | tr -d ' ')
  f=$(bin/spillway runs steps "$ID" | awk '$3=="tool_call" && $4=="finished"' | wc -l | tr -d ' ')
  [ "$n" -ge 3 ] && [ "$f" -lt "$n" ] && break
  sleep 0.25
done
WPID=$(cat "$RUN/worker.pid"); rm -f "$RUN/worker.pid"
{ kill -9 "$WPID"; wait "$WPID"; } 2>/dev/null || true
echo "worker killed. The run is still 'running': nobody holds it, and its lease will run out."

b "3. Start a new worker. It takes over when the lease expires and re-issues the open step"
bg worker2 bin/spillway serve --role=worker --workers 2
for i in $(seq 1 80); do
  s=$(bin/spillway runs get "$ID" | awk '/^status:/{print $2}')
  case "$s" in succeeded|failed|cancelled) break;; esac; sleep 0.5
done
bin/spillway runs get "$ID" | sed -n 1,5p

b "4. The log: one row per step, and a 'reissued' row where the new worker took over"
bin/spillway runs steps "$ID" | awk 'NR==1 || $2!="-" || $3=="run_status"'

b "5. What the tool endpoint saw"
curl -s localhost:9988/ledger; echo
echo
echo "Each step's idempotency key reached the endpoint; the one that was in flight when the worker died arrived twice and was applied once."
echo "That last part is the receiver honouring the key: Spillway sends the same key, it cannot make a receiver deduplicate."
echo
echo "Stack still running. Open the run in the dashboard:"
echo "  cd web && SPILLWAY_ADMIN_URL=http://localhost:8090 npm run dev   # then /runs/$ID, sign in admin / demo-admin"
echo "Stop it with: scripts/demo.sh stop"
