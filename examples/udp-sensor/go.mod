module github.com/lesomnus/grpc-dgram/examples/udp-sensor

go 1.26.1

require (
	github.com/lesomnus/grpc-dgram v0.0.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260724162435-b2f20204f0df // indirect
)

replace github.com/lesomnus/grpc-dgram => ../..
