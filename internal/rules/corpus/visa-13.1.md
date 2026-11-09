---
id: visa-13.1
network: visa
reason_code: "13.1"
category: not_received
title: Merchandise or Services Not Received
requirements:
  - id: delivered
    text: The carrier shows the parcel as delivered, with a delivery date before the dispute was opened.
  - id: delivery_address_match
    text: The address where the carrier delivered is the same as the shipping address on the order.
  - id: delivery_proof
    text: There is proof at the door, a signature or a delivery photo.
fight_when:
  - [delivered, delivery_address_match, delivery_proof]
---
## Summary

The cardholder says the goods never arrived. The merchant has to show that the goods were delivered to the address the cardholder gave.

## What wins

Carrier tracking that shows delivery to the exact shipping address on the order, together with a signature or photo taken at delivery. Customer service messages where the merchant shared tracking help to show good faith.

## What does not win

A tracking number alone, or a status like "shipped" or "in transit", is not proof of delivery. Delivery to a different address than the one on the order supports the cardholder, even if the street is close.
