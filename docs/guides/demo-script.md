# Demo script: the hosted demo, end to end

How to present the two-company demo once it runs on Cloud Run, with
Gemini Enterprise driving Cymbal Shoes' node over MCP. Setting it up is
[the runbook](runbook-cloudrun.md); this is the order of scenes, what to
type, what to point at, and why each step matters. It takes about 10
minutes.

Replace `<project number>` and `<region>` with yours (the runbook's step 0).

| | |
|---|---|
| Cymbal Shoes (shipper, Alex) | `https://cymbal-shoes-<project number>.<region>.run.app/ui/` |
| Ocean Blue (carrier, Sam) | `https://ocean-blue-<project number>.<region>.run.app/ui/` |
| Gemini Enterprise | the app with the Cymbal Shoes data store (runbook step 6) |

## Before you start

1. Open both UIs in **two tabs**, and **Sign in with Google** in each. A
   session lasts 12 hours, so sign in fresh before presenting. (The
   presenter page, which frames both UIs, doesn't work on `run.app`: see
   the runbook's step 4.)
2. In Gemini Enterprise, start a **new chat** and **enable the Cymbal Shoes
   connector**. Without it, the assistant answers from a web search.
3. Don't redeploy shortly before presenting: the nodes keep their bookings
   and sessions in memory, and a redeploy starts them empty.

## The scenes

### 1. Find a carrier (Gemini Enterprise)

> Find a carrier from Rotterdam to Newark for Cymbal Shoes.

- Ocean Blue comes back **verified**; North Star Lines **unverified** (its
  directory listing has no profile that checks out).
- *Why it matters:* the agent uses the company's own node as a tool. A
  directory entry is a claim; trust comes from the carrier's signed
  profile at its own domain.

### 2. Draft a booking (Gemini Enterprise)

> Draft a booking with Ocean Blue for one 40' high-cube container of
> sneakers, 12,000 kg, HS 6404, from Rotterdam to Newark, ready
> `<tomorrow's date>`, reference CY-PO-55120, consignee Cymbal Footwear
> (cymbal-footwear.example). It has to arrive within a week.

- Gemini asks for permission first: drafting isn't a read-only tool.
- It answers that nothing has been sent, with a link to the draft:
  `…/ui/?draft=MD…-0001`.
- *Why it matters:* the agent can only draft. Nothing leaves the company
  without a person's confirmation.

### 3. A person confirms (Cymbal's tab)

- Open the link, or **Review…** in the "From your agents (MCP)" box.
- Point at, in the dialog:
  - the **new partner** warning: Cymbal has never booked with Ocean Blue;
  - **"Not sent: your deadline"**: the private date stays with Cymbal.
- Wait for the button to arm, then **Send booking request**.
- *Why it matters:* the request is signed with Cymbal's key. Under
  **Activity**, the `tool` entry names who asked; the booking's messages
  carry `REST` badges.

### 4. Ocean Blue receives it (Ocean Blue's tab)

- The booking appears with its journey, and the voyage starts.
- *Why it matters:* Ocean Blue verified Cymbal's signature against the keys
  Cymbal publishes before doing anything with the request.

### 5. The carrier's assistant drafts a delay (Ocean Blue's tab, Assistant)

Do this within about 2 minutes of scene 3: the voyage runs at 1 simulated
hour per second.

> Which bookings are on voyage 42W?

> Draft a 72-hour delay for the whole voyage: engine repair, unscheduled
> call at Halifax.

- The draft opens a dialog showing exactly what each customer gets.
  **Publish delay**. (The booking's **Disrupt** button does the same by
  hand.)

### 6. The delay arrives (Cymbal's tab)

- Live: the alert, the banner, the **DELAYED** card, the journey's last
  leg turning red.
- The **summary** says what the delay means for *Cymbal*, drawing on its
  private context (the warehouse slot).
- *Why it matters:* private business context is used by Cymbal's own model
  and never sent to anyone.

### 7. Ask about it (Gemini Enterprise)

> What's the status of CY-PO-55120?

> Ask Ocean Blue why CY-PO-55120 is delayed.

- The second asks for permission first. Ocean Blue's model answers from
  that booking's records and cites them.
- *Why it matters:* the question and the answer are signed messages over
  **A2A**: blue `A2A` badges in both tabs' Activity. **Messages** shows the
  exact signed bytes.

## When something goes wrong

| Symptom | What to do |
|---|---|
| Gemini Enterprise searches the web | The connector isn't enabled in this chat: enable it, or start a new chat. |
| The voyage arrived before you disrupted it | Draft another booking (scene 2), or go on to scene 7. |
| A tab shows the sign-in page | Sign in again: the session expired. |
| A status request is refused once | Gemini guessed a reference; it lists the shipments and retries. |
| The hosted nodes are unavailable | Run the same scenes on the local demo (`dev/demo/hello-ulp`), without Gemini Enterprise. |
