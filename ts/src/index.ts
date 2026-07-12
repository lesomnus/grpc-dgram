// grpc-dgram TypeScript port: gRPC-style RPC over unreliable datagram
// channels, implementing the drpc wire protocol v1.0 (PROTOCOL.md). The
// WebRTC DataChannel adapter lives in the './webrtc' entry.

export { ClientStream, Conn, EndOfStreamError, type ConnOptions } from './conn'
export {
  bidiMethod,
  clientStreamingMethod,
  isUnary,
  serverStreamingMethod,
  unaryMethod,
  type BidiDesc,
  type CallOptions,
  type ClientStreamingDesc,
  type ForcedCodec,
  type MethodDesc,
  type NamedCodec,
  type PayloadCodec,
  type ServerStreamingDesc,
  type UnaryDesc,
} from './desc'
export { DropPolicy, type Limits, type RxBufferConfig } from './limits'
export { cloneMetadata, metadataJoin, type Metadata } from './metadata'
export { K_LOUD, RxVerdict, RxWindow, TxSeq, W_FWD } from './seq'
export {
  Server,
  type BidiHandler,
  type ClientStreamingHandler,
  type ServerContext,
  type ServerOptions,
  type ServerReader,
  type ServerStreamingHandler,
  type ServerWriter,
  type UnaryHandler,
} from './server'
export { abortCause, Code, isMessageTooLarge, MessageTooLargeError, StatusError, statusError, toStatusError } from './status'
export { type Timing } from './timing'
export { hasConnAttacher, hasTransportInfo, unpack, type ConnAttacher, type FrameContext, type FrameHandler, type TransportInfo } from './transport'
export {
  decodeEnvelop,
  decodeFrame,
  encodeEnvelop,
  encodeFrame,
  FlagClose,
  FlagOpen,
  FlagPing,
  FlagReset,
  frame,
  frameStatus,
  isClose,
  isData,
  isHalfClose,
  isHeaderFrame,
  isOpen,
  isPing,
  isReset,
  isTerminal,
  resetFor,
  setFrameError,
  type Frame,
} from './wire'
