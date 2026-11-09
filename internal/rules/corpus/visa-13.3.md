---
id: visa-13.3
network: visa
reason_code: "13.3"
category: not_as_described
title: Not as Described or Defective Merchandise
requirements:
  - id: return_policy_accepted
    text: The customer accepted the store return policy at checkout.
  - id: no_return_received
    text: The merchant has not received the item back (no return recorded for this order).
  - id: remedy_offered
    text: The merchant offered a remedy (return label, exchange or refund) after the customer complained, and the customer did not use it.
fight_when:
  - [return_policy_accepted, no_return_received, remedy_offered]
---
## Summary

The cardholder says the item was different from the description or was damaged. For this reason code the cardholder is normally expected to try to resolve it with the merchant and to return the goods.

## What wins

Proof that the merchant offered a fair solution, such as a prepaid return label, and the customer did not use it, plus the accepted return policy. The item has not come back to the merchant.

## What does not win

If the merchant already has the item back, the cardholder did their part. Holding both the goods and the money cannot be defended; the right action is to refund.
