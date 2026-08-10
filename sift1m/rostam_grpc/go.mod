// Nested module: keeps the gRPC dependency tree OUT of Rostam's lean core
// go.mod. This server exists only for the bench/sift1m gRPC head-to-head.
module rostamgrpcbench

go 1.26.1

require (
	github.com/rostamlabs/rostam v0.0.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
)

replace github.com/rostamlabs/rostam => ../../../rostam
