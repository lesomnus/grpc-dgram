// grpc-dgram TypeScript port: gRPC-style RPC over unreliable datagram
// channels, implementing the drpc wire protocol v1.1 (PROTOCOL.md). Each
// transport adapter lives in its own './transport/*' entry.

export {
  ClientStream,
  Conn,
  EndOfStreamError,
  statusDetails,
  type ConnOptions,
  type DetailedStatusError,
  type FlowTiming,
} from './conn'
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
export {
  cloneMetadata,
  decodeBase64,
  encodeBase64,
  isBinaryKey,
  metadataJoin,
  validateMetadata,
  validateMetadataPair,
  type Metadata,
} from './metadata'
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
export {
  DEFAULT_MAX_RECV_MSG_SIZE,
  DEFAULT_MAX_SEND_MSG_SIZE,
  DEFAULT_STALL_MS,
  getCompressor,
  W_INIT,
  type Compressor,
} from './util'
export { hasConnAttacher, hasTransportInfo, unpack, type ConnAttacher, type FrameContext, type FrameHandler, type TransportInfo } from './seam'
export {
  decodeEnvelop,
  decodeFrame,
  encodeEnvelop,
  encodeFrame,
  FlagClose,
  FlagCompressed,
  FlagOpen,
  FlagPing,
  FlagReset,
  FlagWindow,
  frame,
  frameStatus,
  isClose,
  isData,
  isHalfClose,
  isHeaderFrame,
  isCompressed,
  isOpen,
  isPing,
  isReset,
  isTerminal,
  isWindow,
  hasUnknownFlags,
  legalShape,
  resetFor,
  setFrameError,
  shapeOf,
  SHAPE_MASK,
  type Any,
  type Frame,
} from './wire'
