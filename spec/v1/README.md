# spec/v1: a copy of the format v1 golden sample

These files are a byte-for-byte copy of
[redirhub/integration-spec's `spec/v1/`](https://github.com/redirhub/integration-spec/tree/main/spec/v1),
the normative sample of Integration Mode format v1; what each file means is in its README there.
`spec_test.go` loads them. CI fails when a file differs from the specification's `main`: change
the specification first, then copy the files over here.
