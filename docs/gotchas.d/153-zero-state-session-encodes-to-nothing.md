# A test fake that encodes a `fleet.Session` with a zero `State` sends an empty body

**Issue:** #153 (writing fake peers for the remote driver's label tests)

## What happened

A fake peer answered a relayed create with
`json.NewEncoder(w).Encode(fleet.Session{SessionRef: …, Labels: …})` and the
remote driver under test failed with a bare `EOF` while decoding the response.

`fleet.Status` and `fleet.Confidence` have strict `MarshalJSON` methods that
refuse values outside their closed sets — including `""`, the zero value. So a
`Session` whose `State` was never set does not encode at all:

```
json: error calling MarshalJSON for type *fleet.Session:
json: error calling MarshalJSON for type *fleet.Status: fleet: "" is not a valid Status
```

The fake discarded `Encode`'s error (`_ = …Encode(…)`), the status line had
already been written, and the client saw a well-formed response with no body.
Nothing points at the encoder; the symptom reads like a transport problem.

## The rule

Any `fleet.Session` a test serializes needs a real state, e.g.
`State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil)`. When a decode
in a test fails with `EOF`, check the fake's `Encode` error before suspecting
the client.
