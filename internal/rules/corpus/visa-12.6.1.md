---
id: visa-12.6.1
network: visa
reason_code: "12.6.1"
category: duplicate
title: Duplicate Processing
requirements:
  - id: separate_orders
    text: The other charge with the same amount belongs to a different order (different order ID or different items), not to the disputed order.
  - id: both_fulfilled
    text: Both orders were fulfilled; each was shipped or delivered on its own.
fight_when:
  - [separate_orders, both_fulfilled]
---
## Summary

The cardholder says one purchase was charged more than once. The merchant has to show that each charge belongs to a separate purchase.

## What wins

Two different order numbers, ideally with different items or options, and fulfillment records for both. Order confirmation emails help.

## What does not win

Two captures for the same order number. If the second capture was never refunded, accept the chargeback.
