# Walkthrough 01: understand and validate a request

Status update, September 9, 2026: the user chose assistant-led implementation.
ParseSubmission is now implemented, and these tests run by default. Use this
page to understand and experiment with the code, not as an unfinished task.

Continue with [the implementation walkthrough](02-submission-and-storage.md).

Start here. You do not need the API or PostgreSQL running for this exercise.

## What to read

1. `internal/jobs/request.go`: the request shape and implemented parser.
2. `internal/jobs/request_test.go`: examples of valid and invalid input.

Run `make exercise` to see the passing validation cases. The regression tests
still reject `ErrNotImplemented`, so a placeholder cannot masquerade as validation.

## Implementation to inspect

Read `ParseSubmission(body []byte) (SubmitRequest, error)`.

For this milestone:

- Accept exactly one JSON object.
- Require the exact job type `migrate_campaign`.
- Require `payload` to be a JSON object, not missing, null, a string or an array.
- Return the decoded request on success and a useful error on invalid input.
- Reject malformed JSON and trailing extra JSON documents.
- Unknown fields do not need to be rejected in this first exercise.

Do not validate campaign dates or perform the migration yet. This exercise
validates the request envelope. The worker will handle record-specific rules in
a later milestone. An empty payload object is structurally valid for now.

Trace the valid case, then missing type, then unsupported type. The tests also
cover an empty body, a top-level array/null, a string payload, a numeric job type,
invalid UTF-8, unknown fields and surrounding whitespace. Predict each result
before running the test. Optional practice: add a case you think is missing.

## Questions to answer in your own words

- Why accept bytes rather than an HTTP request in this function?
- What does `json.RawMessage` preserve?
- Why is syntactically valid JSON not necessarily a valid submission?
- What should happen if an unsupported job type is submitted?
- Which checks belong here, and which belong in the future migration worker?

## Discussion checklist

- `make exercise` passes without changing or removing the existing assertions.
- You can explain at least two edge cases and why they are rejected.
- You can explain each branch of your function and one bug caught by a test.

The parser is now connected to HTTP, with a 64 KiB body limit and explicit error
responses. Successful submissions are committed to PostgreSQL before returning
202. Continue with the storage walkthrough to understand that boundary.

Reference: [Go encoding/json documentation](https://pkg.go.dev/encoding/json).
