package drpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var _ grpc.ServiceRegistrar = &Server{}

type Server struct {
	mu sync.Mutex
	tx FrameHandler
	ss map[uint32]*serverStream

	// methods is a mapping from method name to index.
	// Key must be a full method name (e.g. "/hday.HolderService/Add").
	methods  []*serviceDesc
	services map[string]*serviceDesc

	unary_int  grpc.UnaryServerInterceptor
	stream_int grpc.StreamServerInterceptor

	wg     sync.WaitGroup
	drain  atomic.Bool
	closed atomic.Bool
}

func NewServer(tx FrameHandler, opts ...ServerOption) *Server {
	opt := serverOption{}
	for _, o := range opts {
		o.apply(&opt)
	}

	v := &Server{
		tx: tx,
		ss: map[uint32]*serverStream{},

		methods:  []*serviceDesc{},
		services: map[string]*serviceDesc{},

		// unary_int: ,
	}
	if opt.unary_int != nil {
		opt.unary_ints = append([]grpc.UnaryServerInterceptor{opt.unary_int}, opt.unary_ints...)
	}
	if opt.unary_ints != nil {
		v.unary_int = chainUnaryServerInterceptors(opt.unary_ints)
	}
	if opt.stream_int != nil {
		opt.stream_ints = append([]grpc.StreamServerInterceptor{opt.stream_int}, opt.stream_ints...)
	}
	if opt.stream_ints != nil {
		v.stream_int = chainStreamServerInterceptors(opt.stream_ints)
	}

	return v
}

func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	for i, method := range desc.Methods {
		fullname := fmt.Sprintf("/%s/%s", desc.ServiceName, method.MethodName)
		d, ok := s.services[fullname]
		if !ok {
			d = &serviceDesc{
				index:    uint32(len(s.methods)),
				fullname: fullname,
			}
			s.services[fullname] = d
			s.methods = append(s.methods, d)
		}

		d.service = desc
		d.method = &desc.Methods[i]
		d.impl = impl
	}
	for i, stream := range desc.Streams {
		fullname := fmt.Sprintf("/%s/%s", desc.ServiceName, stream.StreamName)
		d, ok := s.services[fullname]
		if !ok {
			d = &serviceDesc{
				index:    uint32(len(s.methods)),
				fullname: fullname,
			}
			s.services[fullname] = d
			s.methods = append(s.methods, d)
		}

		d.service = desc
		d.stream = &desc.Streams[i]
		d.impl = impl
	}
}

func (s *Server) Handle(ctx context.Context, req *Frame) error {
	sid := req.GetSid()
	res := Frame_builder{
		Sid: sid,
		Seq: 1,
	}.Build()
	if s.closed.Load() {
		res.SetCode(uint32(codes.Unavailable))
		s.tx.Handle(ctx, res)
		return nil
	}

	var desc *serviceDesc
	if i := int(req.GetMethodIndex()); i == 0 {
		desc = s.services[req.GetMethod()]
		if desc == nil {
			res.SetCode(uint32(codes.Unimplemented))
			return s.tx.Handle(ctx, res)
		}
	} else if len(s.methods) <= i {
		res.SetCode(uint32(codes.Unimplemented))
		return s.tx.Handle(ctx, res)
	} else {
		desc = s.methods[i]
	}

	fill_err := func(err error) {
		st, ok := status.FromError(err)
		if ok {
			res.SetCode(uint32(st.Code()))
			res.SetDesc(st.Message())
		} else {
			res.SetCode(uint32(codes.Unknown))
			res.SetDesc(err.Error())
		}
	}

	if desc.method != nil {
		// TODO: use codec.
		dec := func(v any) error {
			return proto.Unmarshal(req.GetPayload(), v.(proto.Message))
		}

		s.wg.Go(func() {
			defer s.tx.Handle(ctx, res)
			if s.drain.Load() {
				res.SetCode(uint32(codes.Unavailable))
				return
			}

			transport := serverTransportUnary{}
			ctx = grpc.NewContextWithServerTransportStream(ctx, &transport)
			ctx = mdIn(ctx, req)

			v, err := desc.method.Handler(desc.impl, ctx, dec, s.unary_int)
			res.SetHeader(newMd(transport.header))
			res.SetTrailer(newMd(transport.trailer))
			if err != nil {
				fill_err(err)
				return
			}

			payload, err := proto.Marshal(v.(proto.Message))
			if err != nil {
				fill_err(err)
				return
			}

			res.SetPayload(payload)
			res.SetCode(uint32(codes.OK))
		})
		return nil
	}

	stream, ok := s.getStream(sid, desc)
	if !ok {
		if s.drain.Load() {
			res.SetCode(uint32(codes.Unavailable))
			s.tx.Handle(ctx, res)
			return nil
		}

		stream.ctx = grpc.NewContextWithServerTransportStream(stream.ctx, serverTransportStream{stream})
		stream.ctx = mdIn(stream.ctx, req)

		s.wg.Go(func() {
			defer stream.Close()
			defer s.tx.Handle(ctx, res)
			if s.drain.Load() {
				res.SetCode(uint32(codes.Unavailable))
				return
			}

			var err error
			if s.stream_int != nil {
				info := grpc.StreamServerInfo{
					FullMethod:     desc.fullname,
					IsClientStream: desc.stream.ClientStreams,
					IsServerStream: desc.stream.ServerStreams,
				}
				err = s.stream_int(desc.impl, stream, &info, desc.stream.Handler)
			} else {
				err = desc.stream.Handler(desc.impl, stream)
			}

			res.SetSeq(stream.tx_seq.next())
			res.SetHeader(newMd(stream.header))
			res.SetTrailer(newMd(stream.trailer))
			if err != nil {
				fill_err(err)
				return
			}

			res.SetCode(uint32(codes.OK))
		})
	}

	stream.put(ctx, req)
	return nil
}

func (s *Server) getStream(sid uint32, desc *serviceDesc) (*serverStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.ss[sid]
	if !ok {
		stream = newServerStream(context.Background(), s, sid, desc)
		s.ss[sid] = stream
	}

	return stream, ok
}

// GracefulStop stops the dRPC server gracefully.
// It makes future call of Handle return an error and waits for existing calls to finish.
func (s *Server) GracefulStop() {
	s.drain.Store(true)
	s.wg.Wait()
	s.closed.Store(true)
}

func (s *Server) Stop() {

}

type serviceDesc struct {
	index    uint32
	fullname string

	service *grpc.ServiceDesc
	method  *grpc.MethodDesc
	stream  *grpc.StreamDesc

	impl any
}

type ServerOption interface {
	apply(*serverOption)
}

type serverOption struct {
	unary_int   grpc.UnaryServerInterceptor
	unary_ints  []grpc.UnaryServerInterceptor
	stream_int  grpc.StreamServerInterceptor
	stream_ints []grpc.StreamServerInterceptor
}

type serverOptionFunc func(*serverOption)

func (f serverOptionFunc) apply(o *serverOption) {
	f(o)
}

func UnaryInterceptor(i grpc.UnaryServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		if o.unary_int != nil {
			panic("The unary server interceptor was already set and may not be reset.")
		}
		o.unary_int = i
	})
}

func ChainUnaryInterceptors(is ...grpc.UnaryServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		o.unary_ints = append(o.unary_ints, is...)
	})
}

func chainUnaryServerInterceptors(is []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return is[0](ctx, req, info, getChainUnaryHandler(is, 0, info, handler))
	}
}

func getChainUnaryHandler(is []grpc.UnaryServerInterceptor, curr int, info *grpc.UnaryServerInfo, last grpc.UnaryHandler) grpc.UnaryHandler {
	if curr == len(is)-1 {
		return last
	}
	return func(ctx context.Context, req any) (any, error) {
		return is[curr+1](ctx, req, info, getChainUnaryHandler(is, curr+1, info, last))
	}
}

func StreamInterceptor(i grpc.StreamServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		if o.stream_int != nil {
			panic("The stream server interceptor was already set and may not be reset.")
		}
		o.stream_int = i
	})

}

func ChainStreamInterceptors(is ...grpc.StreamServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		o.stream_ints = append(o.stream_ints, is...)
	})
}

func chainStreamServerInterceptors(is []grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return is[0](srv, ss, info, getChainStreamHandler(is, 0, info, handler))
	}
}

func getChainStreamHandler(is []grpc.StreamServerInterceptor, curr int, info *grpc.StreamServerInfo, last grpc.StreamHandler) grpc.StreamHandler {
	if curr == len(is)-1 {
		return last
	}
	return func(srv any, stream grpc.ServerStream) error {
		return is[curr+1](srv, stream, info, getChainStreamHandler(is, curr+1, info, last))
	}
}
