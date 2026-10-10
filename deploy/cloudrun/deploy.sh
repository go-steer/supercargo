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

# Deploys the two-company demo to Cloud Run (#38): Ocean Blue (carrier)
# and Cymbal Shoes (shipper), each its own service at its own run.app
# address, which is its ULP domain. docs/runbook-cloudrun.md walks through
# it, including what to do by hand first (the OAuth client).
#
#   PROJECT=my-project GOOGLE_CLIENT_ID=123-abc.apps.googleusercontent.com \
#   MEMBERS=you@example.com deploy/cloudrun/deploy.sh
#
# Settings (environment):
#   PROJECT           the Google Cloud project (required)
#   GOOGLE_CLIENT_ID  the OAuth client both UIs and Cymbal's MCP sign in with (required)
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
# Re-running is safe: it creates what's missing, adds a secret version
# only when a file changed, never replaces a signing key, and deploys a
# new revision of each service.
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
image="$REGION-docker.pkg.dev/$PROJECT/supercargo/ulpnode:$TAG"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
out="${RENDER_ONLY:-$work/config}"
mkdir -p "$out" "$work/image"

step "Binary"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$work/image/ulpnode" ./cmd/ulpnode)
(cd "$root" && go build -o "$work/ulpnode-local" ./cmd/ulpnode)

step "Configs (Ocean Blue at $OB, Cymbal Shoes at $CY)"
llm="{ provider: gemini, backend: vertex, model: $LLM_MODEL }   # Vertex AI, as the service account"
cat >"$out/ob.yaml" <<EOF
domain: $OB
name: Ocean Blue
role: operator
listen: ":8080"
tls: { mode: proxy }   # Cloud Run serves https at the domain
keys: [{ kid: ob-1, privateKeyEnv: ULP_SIGNING_KEY }]
partners: [$CY]
operator: { refPrefix: OB-BKG, containerPrefix: OCBU, vessel: Ocean Blue Aurora, voyage: 42W, departureHour: 18, transit: 134h }
simulator: { hoursPerSecond: $HPS }
llm: $llm
skills: { dir: /etc/ulp/skills/ocean-blue }
google: { clientID: $GOOGLE_CLIENT_ID, clientSecretEnv: GOOGLE_CLIENT_SECRET, publicURL: "https://$OB" }
web:
  auth: google
  personas:
    - { id: sam, name: Sam, title: "Ops desk", roles: [operator], members: [$members] }
EOF
cat >"$out/cy.yaml" <<EOF
domain: $CY
name: Cymbal Shoes
role: shipper
listen: ":8080"
tls: { mode: proxy }
keys: [{ kid: cy-1, privateKeyEnv: ULP_SIGNING_KEY }]
partners: [$OB]
business: { file: /etc/ulp/business/business.yaml }   # private: never in an envelope
addressBook: { file: /etc/ulp/address-book/address-book.yaml }
directory: { file: /etc/ulp/directory/directory.yaml }
mcp: { server: { auth: google } }   # Gemini Enterprise, as the signed-in person
llm: $llm
google: { clientID: $GOOGLE_CLIENT_ID, clientSecretEnv: GOOGLE_CLIENT_SECRET, publicURL: "https://$CY" }
web:
  auth: google
  personas:
    - { id: alex, name: Alex, title: "Logistics", roles: [shipper], members: [$members] }
EOF
cat >"$out/business.yaml" <<'EOF'
shipments:
  - shipperRef: CY-PO-55120
    requiredBy: 2026-10-15
    receivingSlot: { date: 2026-10-15, window: "08:00-12:00", warehouse: "Cymbal Newark DC, dock 4" }
    notes: "Spring launch stock: shelves must be full by Friday the 16th"
notes:
  - "Tell the launch team about any delay that puts the receiving slot at risk"
EOF
cat >"$out/address-book.yaml" <<'EOF'
entries:
  - domain: cymbal-footwear.example
    name: Cymbal Footwear
    roles: [consignee]
    description: Our US retail arm; receives at the Newark DC.
EOF
cat >"$out/directory.yaml" <<EOF
entries:
  - domain: $OB
    name: Ocean Blue
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: USEWR, to: NLRTM }]
    description: Transatlantic container service, weekly departures from Rotterdam.
  - domain: north-star.example
    name: North Star Lines
    roles: [carrier]
    modes: [OCEAN]
    lanes: [{ from: NLRTM, to: USEWR }, { from: DEHAM, to: USNYC }]
    description: North Atlantic container lines.
EOF
"$work/ulpnode-local" check --config "$out/ob.yaml"
"$work/ulpnode-local" check --config "$out/cy.yaml"
if [[ -n "${RENDER_ONLY:-}" ]]; then
  echo "Rendered into $out; nothing deployed."
  exit 0
fi

step "APIs"
gc services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com aiplatform.googleapis.com

step "Artifact Registry repository"
gc artifacts repositories describe supercargo --location "$REGION" >/dev/null 2>&1 ||
  gc artifacts repositories create supercargo --location "$REGION" --repository-format docker

step "Service accounts (one per company: Vertex AI, and its own secrets)"
for svc in ocean-blue cymbal-shoes; do
  sa="ulp-$svc@$PROJECT.iam.gserviceaccount.com"
  gc iam service-accounts describe "$sa" >/dev/null 2>&1 ||
    gc iam service-accounts create "ulp-$svc" --display-name "supercargo $svc node"
  # A new account takes a while to be usable in IAM bindings.
  retry gc projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$sa" --role roles/aiplatform.user --condition None
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
for k in ob:ocean-blue cy:cymbal-shoes; do
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
grant ulp-google-client-secret ocean-blue cymbal-shoes

step "Configs into Secret Manager"
secret ulp-ob-config "$out/ob.yaml" ocean-blue
secret ulp-cy-config "$out/cy.yaml" cymbal-shoes
secret ulp-cy-business "$out/business.yaml" cymbal-shoes
secret ulp-cy-address-book "$out/address-book.yaml" cymbal-shoes
secret ulp-cy-directory "$out/directory.yaml" cymbal-shoes

step "Image $image"
mkdir -p "$work/image/skills"
cp -R "$root/dev/demo/skills/." "$work/image/skills/"
cat >"$work/image/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
COPY ulpnode /usr/local/bin/ulpnode
COPY skills /etc/ulp/skills
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ulpnode"]
EOF
gc builds submit "$work/image" --tag "$image" --region "$REGION"

# deploy SVC SECRETS: one instance, always on (the simulator runs between
# requests, and the stores and sessions are in memory), reachable without
# IAM: people sign in with Google, partners send signed envelopes.
deploy() {
  local svc=$1 secrets=$2
  step "Cloud Run service $svc"
  gc run deploy "$svc" --image "$image" --region "$REGION" \
    --service-account "ulp-$svc@$PROJECT.iam.gserviceaccount.com" \
    --no-invoker-iam-check --min-instances 1 --max-instances 1 --no-cpu-throttling \
    --cpu 1 --memory 512Mi --timeout 3600 --port 8080 --concurrency 80 \
    --set-env-vars "GOOGLE_CLOUD_PROJECT=$PROJECT,GOOGLE_CLOUD_LOCATION=$LLM_LOCATION" \
    --set-secrets "$secrets" \
    --args serve,--config,/etc/ulp/config/node.yaml,--log-format,json
}
common="ULP_SIGNING_KEY=ulp-SHORT-signing-key:latest,GOOGLE_CLIENT_SECRET=ulp-google-client-secret:latest,/etc/ulp/config/node.yaml=ulp-SHORT-config:latest"
deploy ocean-blue "${common//SHORT/ob}"
deploy cymbal-shoes "${common//SHORT/cy},/etc/ulp/business/business.yaml=ulp-cy-business:latest,/etc/ulp/address-book/address-book.yaml=ulp-cy-address-book:latest,/etc/ulp/directory/directory.yaml=ulp-cy-directory:latest"

cat <<EOF

Done.
  Ocean Blue    https://$OB/ui/
  Cymbal Shoes  https://$CY/ui/
  Cymbal's MCP  https://$CY/mcp   (Gemini Enterprise: docs/runbook-cloudrun.md, step 6)

The OAuth client needs these authorized redirect URIs (runbook, step 2):
  https://$OB/ui/auth/callback
  https://$CY/ui/auth/callback
  https://vertexaisearch.cloud.google.com/oauth-redirect
EOF
