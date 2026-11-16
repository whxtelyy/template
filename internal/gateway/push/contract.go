package push

//go:generate go tool mockgen -destination=mocks_test.go -package=push net/http RoundTripper
//go:generate go tool mockgen -destination=body_mocks_test.go -package=push io ReadCloser
