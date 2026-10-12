# Multi-Leg Freight Shipping and API Orchestration: Comprehensive Guide

## 1. Introduction to Multi-Leg Shipping and Responsibilities

When cargo moves from Company A (seller) to Company B (buyer) through multiple transport modes (e.g., land $\rightarrow$ ship $\rightarrow$ land), responsibility for booking and managing the journey is **not automatically assigned to the seller**. 

Instead, responsibility is governed entirely by negotiated **Incoterms** (International Commercial Terms):
* **Buyer-Managed Transport (e.g., Ex Works / EXW):** The buyer orchestrates and pays for the entire multi-leg journey from origin loading dock to final destination.
* **Seller-Managed Transport (e.g., Delivered Duty Paid / DDP):** The seller handles and pays for every leg of transit, customs clearance, and final delivery.
* **Shared Responsibility (e.g., CPT, CIF):** Responsibility and cost split at designated transit points or ports.

---

## 2. Execution and Tracking: The Role of Freight Forwarders

Regardless of whether the buyer or seller manages the transport, the orchestrating party rarely calls individual carriers directly. Instead, they hire a **Freight Forwarder** under a single contract.

* **Sub-Contracting Legs:** The forwarder coordinates origin drayage (trucking), main ocean freight (container ship), and destination drayage (trucking) behind the scenes.
* **Control Towers & Visibility Platforms:** Platforms like project44 and FourKites aggregate live tracking data from disparate carriers (truck ELDs, ocean AIS transponders) into centralized dashboards using REST APIs and automated webhooks.

---

## 3. Developer Access, Sandboxes, and API Mocks

### Enterprise API Constraints
* Enterprise visibility platform APIs (such as project44 or FourKites) are **not open self-service public sandboxes**. 
* Access requires corporate verification, enterprise contracts, signed NDAs, and admin-generated credentials.

### Building API Mocks
When direct platform access is unavailable, developers can simulate these logistics APIs locally:
* **WireMock:** Ingests OpenAPI/Swagger specifications, creates mock routing endpoints, and supports dynamic Handlebars templating to generate realistic tracking data and stateful milestone transitions.
* **Mockoon / Prism:** Lightweight open-source tools for rapid REST API simulation and specification-driven mocking.

---

## 4. Forwarder Workflows and Standards Bodies

Freight forwarders rely on standardized data formats defined by international associations to bridge communication across carriers:

* **DCSA (Digital Container Shipping Association):** Standardizes ocean container tracking schemas, digital bookings, and eBLs.
* **IATA ONE Record:** Modernizes air and multi-modal logistics data exchange.
* **NMFTA & AAR:** Standardize North American trucking codes (SCAC) and rail intermodal equipment protocols.
* **EPCIS & ASTM:** Govern item-level supply chain tracking events and physical container specifications.

---

## 5. Bridging Legacy EDI to Modern JSON APIs

Modern multi-leg supply chains rely on translating legacy, flat-file Electronic Data Interchange (EDI) standards into structured, RESTful JSON payloads.

### Side-by-Side Comparison

* **Legacy EDI 322 (Vessel/Terminal Milestone):**
  ```text
  ISA*00*          *00*          *ZZ*SENDER         *ZZ*RECEIVER       *260330*1200*U*00401*000000001*0*P*:~
  GS*QO*SENDER*RECEIVER*20260330*1200*1*X*004010~
  ST*322*0001~
  B4*20260330*1200*A1*CONT1234567*S*USNYC*D*VESSELX*123N~
  SE*5*0001~
  GE*1*1~
  IEA*1*0001~
  ```

* **Modern DCSA JSON API Payload:**
  ```json
  {
    "eventType": "SHIPMENT_EVENT",
    "eventDateTime": "2026-03-30T12:00:00Z",
    "shipmentEventTypeCode": "ARRI",
    "equipmentReference": "CONT1234567",
    "facilityTypeCode": "TERM",
    "transportCall": {
      "carrierVoyageNumber": "123N",
      "vesselIMO": 9876543,
      "location": {
        "locationName": "Port of New York",
        "UNLocationCode": "USNYC"
      }
    }
  }
  ```

### Key Translation Mechanics
1. **Status Codes:** Legacy shorthand codes (e.g., `A1`, `AF`) are mapped to explicit, human-readable enumerated strings (`ARRI`, `LOAD`).
2. **Transport Architecture:** Batch FTP polling is replaced by real-time REST subscription webhooks.
3. **Data Structures:** Cryptic delimited lines are transformed into hierarchical, self-documenting JSON objects.