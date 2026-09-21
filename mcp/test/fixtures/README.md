Fixtures for the ezbookkeeping MCP tests. Every value here is synthetic (pm/apis.mdx §20): invented
account names, invented ids, no real path, no real key.

* plane_routes.json — the machine plane's route table as `GET /machine/v1/capabilities` published
  it. The route-parity test asserts every tool names exactly one route in it (both directions, with
  the omissions table in test/catalogue.test.ts). Regenerate against a live server:
  `curl -s -H "X-Ezbk-Api-Key: $KEY" http://127.0.0.1:8080/machine/v1/capabilities` and keep
  method, path, tier, status per route.
* envelopes/*.json — recorded tool envelopes from the live integration run over synthetic books,
  used by the currency and redaction canaries. Regenerate with `EZBKMCP_RECORD_FIXTURES=1 npm test`
  while a server binary is available.
