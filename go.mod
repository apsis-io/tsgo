module github.com/apsis-io/tsgo

go 1.27

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/klauspost/compress v1.20.0
	github.com/microsoft/TypeScript/tsc v0.0.0-20261006000712-a1ef42b9ea70
	github.com/zeebo/xxh3 v1.1.0
	golang.org/x/sync v0.23.0
	golang.org/x/text v0.42.0
)

require (
	github.com/klauspost/cpuid/v2 v2.2.10 // indirect
	github.com/matryer/moq v0.7.1 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
)

tool (
	github.com/matryer/moq
	golang.org/x/tools/cmd/stringer
)
