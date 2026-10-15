# Runbook: the demo on Cloud Run, driven from Gemini Enterprise

This takes you from an empty Google Cloud project to two outcomes:

- **The booking:** someone asks Gemini Enterprise to book a container with
  Cymbal Shoes' node. The agent drafts the booking over MCP, a person
  confirms it in Cymbal's browser UI, and Ocean Blue confirms it over ULP
  (#38, #39, #42).
- **Door to door:** Cymbal's assistant orders a container from Tilburg to
  Cajamar from Meridian Forwarding, which books three legs. Polder
  Haulage's truck goes over EDIFACT, Ocean Blue's ship and Serra
  Transportes' truck over ULP. Cymbal follows one journey, and sees which
  facts are signed.

Each company runs on Cloud Run at its own `run.app` address, which is its
ULP domain. Polder Haulage has no node: it runs beside Meridian, as the
EDI sidecar in Meridian's service.

Allow about an hour the first time. Most of it is console clicks in steps
2 and 6.

## What you get

| | |
|---|---|
| Ocean Blue (ocean carrier) | `https://ocean-blue-<project number>.<region>.run.app/ui/` |
| Cymbal Shoes (shipper) | `https://cymbal-shoes-<project number>.<region>.run.app/ui/` |
| Meridian Forwarding (forwarder), with Polder Haulage (EDI) | `https://meridian-forwarding-<project number>.<region>.run.app/ui/` |
| Serra Transportes (road carrier) | `https://serra-transportes-<project number>.<region>.run.app/ui/` |
| Cymbal's MCP endpoint | `https://cymbal-shoes-<project number>.<region>.run.app/mcp` |

- **Sign-in:** people sign in to every UI with Google. Only the emails or
  domains you list in `MEMBERS` get in.
- **MCP calls:** Gemini Enterprise calls Cymbal's tools as the signed-in
  person, with that person's own Google token. Every call is in Cymbal's
  activity log with the caller's email.
- **What an agent can do:** tools read and draft only. Nothing is sent
  until a person confirms it in the UI.
- **Models:** every node uses Gemini on Vertex AI through its own
  service account. There are no API keys.

## 0. What you need

- A Google Cloud project with billing, where you're Owner (or Editor plus
  Security Admin and Secret Manager Admin).
- A Gemini Enterprise subscription in the same organization, and
  permission to change its org policies, or someone who has it (step 5).
- On your machine: `gcloud` signed in (`gcloud auth login`), Go (the
  version in `go.mod`; the toolchain downloads itself), `git`, and a
  checkout of this repository.

```bash
export PROJECT=your-project-id
export REGION=us-central1
gcloud config set project "$PROJECT"
gcloud projects describe "$PROJECT" --format='value(projectNumber)'   # note it: <project number>
```

The nodes' addresses follow from these values, so you can write them
down now:

```
OB=ocean-blue-<project number>.$REGION.run.app
CY=cymbal-shoes-<project number>.$REGION.run.app
MF=meridian-forwarding-<project number>.$REGION.run.app
SR=serra-transportes-<project number>.$REGION.run.app
```

## 1. The OAuth consent screen

1. Open **Google Auth Platform** (Console → APIs & Services → OAuth consent
   screen):
   https://console.cloud.google.com/auth/overview?project=YOUR_PROJECT
2. **Get started**:
   - App name `supercargo demo`, and your email as the support email.
   - Audience **Internal**: only your organization's accounts can sign in.
     Choose External only if you must, and then add test users.
   - Your email as the contact, then **Create**.
3. **Data access → Add or remove scopes:** pick `openid` and
   `.../auth/userinfo.email`, then **Save**. Nothing else is needed: the
   nodes only learn who you are.

## 2. The OAuth client

One client serves every sign-in: the four UIs, and Gemini Enterprise
calling Cymbal's MCP. In a real deployment each company has its own
client.

1. **Clients → Create client**, type **Web application**, name
   `supercargo`.
2. **Authorized redirect URIs:** add all five.
   ```
   https://ocean-blue-<project number>.<region>.run.app/ui/auth/callback
   https://cymbal-shoes-<project number>.<region>.run.app/ui/auth/callback
   https://meridian-forwarding-<project number>.<region>.run.app/ui/auth/callback
   https://serra-transportes-<project number>.<region>.run.app/ui/auth/callback
   https://vertexaisearch.cloud.google.com/oauth-redirect
   ```
3. **Create**, then copy the **client ID** (`…apps.googleusercontent.com`)
   and the **client secret**. The secret goes into Secret Manager in the
   next step. Don't save it anywhere else.

## 3. Deploy the nodes

```bash
export GOOGLE_CLIENT_ID=123-abc.apps.googleusercontent.com
export MEMBERS=you@example.com            # comma-separated; @example.com for every account of that Workspace
deploy/cloudrun/deploy.sh
```

An `@domain` entry admits only accounts that Google says belong to that
Google Workspace domain. A personal Google account that happens to use a
company address doesn't count. For a Workspace secondary domain, or a
personal account, list the exact address.

To see the configs without touching the project, run
`PROJECT_NUMBER=<project number> RENDER_ONLY=/tmp/render deploy/cloudrun/deploy.sh`.
It writes them to `/tmp/render` and checks them, Cymbal's business context
and address book included.

**Dates:** Cymbal's deadlines are set from the day you deploy (in UTC). The
Newark stock is due in 10 days, and the Brazil launch is 23 days out.
Re-run the script on the day of a demo, so a delay misses the launch as
the scene expects.

On its first run the script asks for the client secret (it isn't echoed)
and stores it in Secret Manager. Here is what it does, so you know what's
in your project:

- **APIs:** enables Cloud Run, Cloud Build, Artifact Registry, Secret
  Manager and Vertex AI.
- **Service accounts:** creates one per company: `ulp-ocean-blue@…`,
  `ulp-cymbal-shoes@…`, `ulp-meridian-forwarding@…` and
  `ulp-serra-transportes@…`. Each gets `roles/aiplatform.user`, and can
  read only its own secrets.
  A new account takes up to a couple of minutes to be usable in IAM, so
  the script retries those bindings.
- **Cloud Build:** grants Cloud Build's account
  (`<project number>-compute@developer.gserviceaccount.com`) what it needs
  to push the image and write its logs. New projects no longer give that
  account Editor.
- **Signing keys:** makes each company's Ed25519 signing key locally, puts
  it straight into Secret Manager (`ulp-ob-signing-key`,
  `ulp-cy-signing-key`, `ulp-mf-signing-key`, `ulp-sr-signing-key`), and
  deletes the local copy. A re-run never replaces a key.
- **Configs:** renders each node's config into secrets (`ulp-ob-config`,
  `ulp-cy-config`, `ulp-mf-config`, `ulp-sr-config`). It also stores
  Cymbal's private business context, its address book (with Meridian as
  its forwarder, and its two doors) and its directory (`ulp-cy-*`). These
  are mounted as files, and a re-run adds a new version only when
  something changed.
- **Image:** builds the node and the mock EDI carrier, and uploads a small
  image (the two binaries and Ocean Blue's agent skills) with Cloud
  Build. No source and no secret go into it.
- **Meridian's EDI sidecar:** `meridian-forwarding` runs two containers.
  One is the node; the other, `polder`, is Polder Haulage's mock EDIFACT
  carrier. They share two in-memory volumes, the file drops, and talk only
  through files there, as a forwarder and a trucker without a node would.
  This is a demo arrangement, not a trust boundary: the two containers run
  as the same user, with Meridian's service account.
  Meridian's EDI identity is `meridian-forwarding` (`forwarder.ediId`),
  because an EDI identity is at most 35 characters.
- **Services:** deploys `ocean-blue`, `cymbal-shoes`,
  `meridian-forwarding` and `serra-transportes` with:
  - exactly one instance each, with the CPU always on (the stores are in
    memory, and the voyage simulator runs between requests);
  - `--no-invoker-iam-check`, so anyone can reach them, and nobody needs
    an `allUsers` IAM binding, which many org policies forbid. People
    still sign in with Google, and partners still send signed envelopes.

It prints the addresses when done. Then check that each node is up:

```bash
curl -s "https://$OB/.well-known/ulp" | head -c 300; echo
curl -s "https://$MF/.well-known/ulp" | head -c 300; echo
curl -s "https://$SR/.well-known/ulp" | head -c 300; echo
curl -s "https://$CY/.well-known/oauth-protected-resource/mcp"; echo
```

If `gcloud run deploy` fails on `--no-invoker-iam-check`, update gcloud
(`gcloud components update`).

## 4. Try the UIs

1. Open Cymbal Shoes' UI (`https://$CY/ui/`), choose **Sign in with
   Google**, and pick an account in `MEMBERS`. You land as Alex
   (Logistics).
2. In a **second tab**, open Ocean Blue's UI the same way. You land as
   Sam (Ops desk).

   The presenter page, which frames the UIs side by side, doesn't work
   on `run.app`. The addresses are different sites, so browsers don't
   send the session cookies inside frames, and Google's sign-in page
   won't load in a frame. Use a tab per company.
3. In Cymbal's Assistant tab, ask *"Find a carrier from Rotterdam to
   Newark and draft a booking for one 40' container of sneakers, 12 t,
   ready tomorrow."*
4. Confirm the draft in its dialog. Ocean Blue confirms the booking, and
   the voyage starts on both sides.

A refused sign-in shows **Not allowed** with the email. Add it to
`MEMBERS` and re-run the script.

## 5. Gemini Enterprise: org policy and roles

An organization policy administrator does this once. In the Gemini
Enterprise project:

1. **Override the constraint that blocks Custom MCP data stores**, as the
   Gemini Enterprise docs describe:
   https://docs.cloud.google.com/gemini/enterprise/docs/connectors/custom-mcp-server/set-up-custom-mcp-server
2. If `allowedEgressFqdns` is enforced, add these three hosts (bare
   domains):
   ```
   cymbal-shoes-<project number>.<region>.run.app
   accounts.google.com
   oauth2.googleapis.com
   ```
3. If project enforcement is on, or the project is inside VPC Service
   Controls, add `custom_mcp` to `allowedDataSources`.
4. Grant whoever creates the data store **Discovery Engine Editor**
   (`roles/discoveryengine.editor`):
   ```bash
   gcloud projects add-iam-policy-binding "$PROJECT" --member user:you@example.com --role roles/discoveryengine.editor
   ```

You don't need Cloud Run Invoker for the Discovery Engine service agent:
the services run without the invoker check, and Cymbal's node checks each
caller's own token itself.

## 6. Gemini Enterprise: the Custom MCP Server data store

1. Open https://console.cloud.google.com/gemini-enterprise/, then your
   app → **Data stores** → **Create data store**. Search **Custom MCP
   Server**, then **Add MCP server**.
2. **Authentication settings:** choose **OAuth 2.0** and fill in:

   | Field | Value |
   |---|---|
   | MCP Server URL | `https://cymbal-shoes-<project number>.<region>.run.app/mcp` |
   | Authorization URL | `https://accounts.google.com/o/oauth2/v2/auth` |
   | Authorization URL Parameters | `&access_type=offline&prompt=consent` (so it gets a refresh token) |
   | Token URL | `https://oauth2.googleapis.com/token` |
   | Client ID | the client ID from step 2 |
   | Enable PKCE Support | on |
   | Client Secret | the client secret from step 2 |
   | Scopes | `openid email` |

3. **Verify Auth.** Google asks you to sign in. Use an account in
   `MEMBERS`. Then **Continue**, and **Continue** again.
4. Location **Multi-region (global)**, connector name `Cymbal Shoes`, and
   a description. The assistant uses the description to decide when the
   connector applies, so say what it's for:
   > Cymbal Shoes' shipping node (ULP). Use for anything about Cymbal
   > Shoes' ocean shipments: list our shipments and their status, track
   > events and delay alerts, look up carriers (our partners, or find new
   > ones on a route), ask a carrier about one of our bookings, and draft
   > a new booking. Drafting never sends anything: a person reviews and
   > confirms every booking in Cymbal's browser UI, at the link the tool
   > returns.

   Then **Create**, and wait until the state is **Active**.
5. Open the data store → **Actions** → **Reload custom actions**. That
   is the first request that reaches Cymbal's node, with your token; it
   lists Cymbal's tools: `list_shipments`, `get_events`, `get_status`,
   `get_operator_profile`, `list_partners`, `find_partners`,
   `ask_operator`, `draft_booking`. Select them all and choose **Enable
   actions**.
   - Gemini Enterprise asks before every call to `draft_booking` and
     `ask_operator`.
   - The reads run without asking, because their tools say
     `readOnlyHint`.
6. If the app doesn't use the data store yet, connect it to the app:
   https://docs.cloud.google.com/gemini/enterprise/docs/connectors/connect-existing-data-store

## 7. The scene

To present it, follow [the demo script](demo-script.md): the scenes in
order, what to type, what to point at, and what to do when something goes
wrong. The short version, to check that everything works:

First, in the Gemini Enterprise chat app, **enable the Cymbal Shoes
connector** for your chat. Until you do, the assistant answers from a
web search instead of calling Cymbal's tools, and Cymbal's node sees no
`tool call`.

1. In Gemini Enterprise, ask: *"Using Cymbal Shoes, find a carrier from
   Rotterdam to Newark and draft a booking for one 40' high-cube
   container of sneakers, 12,000 kg, HS 6404, ready October 6, our
   reference CY-PO-55120."*
2. It calls `find_partners` (Ocean Blue is verified, North Star isn't),
   then asks you to allow `draft_booking`. Its answer says nothing has
   been sent and gives a link: `…/ui/?draft=MDxxxx-0001`.
3. Open the link. After sign-in, Cymbal's UI opens that draft, marked as
   a carrier Cymbal has never booked with. Wait for the button to arm,
   then **Confirm**.
4. In Ocean Blue's tab, the booking arrives, signed and confirmed. In
   Cymbal's activity log, the tool calls appear as
   `tool draft_booking via mcp for you@example.com`.
5. Disrupt the voyage from Ocean Blue's view. Then ask Gemini Enterprise
   *"What's the status of CY-PO-55120?"*

## 8. The door-to-door scene

Four tabs: Cymbal (Alex), Meridian (Mia), Ocean Blue (Sam), Serra (Rita).

1. **Cymbal orders.** In Cymbal's Assistant, type *"Ask Meridian
   Forwarding to ship one 45-footer of the Brazil launch sneakers from our
   Tilburg warehouse to the Cajamar DC, door to door."*
   - The assistant reads Cymbal's private context and drafts the order
     with a deliver-by date two days before the launch.
   - The dialog lists everything Meridian gets, and lists the launch date,
     the receiving slot and the note as **Not sent**.
   - Click **Send order**. Meridian takes it as pending.
2. **Meridian books.** In Meridian's **Orders** tab, open the order, then
   **Confirm plan & book 3 legs…**. The dialog shows what each carrier
   gets:
   - Polder: an EDIFACT IFTMIN, unsigned;
   - Ocean Blue and Serra: signed bookings.

   Book. The journey starts. Polder's statuses reach Cymbal marked
   **◇ unsigned**, and Ocean Blue's events marked **✓ signed**.
3. **The ship is late.** In Ocean Blue's Bookings, **Disrupt…** the
   booking by 72 hours.
   - Cymbal's banner says **misses your deadline**.
   - Meridian's order shows a proposal to re-time Serra's truck.
4. **Meridian's desk.** In Meridian's operator-side Assistant, ask *"What
   should we do about MF-0007?"* (the order's reference, as the Orders tab shows it). It says what
   happened and the options, and drafts confirming the proposal. Its
   dialog sends the amendment to Serra.
5. **Cymbal reacts.** On Cymbal's banner, click **✦ What can we do?**.
   - The assistant weighs the delay against the private launch date and
     notes, and drafts a question to Meridian.
   - Its dialog shows the exact text, with nothing private in it.
     **Ask** sends it, and Meridian's agent answers leg by leg, saying
     which facts are unsigned.

The journey takes about 21 simulated days: at the default
`HOURS_PER_SECOND=1`, about 8 minutes, with the ship at sea for about 6 of
them. Disrupt while it's at sea.

## When something fails

| Symptom | Cause and fix |
|---|---|
| `redirect_uri_mismatch` at Google | A redirect URI from step 2 is missing or has a typo, or it's still propagating (wait 5 min). |
| **Not allowed** after sign-in | The email isn't in `MEMBERS`. Add it and re-run the script. |
| Verify Auth works, but Reload custom actions fails with 401 | The account isn't in `MEMBERS`, or the scopes lack `email`. Cymbal's log says `unauthorized`. |
| Gemini Enterprise can't reach the server | `allowedEgressFqdns` is missing the run.app host or Google's hosts (step 5). |
| Gemini Enterprise answers from the web; Cymbal's log has no `tool call` | The Cymbal Shoes connector isn't enabled in the chat app (step 7). Enable it, and check the data store is connected to the app. |
| Reload custom actions shows nothing; Cymbal's log has no `/mcp` request | The MCP Server URL is wrong, or `allowedEgressFqdns` blocks the run.app host (step 5). |
| The assistant answers from templates | Vertex AI isn't enabled, or the model isn't available in `LLM_LOCATION`. Set `LLM_MODEL` or `LLM_LOCATION` and re-run. |
| Nodes reject each other's messages | Read each node's log: `gcloud run services logs read cymbal-shoes --region "$REGION"`. |
| `meridian-forwarding` doesn't start; its log says `edi: drop directory: … permission denied` | The node can't write to the in-memory volumes. The services run in the second-generation environment (`--execution-environment gen2`), which in-memory volumes need. If the error persists, mount the drops on a directory the nonroot user owns, or file an issue with the log. |
| Meridian's booking stops at Polder's leg ("no acknowledgement") | The `polder` sidecar isn't running, or can't use the file drops. Read its log: Console → Cloud Run → meridian-forwarding → **Logs**, container `polder`. |
| Meridian refuses to book: a leg ends after the order's date | The dates are stale: Cymbal's deadlines were set when you deployed. Re-run the script, then order again. |
| "What can we do?" says the deadline is still met | The same: re-run the script on the day, so the 72-hour delay misses the launch. |

Logs: Console → Cloud Run → the service → **Logs**, or `gcloud run
services logs read <service> --region "$REGION"`.

## Tearing it down

```bash
gcloud run services delete ocean-blue cymbal-shoes meridian-forwarding serra-transportes --region "$REGION"
for s in ulp-ob-signing-key ulp-cy-signing-key ulp-mf-signing-key ulp-sr-signing-key ulp-google-client-secret \
         ulp-ob-config ulp-cy-config ulp-mf-config ulp-sr-config \
         ulp-cy-business ulp-cy-address-book ulp-cy-directory; do gcloud secrets delete "$s" --quiet; done
gcloud artifacts repositories delete supercargo --location "$REGION"
for a in ocean-blue cymbal-shoes meridian-forwarding serra-transportes; do
  gcloud iam service-accounts delete "ulp-$a@$PROJECT.iam.gserviceaccount.com"
done
```

Then delete the OAuth client and the Gemini Enterprise data store in the
console.
