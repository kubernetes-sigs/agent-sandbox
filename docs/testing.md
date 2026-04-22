
## To create a kind cluster
```shell
make deploy-kind
```

## Running unit tests
To run all unit tests:
```shell
make test-unit
```
## Running the e2e tests (including benchmarks)
To run all e2e tests:
```shell
make test-e2e
```
## Running only e2e benchmarks
To run only e2e benchmarks:
```shell
make test-e2e --suite=benchmarks
```

## Race detection
Both `test-unit` and `test-e2e` invoke `go test -race`, so data races in
the concurrently-running reconcilers (`SandboxReconciler`,
`SandboxClaimReconciler`, `SandboxWarmPoolReconciler`) fail the build
instead of silently corrupting state. The race-detector binary has a
non-trivial memory and CPU overhead, so test runs are somewhat slower
and use more RAM than without it — see
[go.dev/doc/articles/race_detector](https://go.dev/doc/articles/race_detector)
for details.

## Remove the kind cluster
```shell
make delete-kind
```

### See also
- [Kubernetes testing guide](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/testing.md)
- [Integration Testing in Kubernetes](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/integration-tests.md)
- [End-to-End Testing in Kubernetes](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md)
- [Flaky Tests in Kubernetes](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/flaky-tests.md)
