// Code generated by protoc-gen-go-grpc. DO NOT EDIT.

package userdaemon

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// SystemAClient is the client API for SystemA service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type SystemAClient interface {
	// ResolveInterceptIngressInfo gets the ingress information that the daemon should use to create the preview url
	// associated with an intercept
	ResolveIngressInfo(ctx context.Context, in *IngressInfoRequest, opts ...grpc.CallOption) (*IngressInfoResponse, error)
	// ReportAvailableNamespaces
	ReportAvailableNamespaces(ctx context.Context, opts ...grpc.CallOption) (SystemA_ReportAvailableNamespacesClient, error)
}

type systemAClient struct {
	cc grpc.ClientConnInterface
}

func NewSystemAClient(cc grpc.ClientConnInterface) SystemAClient {
	return &systemAClient{cc}
}

func (c *systemAClient) ResolveIngressInfo(ctx context.Context, in *IngressInfoRequest, opts ...grpc.CallOption) (*IngressInfoResponse, error) {
	out := new(IngressInfoResponse)
	err := c.cc.Invoke(ctx, "/telepresence.userdaemon.SystemA/ResolveIngressInfo", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *systemAClient) ReportAvailableNamespaces(ctx context.Context, opts ...grpc.CallOption) (SystemA_ReportAvailableNamespacesClient, error) {
	stream, err := c.cc.NewStream(ctx, &SystemA_ServiceDesc.Streams[0], "/telepresence.userdaemon.SystemA/ReportAvailableNamespaces", opts...)
	if err != nil {
		return nil, err
	}
	x := &systemAReportAvailableNamespacesClient{stream}
	return x, nil
}

type SystemA_ReportAvailableNamespacesClient interface {
	Send(*AvailableNamespacesRequest) error
	CloseAndRecv() (*emptypb.Empty, error)
	grpc.ClientStream
}

type systemAReportAvailableNamespacesClient struct {
	grpc.ClientStream
}

func (x *systemAReportAvailableNamespacesClient) Send(m *AvailableNamespacesRequest) error {
	return x.ClientStream.SendMsg(m)
}

func (x *systemAReportAvailableNamespacesClient) CloseAndRecv() (*emptypb.Empty, error) {
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	m := new(emptypb.Empty)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// SystemAServer is the server API for SystemA service.
// All implementations must embed UnimplementedSystemAServer
// for forward compatibility
type SystemAServer interface {
	// ResolveInterceptIngressInfo gets the ingress information that the daemon should use to create the preview url
	// associated with an intercept
	ResolveIngressInfo(context.Context, *IngressInfoRequest) (*IngressInfoResponse, error)
	// ReportAvailableNamespaces
	ReportAvailableNamespaces(SystemA_ReportAvailableNamespacesServer) error
	mustEmbedUnimplementedSystemAServer()
}

// UnimplementedSystemAServer must be embedded to have forward compatible implementations.
type UnimplementedSystemAServer struct {
}

func (UnimplementedSystemAServer) ResolveIngressInfo(context.Context, *IngressInfoRequest) (*IngressInfoResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ResolveIngressInfo not implemented")
}
func (UnimplementedSystemAServer) ReportAvailableNamespaces(SystemA_ReportAvailableNamespacesServer) error {
	return status.Errorf(codes.Unimplemented, "method ReportAvailableNamespaces not implemented")
}
func (UnimplementedSystemAServer) mustEmbedUnimplementedSystemAServer() {}

// UnsafeSystemAServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to SystemAServer will
// result in compilation errors.
type UnsafeSystemAServer interface {
	mustEmbedUnimplementedSystemAServer()
}

func RegisterSystemAServer(s grpc.ServiceRegistrar, srv SystemAServer) {
	s.RegisterService(&SystemA_ServiceDesc, srv)
}

func _SystemA_ResolveIngressInfo_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(IngressInfoRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(SystemAServer).ResolveIngressInfo(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/telepresence.userdaemon.SystemA/ResolveIngressInfo",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(SystemAServer).ResolveIngressInfo(ctx, req.(*IngressInfoRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _SystemA_ReportAvailableNamespaces_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(SystemAServer).ReportAvailableNamespaces(&systemAReportAvailableNamespacesServer{stream})
}

type SystemA_ReportAvailableNamespacesServer interface {
	SendAndClose(*emptypb.Empty) error
	Recv() (*AvailableNamespacesRequest, error)
	grpc.ServerStream
}

type systemAReportAvailableNamespacesServer struct {
	grpc.ServerStream
}

func (x *systemAReportAvailableNamespacesServer) SendAndClose(m *emptypb.Empty) error {
	return x.ServerStream.SendMsg(m)
}

func (x *systemAReportAvailableNamespacesServer) Recv() (*AvailableNamespacesRequest, error) {
	m := new(AvailableNamespacesRequest)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// SystemA_ServiceDesc is the grpc.ServiceDesc for SystemA service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var SystemA_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "telepresence.userdaemon.SystemA",
	HandlerType: (*SystemAServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "ResolveIngressInfo",
			Handler:    _SystemA_ResolveIngressInfo_Handler,
		},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "ReportAvailableNamespaces",
			Handler:       _SystemA_ReportAvailableNamespaces_Handler,
			ClientStreams: true,
		},
	},
	Metadata: "rpc/userdaemon/userdaemon.proto",
}