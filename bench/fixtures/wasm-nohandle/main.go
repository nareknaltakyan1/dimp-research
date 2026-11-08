// Command wasm-nohandle is a benchmark fixture for E6 (failed upgrade) of
// s3-wasm: a valid WASI module that does NOT export the "handle" function the
// host requires, so wasmimpl.Load rejects it before publication.
// Build: GOOS=wasip1 GOARCH=wasm go build -o v2-broken.wasm ./bench/fixtures/wasm-nohandle
package main

func main() {}
