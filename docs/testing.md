
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
## Running Tests with Race Detector

Unit tests run with Go’s race detector (`-race`) enabled by default.

To run e2e tests with race detection:

```shell
make test-e2e-race
```
## Remove the kind cluster
```shell
make delete-kind
```

### See also
- [Kubernetes testing guide](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/testing.md)
- [Integration Testing in Kubernetes](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/integration-tests.md)
- [End-to-End Testing in Kubernetes](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md)
- [Flaky Tests in Kubernetes](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/flaky-tests.md)
