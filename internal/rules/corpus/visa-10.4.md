---
id: visa-10.4
network: visa
reason_code: "10.4"
category: fraud
title: Other Fraud, Card-Absent Environment
requirements:
  - id: authentication
    text: The purchase was authenticated with 3-D Secure (status "authenticated"). This moves fraud liability to the issuer.
  - id: avs_cvv_match
    text: The address check (AVS) returned a full match "Y" and the card security code check (CVV2) returned "M".
  - id: prior_undisputed_history
    text: The same customer has at least two earlier orders that were not disputed, placed from the same device ID or IP address as the disputed order.
fight_when:
  - [authentication]
  - [avs_cvv_match, prior_undisputed_history]
---
## Summary

The cardholder says they did not make or allow a card-not-present purchase. The issuer has already confirmed the claim with the cardholder, so the merchant has to show either that the issuer carries the fraud risk (authentication) or that the real cardholder was very likely the buyer.

## What wins

A successful 3-D Secure authentication is the strongest answer: with it, the issuer cannot charge the fraud back. Without it, the merchant needs compelling evidence that ties the purchase to the cardholder. The usual pattern is a clean AVS and CVV result plus earlier orders by the same customer, from the same device or IP, that the cardholder never disputed.

## What does not win

A successful authorization alone proves nothing about who used the card. Delivery to an address is not enough if that address is not linked to the cardholder. A first-time customer with a failed or unavailable address check is very hard to defend.
