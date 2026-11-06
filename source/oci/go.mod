module github.com/mindfire/zygote/source/oci

go 1.25.14

require github.com/mindfire/zygote v0.0.0-00010101000000-000000000000

require (
	github.com/klauspost/cpuid/v2 v2.0.9
	lukechampine.com/blake3 v1.4.1
)

replace github.com/mindfire/zygote => ../..
