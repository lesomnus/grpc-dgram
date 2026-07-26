module github.com/lesomnus/grpc-dgram/examples/websocket-echo

go 1.26.1

require (
	github.com/gorilla/websocket v1.5.3
	github.com/lesomnus/grpc-dgram v0.0.0
	github.com/lesomnus/grpc-dgram/transport/gorilla v0.0.0
	google.golang.org/grpc v1.79.2
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)

replace github.com/lesomnus/grpc-dgram => ../..

replace github.com/lesomnus/grpc-dgram/transport/gorilla => ../../transport/gorilla
