package drpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
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

	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.ss[sid]
	if ok {
		if req.GetCode() == uint32(codes.Canceled) {
			stream.close()
		}
		return stream.put(ctx, req)
	}

	res_err := func(code codes.Code, msg string, args ...any) error {
		c := uint32(code)
		res := Frame_builder{
			Sid:  sid,
			Seq:  1,
			Code: &c,
			Desc: fmt.Sprintf(msg, args...),
		}
		return s.tx.Handle(ctx, res.Build())
	}
	if s.closed.Load() {
		return res_err(codes.Unavailable, "server is closed")
	}
	if s.drain.Load() {
		return res_err(codes.Unavailable, "server is draining")
	}

	var desc *serviceDesc
	if i := req.GetMethodIndex(); i > 0 {
		if len(s.methods) <= int(i) {
			return res_err(codes.Unimplemented, "method not found: %d", i)
		} else {
			desc = s.methods[i]
		}
	} else if desc = s.services[req.GetMethod()]; desc == nil {
		return res_err(codes.Unimplemented, "method not found")
	}

	codec := req.getCodec()
	if codec == nil {
		return res_err(codes.Unimplemented, "unsupported codec: %s", req.GetCodec())
	}

	stream = s.newStream(sid, desc)
	stream.codec = codec
	stream.rx <- req

	if desc.IsUnary() {
		dec := func(v any) error {
			buf := mem.SliceBuffer(req.GetPayload())
			return codec.Unmarshal(mem.BufferSlice{buf}, v)
		}

		s.wg.Go(func() {
			res := Frame_builder{Sid: sid, Seq: 1}.Build()

			defer stream.close()
			if s.drain.Load() {
				res.SetCode(uint32(codes.Unavailable))
				s.tx.Handle(stream.ctx, res)
				return
			}

			transport := serverTransportUnary{}
			stream.ctx = grpc.NewContextWithServerTransportStream(stream.ctx, &transport)
			stream.ctx = newIncomingContext(stream.ctx, req)

			v, err := desc.method.Handler(desc.impl, stream.ctx, dec, s.unary_int)
			if stream.ctx.Err() != nil {
				// Client abort the request, so we can just return without sending response.
				return
			}

			defer s.tx.Handle(stream.ctx, res)
			if transport.header != nil {
				res.SetHeader(newMd(transport.header))
			}
			if transport.trailer != nil {
				res.SetTrailer(newMd(transport.trailer))
			}
			if err != nil {
				res.setError(err)
				return
			}

			buf, err := codec.Marshal(v)
			if err != nil {
				res.setError(err)
				return
			}
			defer buf.Free()

			res.SetPayload(buf.Materialize())
			res.SetCode(uint32(codes.OK))
		})
	} else {
		s.wg.Go(func() {
			res := Frame_builder{Sid: sid, Seq: 1}.Build()

			tx := stream.tx
			defer stream.Close()
			defer tx.Handle(stream.ctx, res)
			if s.drain.Load() {
				res.SetCode(uint32(codes.Unavailable))
				return
			}

			transport := serverTransportStream{stream}
			stream.ctx = grpc.NewContextWithServerTransportStream(stream.ctx, transport)
			stream.ctx = newIncomingContext(stream.ctx, req)

			if !desc.stream.ServerStreams {
				// In client-streaming RPC, SendMsg is called before the stream handler
				// returns so keep it and send it with the trailer.
				// TODO: It would be nice if we have some flag to send header immediately
				// without waiting for the first SendMsg in this case.
				stream.tx = FrameHandlerFunc(func(ctx context.Context, f *Frame) error {
					res.SetPayload(f.GetPayload())
					res.SetHeader(f.GetHeader())
					return nil
				})
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
			if stream.header != nil {
				res.SetHeader(newMd(stream.header))
			}
			if stream.trailer != nil {
				res.SetTrailer(newMd(stream.trailer))
			}
			if err != nil {
				res.setError(err)
				return
			}

			res.SetCode(uint32(codes.OK))
		})
	}
	return nil
}

func (s *Server) newStream(sid uint32, desc *serviceDesc) (stream *serverStream) {
	stream = newServerStream(context.Background(), s, sid, desc)
	s.ss[sid] = stream
	return
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

func (d *serviceDesc) IsUnary() bool {
	return d.method != nil
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
