# Operational Runbook: Payment Disputes & Chargebacks

- **Version**: 1.0.0
- **Audience**: Property Owners, Facility Managers, Operations & Finance Admins
- **Subsystem**: Cashfree Payment Gateway Ingress (`internal/cashfree`, `internal/api/handlers_pay.go`)
- **Status**: Production Standard

---

## 1. Executive Summary & Philosophy

When a tenant or cardholder initiates a dispute or chargeback with their issuing bank (e.g. claiming "unauthorized transaction", "services not rendered", or "double charge"), Cashfree dispatches an asynchronous dispute webhook to PG Cashflow.

### The Zero Automated Mutation Invariant
Under double-entry accounting discipline, **an incoming dispute is a contested claim, not an approved refund**. 
1. The backend automatically ingests the webhook, logs loud alerts, and notifies the property owner.
2. The backend **never** automatically reverses journal entries (`AcctRentRevenue`), nor does it flip `due.Status` back to unpaid upon dispute receipt.
3. Automatically reversing revenue before bank adjudication would corrupt the ledger and introduce duplicate debit entries if the property owner contests the dispute with proof of accommodation and prevails.

---

## 2. Ingested Webhook Metadata

When Cashfree dispatches `PAYMENT_DISPUTE_CREATED_WEBHOOK`, `DISPUTE_CREATED_WEBHOOK`, or `DISPUTE_STATUS_UPDATE_WEBHOOK`, the payload is mapped to `cashfree.DisputeWebhook`:

| Webhook Field | Go Struct Field | Description | Example Value |
|---|---|---|---|
| `data.dispute_id` | `DisputeID` | Cashfree unique alphanumeric dispute reference | `"DISP_9823145"` |
| `data.order_id` | `OrderID` | Internal PG Cashflow due / order ID | `"due_01J8K9...`" |
| `data.cf_payment_id` | `CFPaymentID` | Gateway payment reference | `"1982736412"` |
| `data.dispute_type` | `DisputeType` | Classification of the dispute | `"CHARGEBACK"`, `"RETRIEVAL"`, `"FRAUD"` |
| `data.dispute_status` | `DisputeStatus` | Current lifecycle state | `"ACTION_REQUIRED"`, `"UNDER_REVIEW"`, `"WON"`, `"LOST"` |
| `data.dispute_amount` | `DisputeAmount` | Disputed amount strictly in integer paise | `1500000` (₹15,000.00) |
| `data.reason_code` | `ReasonCode` | Card network / bank chargeback reason code | `"10.4"`, `"4837"`, `"UA01"` |
| `data.reason_description` | `ReasonDescription` | Plaintext explanation provided by bank | `"Cardholder claims transaction not recognized"` |
| `data.respond_by` | `RespondBy` | Hard statutory deadline to submit contestation proof | `"2026-10-05T18:30:00Z"` |

### System Side-Effects Upon Ingestion:
1. **Audit Record**: Inserted or updated in PostgreSQL table `webhook_events` with status `dispute_action_required` and full raw JSON payload.
2. **Operator Alert**: Loud `slog.Error("ALERT: Cashfree payment dispute/chargeback received - zero-mutation fail-safe", ...)` logged for observability monitoring.
3. **In-App Notification**: Event `domain.EvtPaymentDisputed` published to the outbox, displaying an urgent push/feed banner to property owners linking to `/owner/payments`.
4. **Gateway Acknowledgment**: HTTP `200 OK` returned immediately to prevent Cashfree retry storms.

---

## 3. Statutory Timelines & SLA Matrix

Card networks (Visa, Mastercard, RuPay) and NPCI (UPI) enforce strict, non-negotiable contestation windows:

| Phase | Statutory Limit | Recommended Operational SLA | Action Required |
|---|---|---|---|
| **Triage & Acknowledgement** | Within 24 hours of notification | **< 4 hours** | Review dispute details in Cashfree Merchant Dashboard |
| **Evidence Pack Compilation** | Within `RespondBy` (typically 7–14 days) | **< 48 hours** | Assemble KYC, agreement, and occupancy proof |
| **Dashboard Submission** | 24 hours before `RespondBy` deadline | **> 48 hours before deadline** | Upload PDF evidence pack to Cashfree portal |
| **Bank Adjudication** | 30–60 business days | Passive monitoring | Track status updates via webhook / dashboard |

> [!WARNING]
> **Missing the `RespondBy` deadline results in immediate, irreversible forfeiture of the funds** to the issuing bank, regardless of the validity of your rental claim.

---

## 4. Standard Evidence Pack Checklist (Accommodation Provider)

Indian payment aggregators and card networks require verifiable documentation proving that the cardholder/tenant legitimately contracted for and utilized the residential premises. Compile the following 5 exhibits into a single consolidated PDF:

### Exhibit A: Tenant Identity & KYC Verification
* Export from `/owner/tenants/:id/kyc`.
* DigiLocker verifiable XML/PDF or verified UIDAI Aadhaar QR cryptographic verification timestamp.
* Tenant registered mobile number and email ID matching the payment session.

### Exhibit B: Digital Rental Agreement & Terms
* Signed residential tenancy agreement or digitally accepted onboarding consent log.
* Explicit clauses demonstrating:
  * Agreed monthly rent and security deposit amounts.
  * Payment terms (due on 1st/5th of each calendar month).
  * Non-refundable policy for utilized occupancy periods.

### Exhibit C: Proof of Physical Occupancy During Disputed Period
Banks frequently reject disputes when physical occupancy is proven. Provide at least two of:
1. **Move-in Handover Sheet**: Signed room inspection and key handover acknowledgement.
2. **Access / Biometric Logs**: Entry/exit timestamps from biometric turnstiles or digital door locks for the disputed month.
3. **Wi-Fi Router Association Logs**: DHCP lease history showing tenant device MAC address active on PG premises.
4. **Sub-Meter Electricity / Utility Readings**: Monthly meter photos or billing entries recorded for the tenant's room.

### Exhibit D: Payment Match & Invoice History
* System invoice / due record showing billing cycle (e.g., "Rent for September 2026").
* Cashfree payment receipt showing `cf_payment_id`, matching UPI transaction reference (RRN), or masked card digits.
* Prior uninterrupted payment history (demonstrating ongoing tenancy and prior authorized payments).

### Exhibit E: Written Communication & Acknowledgement
* WhatsApp or SMS notification logs showing rent payment confirmation sent to tenant.
* Any ticket, chat, or maintenance requests lodged by the tenant during the disputed month proving active residency.

---

## 5. Step-by-Step Operator Action Workflow

### Step 1: Webhook Receipt & Triage
1. When notified via in-app banner or log monitor, query the incident:
   ```sql
   SELECT id, event_type, status, idempotency_key, payload, created_at 
   FROM webhook_events 
   WHERE status = 'dispute_action_required' 
   ORDER BY created_at DESC;
   ```
2. Note the `order_id`, `cf_payment_id`, `dispute_amount`, and `respond_by` timestamp from `payload`.
3. Check `due_id` and tenant profile in the PG Cashflow admin console.

### Step 2: Tenant Direct Contact (Pre-Contestation)
In ~40% of cases, disputes arise from cardholder confusion (e.g. a parent paying whose bank statement displays "CASHFREE*PG" rather than the PG property name):
1. Call the tenant immediately:
   > *"Hello [Tenant Name], we received a formal bank contestation for your rent payment of ₹[Amount] on [Date]. Was this initiated by you or your family member by mistake?"*
2. **If Confirmed Misunderstanding**: Ask the tenant to contact their bank immediately to formally withdraw the chargeback, and obtain written confirmation via email/WhatsApp.
3. **If Intentional / Fraudulent**: Advise the tenant that formal evidence of residency will be submitted to the bank, and initiate tenancy default procedures.

### Step 3: Evidence Submission via Cashfree Portal
1. Log in to [Cashfree Merchant Dashboard](https://merchant.cashfree.com).
2. Navigate to **Payment Gateway** → **Disputes**.
3. Locate the dispute using `cf_payment_id` or `dispute_id`.
4. Click **Contest Dispute / Submit Evidence**.
5. Upload the compiled PDF evidence pack (Exhibits A through E).
6. Enter an objective, chronological summary in the explanation text box.
7. Submit and record the submission confirmation number.

---

## 6. Post-Adjudication Accounting & Database Reconciliation

Once the issuing bank reviews the evidence, Cashfree dispatches `DISPUTE_STATUS_UPDATE_WEBHOOK` with `dispute_status` set to either `WON` or `LOST`.

### Case A: Dispute Won (Merchant Prevails)
* **Gateway Action**: Cashfree unfreezes the contested funds; payout settlement proceeds normally.
* **Accounting Ledger**: No adjustments required. The original double-entry revenue posting (`Dr AcctGatewayClearing`, `Cr AcctRentRevenue`) remains valid.
* **Audit Trail**: Update event status in database:
  ```sql
  UPDATE webhook_events 
  SET status = 'dispute_won' 
  WHERE idempotency_key = 'dispute:' || :dispute_id;
  ```

### Case B: Dispute Lost (Chargeback Sustained by Bank)
* **Gateway Action**: Cashfree claws back the disputed amount plus bank chargeback fees (typically ₹250–₹500 + GST) from your next settlement cycle.
* **Accounting Ledger Reconciliation**:
  To reflect that the money was clawed back by the bank, a manual journal adjustment must be recorded in `financial_journal_entries`:
  - **Debit**: `AcctRentRevenue` (or `AcctTenantReceivable` if treating as an active debt owed by tenant)
  - **Credit**: `AcctGatewayClearing` (reflecting clawback of funds from gateway settlement)
* **Operational Action**:
  1. Reopen the tenant's due or generate an unpaid adjustment invoice.
  2. Issue formal legal notice of rent default under state tenancy laws.
  3. Deduct the disputed amount and chargeback penalty from the tenant's security deposit via departure deduction if tenant vacates.
  4. Update event status in database:
     ```sql
     UPDATE webhook_events 
     SET status = 'dispute_lost' 
     WHERE idempotency_key = 'dispute:' || :dispute_id;
     ```

---

## 7. Emergency Contacts & Escalation Matrix

* **Cashfree Dispute Operations Desk**: `disputes@cashfree.com`
* **Cashfree Priority Escalations**: Merchant Dashboard Ticket / Assigned Account Manager
* **PG Cashflow Finance Team**: `finance@pgcashflow.com`
* **Legal Counsel / Eviction Advisory**: Designated Property Legal Retainer
