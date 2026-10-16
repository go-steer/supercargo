#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Deploys the demo to Cloud Run (#38): Ocean Blue (ocean carrier), Cymbal
# Shoes (shipper), Meridian Forwarding (forwarder, with Polder Haulage, a
# road carrier without a node, as its EDI sidecar) and Serra Transportes
# (road carrier), each its own service at its own run.app address, which
# is its ULP domain. docs/guides/runbook-cloudrun.md walks through it,
# including what to do by hand first (the OAuth client).
#
#   PROJECT=my-project GOOGLE_CLIENT_ID=123-abc.apps.googleusercontent.com \
#   MEMBERS=you@example.com deploy/cloudrun/deploy.sh
#
# Settings (environment):
#   PROJECT           the Google Cloud project (required)
#   GOOGLE_CLIENT_ID  the OAuth client the UIs and Cymbal's MCP sign in with (required)
#   MEMBERS           who may sign in, comma-separated: emails, or @domain for
#                     every account of that Google Workspace domain (required)
#   REGION            Cloud Run region (default us-central1)
#   LLM_LOCATION      Vertex AI location for the models (default global)
#   LLM_MODEL         Gemini model ID (default gemini-3.8-flash)
#   HOURS_PER_SECOND  simulated hours per second (default 1)
#   TAG               image tag (default: the current commit)
#   RENDER_ONLY=DIR   only render and check the configs into DIR (no gcloud
#                     needed; with PROJECT_NUMBER set)
#
# The scenarios' dates (Cymbal's deadlines) are set from today: re-run it
# before a demo. Re-running is safe: it creates what's missing, adds a
# secret version only when a file changed, never replaces a signing key,
# and replaces each service (a minute of downtime each: see replace).
set -euo pipefail

: "${PROJECT:?set PROJECT}" "${GOOGLE_CLIENT_ID:?set GOOGLE_CLIENT_ID}" "${MEMBERS:?set MEMBERS (emails or @domain, comma-separated)}"
REGION="${REGION:-us-central1}"
LLM_LOCATION="${LLM_LOCATION:-global}"
LLM_MODEL="${LLM_MODEL:-gemini-3.8-flash}"
HPS="${HOURS_PER_SECOND:-1}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ -z "${TAG:-}" ]]; then
  TAG="$(git -C "$root" rev-parse --short HEAD)"
  git -C "$root" diff --quiet HEAD || TAG+="-dirty"
fi

[[ $PROJECT =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || { echo "PROJECT $PROJECT is not a project ID" >&2; exit 1; }
[[ $REGION =~ ^[a-z]+-[a-z]+[0-9]$ ]] || { echo "REGION $REGION is not a region" >&2; exit 1; }
[[ $GOOGLE_CLIENT_ID =~ ^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$ ]] || { echo "GOOGLE_CLIENT_ID is not an OAuth client ID" >&2; exit 1; }
[[ $LLM_MODEL =~ ^[a-z0-9.-]+$ && $LLM_LOCATION =~ ^[a-z0-9-]+$ && $HPS =~ ^[0-9]+$ ]] || { echo "LLM_MODEL, LLM_LOCATION or HOURS_PER_SECOND is malformed" >&2; exit 1; }
members=""
[[ $MEMBERS != *$'\n'* ]] || { echo "MEMBERS: one line, comma-separated" >&2; exit 1; }
IFS=',' read -ra list <<<"$MEMBERS"
for m in "${list[@]}"; do
  m="$(echo "$m" | tr -d ' ' | tr '[:upper:]' '[:lower:]')"
  [[ $m =~ ^[a-z0-9._%+-]*@[a-z0-9.-]+\.[a-z]+$ ]] || { echo "MEMBERS: $m is not an email or @domain" >&2; exit 1; }
  members+="${members:+, }\"$m\""
done

gc() { gcloud --project "$PROJECT" --quiet "$@"; }
# retry CMD...: CMD, again every 10 s for up to 2 minutes (IAM is eventually
# consistent: a new service account isn't usable at once).
retry() {
  local i
  for i in $(seq 12); do
    "$@" >/dev/null 2>"$work/retry.err" && return 0
    sleep 10
  done
  cat "$work/retry.err" >&2
  return 1
}
step() { printf '\n\033[1m▸ %s\033[0m\n' "$*"; }

number="${PROJECT_NUMBER:-}"
[[ -n $number ]] || number="$(gc projects describe "$PROJECT" --format='value(projectNumber)')"
[[ $number =~ ^[0-9]+$ ]] || { echo "project number $number is not a number" >&2; exit 1; }
OB="ocean-blue-$number.$REGION.run.app"
CY="cymbal-shoes-$number.$REGION.run.app"
MF="meridian-forwarding-$number.$REGION.run.app"
SR="serra-transportes-$number.$REGION.run.app"
image="$REGION-docker.pkg.dev/$PROJECT/supercargo/ulpnode:$TAG"
# Cymbal's deadlines, from today (UTC): Newark's spring stock (the ship takes
# about 6 days; 72 hours late misses it) and the Brazil launch (door to
# door takes about 21 days; 72 hours late misses it).
newark="$(date -u -d '+10 days' +%F)"
launch="$(date -u -d '+23 days' +%F)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
out="${RENDER_ONLY:-$work/config}"
mkdir -p "$out" "$work/image"

step "Binaries"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$work/image/ulpnode" ./cmd/ulpnode)
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$work/image/ulpmock" ./cmd/ulpmock)
(cd "$root" && go build -o "$work/ulpnode-local" ./cmd/ulpnode)

step "Configs (Ocean Blue at $OB, Cymbal Shoes at $CY, Meridian at $MF, Serra at $SR)"
llm="{ provider: gemini, backend: vertex, model: $LLM_MODEL }   # Vertex AI, as the service account"
# node FILE DOMAIN NAME KID PARTNERS PERSONA BODY: a node's config.
node() {
  cat >"$out/$1" <<EOF
domain: $2
name: $3
listen: ":8080"
tls: { mode: proxy }   # Cloud Run serves https at the domain
keys: [{ kid: $4-1, privateKeyEnv: ULP_SIGNING_KEY }]
partners: [$5]
llm: $llm
google: { clientID: $GOOGLE_CLIENT_ID, clientSecretEnv: GOOGLE_CLIENT_SECRET, publicURL: "https://$2" }
web:
  auth: google
  personas:
    - $6
persistence: { file: /var/lib/ulp/node.db }   # kept in the service's bucket by Litestream
$7
EOF
}
node ob.yaml "$OB" "Ocean Blue" ob "$CY, $MF" "{ id: sam, name: Sam, title: \"Ops desk\", roles: [operator], members: [$members] }" "role: operator
operator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: Ocean Blue Aurora, voyage: 42W, departureHour: 18, transit: 134h, transits: { BRSSZ: 380h } }
simulator: { hoursPerSecond: $HPS, clock: shared }
skills: { dir: /etc/ulp/skills/ocean-blue }"
node cy.yaml "$CY" "Cymbal Shoes" cy "$OB, $MF" "{ id: alex, name: Alex, title: \"Logistics\", roles: [shipper], members: [$members] }" "role: shipper
business: { file: /etc/ulp/business/business.yaml }   # private: never in an envelope
addressBook: { file: /etc/ulp/address-book/address-book.yaml }
directory: { file: /etc/ulp/directory/directory.yaml }
mcp: { server: { auth: google } }   # Gemini Enterprise, as the signed-in person"
node mf.yaml "$MF" "Meridian Forwarding" mf "$CY, $OB, $SR" "{ id: mia, name: Mia, title: \"Forwarding desk\", roles: [operator, shipper], members: [$members] }" "roles: [operator, shipper]
forwarder:
  refPrefix: MF
  ediId: meridian-forwarding          # its EDI identity: the run.app domain is too long for one
  edi:                                # Polder Haulage has no node: EDIFACT files, with the sidecar
    - { carrier: polder-haulage.example, id: polder-haulage, outbox: /edi/to-polder, inbox: /edi/from-polder, zone: Europe/Amsterdam, transit: 3h }
  lanes:                              # the routing guide
    - from: NLTLB
      to: BRSAO
      legs:
        - { mode: road, carrier: polder-haulage.example, name: Polder Haulage, to: NLRTM, facility: Euromax terminal, edi: true }
        - { mode: ocean, carrier: $OB, name: Ocean Blue, to: BRSSZ, facility: Santos Brasil Tecon terminal }
        - { mode: road, carrier: $SR, name: Serra Transportes, to: BRSAO }"
node sr.yaml "$SR" "Serra Transportes" sr "$MF" "{ id: rita, name: Rita, title: \"Dispatch\", roles: [operator], members: [$members] }" "role: operator
operator: { refPrefix: SR-BKG, road: { transit: 30h, cte: { cnpj: \"12345678000190\", state: 35 } } }
simulator: { hoursPerSecond: $HPS, clock: shared }"
cat >"$out/business.yaml" <<EOF
shipments:
  - shipperRef: CY-PO-55120
    requiredBy: $newark
    receivingSlot: { date: $newark, window: "08:00-12:00", warehouse: "Cymbal Newark DC, dock 4" }
    notes: "Spring stock for the Newark stores"
  - shipperRef: CY-PO-55210
    requiredBy: $launch
    receivingSlot: { date: $launch, window: "08:00-12:00", warehouse: "Cajamar DC, Brazil launch stock" }
    notes: "Brazil launch: the São Paulo stores open with this stock"
notes:
  - "Tell the launch team about any delay that puts a launch at risk"
  - "1,200 pairs of the launch line are already in the São Paulo store room: enough for a partial launch"
EOF
cat >"$out/address-book.yaml" <<EOF
entries:
  - domain: cymbal-footwear.example
    name: Cymbal Footwear
    roles: [consignee]
    description: Our US retail arm; receives at the Newark DC.
  - domain: $MF
    name: Meridian Forwarding
    roles: [forwarder]
    description: Our forwarder, door to door to Brazil.
places:
  - id: tilburg-warehouse
    name: Tilburg warehouse
    party: { name: Cymbal Shoes B.V., domain: $CY, identifiers: [{ scheme: EORI, value: NL123456789 }] }
    place: { type: door, un_location_code: NLTLB, address: { street: Ringbaan Oost 100, postal_code: "5013 CD", locality: Tilburg, country: NL } }
  - id: cajamar-dc
    name: Cajamar DC
    party: { name: Cymbal Calçados Ltda, identifiers: [{ scheme: "BR:CNPJ", value: 12.345.678/0001-90 }] }
    place: { type: door, facility: "Cymbal Calçados DC, dock 2", un_location_code: BRSAO, address: { street: "Rodovia Anhanguera, km 33", locality: Cajamar, region: SP, country: BR } }
EOF
cat >"$out/directory.yaml" <<EOF
entries:
  - domain: $OB
    name: Ocean Blue
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: USEWR, to: NLRTM }, { from: NLRTM, to: BRSSZ }]
    description: Container services from Rotterdam to Newark and to Santos.
  - domain: north-star.example
    name: North Star Lines
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: DEHAM, to: USNYC }]
    description: North Atlantic container lines.
EOF
for f in ob mf sr; do "$work/ulpnode-local" check --config "$out/$f.yaml"; done
"$work/ulpnode-local" check --config "$out/cy.yaml" --business "$out/business.yaml" --address-book "$out/address-book.yaml" --directory "$out/directory.yaml"
if [[ -n "${RENDER_ONLY:-}" ]]; then
  echo "Rendered into $out; nothing deployed."
  exit 0
fi

services="ocean-blue cymbal-shoes meridian-forwarding serra-transportes"

step "APIs"
gc services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com aiplatform.googleapis.com storage.googleapis.com

step "Artifact Registry repository"
gc artifacts repositories describe supercargo --location "$REGION" >/dev/null 2>&1 ||
  gc artifacts repositories create supercargo --location "$REGION" --repository-format docker

step "Service accounts (one per company: Vertex AI, and its own secrets)"
for svc in $services; do
  sa="ulp-$svc@$PROJECT.iam.gserviceaccount.com"
  gc iam service-accounts describe "$sa" >/dev/null 2>&1 ||
    gc iam service-accounts create "ulp-$svc" --display-name "supercargo $svc node"
  # A new account takes a while to be usable in IAM bindings.
  retry gc projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$sa" --role roles/aiplatform.user --condition None
done

# bucket SVC: the bucket SVC's node keeps its records in (Litestream).
bucket() { echo "$PROJECT-ulp-$1-state"; }

step "State buckets (one per company: each node's records, copied by Litestream)"
for svc in $services; do
  b="$(bucket "$svc")"
  gc storage buckets describe "gs://$b" >/dev/null 2>&1 ||
    gc storage buckets create "gs://$b" --location "$REGION" --uniform-bucket-level-access --public-access-prevention
  # Its company's account is the only service account granted on it (the
  # project's owners, and Cloud Build's account with its project-wide
  # read, can read it too).
  retry gc storage buckets add-iam-policy-binding "gs://$b" --member "serviceAccount:ulp-$svc@$PROJECT.iam.gserviceaccount.com" \
    --role roles/storage.objectAdmin
done

step "Cloud Build's account (pushes the image, writes its logs)"
build_sa="$number-compute@developer.gserviceaccount.com"
for role in roles/artifactregistry.writer roles/logging.logWriter roles/storage.objectViewer; do
  retry gc projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$build_sa" --role "$role" --condition None
done

# grant NAME SVC...: the services' accounts may read secret NAME.
grant() {
  local name=$1 svc
  shift
  for svc in "$@"; do
    retry gc secrets add-iam-policy-binding "$name" --member "serviceAccount:ulp-$svc@$PROJECT.iam.gserviceaccount.com" \
      --role roles/secretmanager.secretAccessor
  done
}

# secret NAME FILE SVC...: the secret's latest version holds FILE (a new
# version only if it changed), readable by the services' accounts.
secret() {
  local name=$1 file=$2
  shift 2
  if ! gc secrets describe "$name" >/dev/null 2>&1; then
    gc secrets create "$name" --replication-policy automatic --data-file "$file" >/dev/null
  else
    rm -f "$work/current"
    gc secrets versions access latest --secret "$name" --out-file "$work/current"
    cmp -s "$file" "$work/current" || gc secrets versions add "$name" --data-file "$file" >/dev/null
    rm -f "$work/current"
  fi
  grant "$name" "$@"
}

step "Signing keys (made once, here, straight into Secret Manager)"
for k in ob:ocean-blue cy:cymbal-shoes mf:meridian-forwarding sr:serra-transportes; do
  short=${k%%:*} svc=${k#*:}
  if ! gc secrets describe "ulp-$short-signing-key" >/dev/null 2>&1; then
    "$work/ulpnode-local" keygen --kid "$short-1" --out "$work/$short.pem" >/dev/null
    gc secrets create "ulp-$short-signing-key" --replication-policy automatic --data-file "$work/$short.pem" >/dev/null
    rm -f "$work/$short.pem"
  fi
  grant "ulp-$short-signing-key" "$svc"
done

step "The OAuth client's secret"
if ! gc secrets describe ulp-google-client-secret >/dev/null 2>&1; then
  [[ -t 0 ]] || { echo "the OAuth client's secret isn't in Secret Manager yet: run this interactively once to paste it" >&2; exit 1; }
  client_secret=""
  read -rsp "Paste the OAuth client's secret (not echoed): " client_secret || true
  echo
  [[ -n $client_secret ]] || { echo "no secret given" >&2; exit 1; }
  (umask 077 && printf '%s' "$client_secret" >"$work/client-secret")
  unset client_secret
  gc secrets create ulp-google-client-secret --replication-policy automatic --data-file "$work/client-secret" >/dev/null
  rm -f "$work/client-secret"
fi
# shellcheck disable=SC2086 # the service names, split
grant ulp-google-client-secret $services

step "Configs into Secret Manager"
secret ulp-ob-config "$out/ob.yaml" ocean-blue
secret ulp-cy-config "$out/cy.yaml" cymbal-shoes
secret ulp-mf-config "$out/mf.yaml" meridian-forwarding
secret ulp-sr-config "$out/sr.yaml" serra-transportes
secret ulp-cy-business "$out/business.yaml" cymbal-shoes
secret ulp-cy-address-book "$out/address-book.yaml" cymbal-shoes
secret ulp-cy-directory "$out/directory.yaml" cymbal-shoes

step "Image $image"
mkdir -p "$work/image/skills"
cp -R "$root/dev/demo/skills/." "$work/image/skills/"
cat >"$work/image/Dockerfile" <<'EOF'
# Litestream (a static binary) copies each node's SQLite file to its bucket.
FROM litestream/litestream:0.5.17-scratch@sha256:f757c70d070ac278d45b8847d31a54ab2de24de5e77b09018c642eca263e3967 AS litestream
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY ulpnode /usr/local/bin/ulpnode
COPY ulpmock /usr/local/bin/ulpmock
COPY skills /etc/ulp/skills
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ulpnode"]
EOF
gc builds submit "$work/image" --tag "$image" --region "$REGION"

# Every service: one instance, always on (the simulators run between
# requests, and sessions are in memory), reachable without IAM: people
# sign in with Google, partners send signed envelopes.
service_flags=(--region "$REGION" --execution-environment gen2 --no-invoker-iam-check --min-instances 1 --max-instances 1 --no-cpu-throttling
  --timeout 3600 --concurrency 80)
# Each node runs under Litestream: it restores the node's file from the
# bucket if the instance has none (a new revision, a restarted instance),
# starts the node, copies every change to the bucket while it runs (about
# a second behind), and passes the node its stop signal. The file lives on
# an in-memory volume: the instance's own disk.
state_volume=(--add-volume name=state,type=in-memory,size-limit=128Mi)
state_mount=(--add-volume-mount volume=state,mount-path=/var/lib/ulp)
# node_args SVC: the node's container arguments.
node_args() {
  echo "replicate,-restore-if-db-not-exists,-exec,/usr/local/bin/ulpnode serve --config /etc/ulp/config/node.yaml --log-format json,/var/lib/ulp/node.db,gs://$(bucket "$1")/node"
}
env="GOOGLE_CLOUD_PROJECT=$PROJECT,GOOGLE_CLOUD_LOCATION=$LLM_LOCATION"
common="ULP_SIGNING_KEY=ulp-SHORT-signing-key:latest,GOOGLE_CLIENT_SECRET=ulp-google-client-secret:latest,/etc/ulp/config/node.yaml=ulp-SHORT-config:latest"

# replace SVC: delete SVC if it runs, and give its instance time for its
# last copy. A new revision would otherwise run beside the old one for a
# while, both copying to the bucket: Litestream has no lease on Cloud
# Storage, and two writers can leave a copy no restore can use. The
# service's address is fixed (SVC-NUMBER.REGION.run.app), so deleting it
# costs a minute of downtime and nothing else.
replace() {
  if gc run services describe "$1" --region "$REGION" >/dev/null 2>&1; then
    gc run services delete "$1" --region "$REGION"
    sleep 20 # SIGTERM, the node's shutdown, Litestream's last copy
  fi
}

# deploy SVC SHORT [EXTRA_SECRETS]: one node, one container.
deploy() {
  local svc=$1 secrets="${common//SHORT/$2}${3:+,$3}"
  step "Cloud Run service $svc"
  replace "$svc"
  gc run deploy "$svc" "${service_flags[@]}" --service-account "ulp-$svc@$PROJECT.iam.gserviceaccount.com" \
    --image "$image" --cpu 1 --memory 512Mi --port 8080 "${state_volume[@]}" "${state_mount[@]}" \
    --set-env-vars "$env" --set-secrets "$secrets" --command /usr/local/bin/litestream --args "$(node_args "$svc")"
}
deploy ocean-blue ob
deploy cymbal-shoes cy "/etc/ulp/business/business.yaml=ulp-cy-business:latest,/etc/ulp/address-book/address-book.yaml=ulp-cy-address-book:latest,/etc/ulp/directory/directory.yaml=ulp-cy-directory:latest"
deploy serra-transportes sr

# Meridian, with Polder Haulage beside it: Polder has no node, only the
# EDIFACT file drops the two share, each an in-memory volume. Both run as
# the image's nonroot user, as the drops' ownership checks require.
step "Cloud Run service meridian-forwarding (with Polder Haulage's EDI sidecar)"
replace meridian-forwarding
gc run deploy meridian-forwarding "${service_flags[@]}" --service-account "ulp-meridian-forwarding@$PROJECT.iam.gserviceaccount.com" \
  --add-volume name=to-polder,type=in-memory,size-limit=32Mi --add-volume name=from-polder,type=in-memory,size-limit=32Mi "${state_volume[@]}" \
  --container node --image "$image" --cpu 1 --memory 512Mi --port 8080 \
  --set-env-vars "$env" --set-secrets "${common//SHORT/mf}" --command /usr/local/bin/litestream --args "$(node_args meridian-forwarding)" \
  --add-volume-mount volume=to-polder,mount-path=/edi/to-polder --add-volume-mount volume=from-polder,mount-path=/edi/from-polder "${state_mount[@]}" \
  --container polder --image "$image" --cpu 1 --memory 256Mi --command /usr/local/bin/ulpmock \
  --args "edifact,--id,polder-haulage,--partner,meridian-forwarding,--in,/edi/to-polder,--out,/edi/from-polder,--ecmr-prefix,ECMR-NL-2026,--zone,Europe/Amsterdam,--hours-per-second,$HPS,--clock,shared" \
  --add-volume-mount volume=to-polder,mount-path=/edi/to-polder --add-volume-mount volume=from-polder,mount-path=/edi/from-polder

cat <<EOF

Done.
  Ocean Blue           https://$OB/ui/
  Cymbal Shoes         https://$CY/ui/
  Meridian Forwarding  https://$MF/ui/
  Serra Transportes    https://$SR/ui/
  Cymbal's MCP         https://$CY/mcp   (Gemini Enterprise: docs/guides/runbook-cloudrun.md, step 6)

The OAuth client needs these authorized redirect URIs (runbook, step 2):
  https://$OB/ui/auth/callback
  https://$CY/ui/auth/callback
  https://$MF/ui/auth/callback
  https://$SR/ui/auth/callback
  https://vertexaisearch.cloud.google.com/oauth-redirect
EOF
