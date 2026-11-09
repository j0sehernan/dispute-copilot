---
id: visa-13.6
network: visa
reason_code: "13.6"
category: credit_not_processed
title: Credit Not Processed
requirements:
  - id: refund_issued
    text: A refund for this transaction has status "completed" and was created before the dispute was opened.
  - id: return_policy_accepted
    text: The customer accepted the store return policy at checkout.
  - id: no_return_received
    text: The merchant has not received the item back (no return recorded for this order).
fight_when:
  - [refund_issued]
  - [return_policy_accepted, no_return_received]
---
## Summary

The cardholder says they returned the goods or cancelled, and the promised refund never arrived.

## What wins

A completed refund with its date and reference (ARN), so the issuer can find it. Or proof that the customer never returned the goods even though the return policy required it.

## What does not win

If the return arrived and there is no refund, the cardholder is right.
