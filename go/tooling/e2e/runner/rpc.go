package runner

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// RPCCaller drives the RPC surface: native gRPC to the gateway's RPC
// listener. The runner has no generated stubs, so it calls dynamically: the
// codegen bakes the surface's descriptors into the runner (Descriptors), the
// step's JSON input becomes the request message through protojson, and the
// answer comes back as JSON in the shape REST writes (proto field names,
// enums as numbers), so the matchers and captures apply unchanged.
//
// The token goes out as `authorization: Bearer <token>` metadata, a
// credential as the whole `authorization` value, and a step's headers as
// metadata (lower-cased, as gRPC requires). A refusal is a CallError whose
// Code is the status code in the SCREAMING_SNAKE spelling every transport
// uses, with the w17.ErrorDetail entries as Details.
type RPCCaller struct {
	conn  *grpc.ClientConn
	files *protoregistry.Files
}

// NewRPCCaller dials target (`localhost:8082`, plaintext h2c, as the
// listener serves) and reads the baked descriptors, a serialized
// google.protobuf.FileDescriptorSet. Nothing is sent until the first call.
func NewRPCCaller(target string, descriptors []byte) (*RPCCaller, error) {
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(descriptors, set); err != nil {
		return nil, fmt.Errorf("rpc: the baked descriptors: %w", err)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("rpc: the baked descriptors: %w", err)
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("rpc: dial %s: %w", target, err)
	}
	return &RPCCaller{conn: conn, files: files}, nil
}

// Close closes the connection.
func (c *RPCCaller) Close() error { return c.conn.Close() }

func (c *RPCCaller) message(name string) (*dynamicpb.Message, error) {
	d, err := c.files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, fmt.Errorf("rpc: message %s is not in the baked descriptors: %w", name, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("rpc: %s is not a message", name)
	}
	return dynamicpb.NewMessage(md), nil
}

var (
	rpcUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: false}
	rpcMarshal   = protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}
)

// Call makes one unary call.
func (c *RPCCaller) Call(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string, credential string, files []FilePart) (map[string]any, error) {
	op := "rpc " + ep.FullMethod
	if len(files) > 0 {
		return nil, fmt.Errorf("%s: files: is multipart, which the RPC surface does not speak — send the bytes as a field", op)
	}
	if ep.Stream {
		return nil, fmt.Errorf("%s: a streaming method; the runner drives unary RPC calls only", op)
	}
	req, err := c.message(ep.RPCRequest)
	if err != nil {
		return nil, err
	}
	resp, err := c.message(ep.RPCResponse)
	if err != nil {
		return nil, err
	}
	if len(input) > 0 {
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("%s: input: %w", op, err)
		}
		if err := rpcUnmarshal.Unmarshal(raw, req); err != nil {
			return nil, fmt.Errorf("%s: input does not fit %s: %w", op, ep.RPCRequest, err)
		}
	}
	md := metadata.MD{}
	for k, v := range headers {
		md.Append(k, v)
	}
	switch {
	case credential != "":
		md.Set("authorization", credential)
	case token != "" && ep.AuthRequired && !ep.CredentialInURL && !ep.Mint:
		md.Set("authorization", "Bearer "+token)
	}
	if len(md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	if err := c.conn.Invoke(ctx, ep.FullMethod, req, resp); err != nil {
		return nil, rpcCallError(op, err)
	}
	out, err := rpcMarshal.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("%s: answer: %w", op, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("%s: answer: %w", op, err)
	}
	return m, nil
}

// rpcCallError is a status as the CallError every transport answers with;
// an error that is no status (the listener unreachable) stays a transport
// failure.
func rpcCallError(op string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%s: %w", op, err)
	}
	e := &CallError{Op: op, Code: screamingSnake(st.Code().String()), Message: st.Message(), Raw: truncate([]byte(st.Message()), 512)}
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			e.Details = append(e.Details, CallErrorDetail{Field: ed.GetField(), Code: ed.GetCode(), Message: ed.GetMessage()})
		}
	}
	return e
}

var _ Caller = (*RPCCaller)(nil)
