// gRPC status codes and the error model of the port.
//
// Go carries statuses as *status.Status errors; here every call failure
// surfaces as a StatusError. toStatusError mirrors Go's toStatusErr
// (stream.go): context errors map to their canonical codes and an adapter's
// synchronous size refusal maps to RESOURCE_EXHAUSTED (PROTOCOL.md §4.4).

export enum Code {
  OK = 0,
  CANCELLED = 1,
  UNKNOWN = 2,
  INVALID_ARGUMENT = 3,
  DEADLINE_EXCEEDED = 4,
  NOT_FOUND = 5,
  ALREADY_EXISTS = 6,
  PERMISSION_DENIED = 7,
  RESOURCE_EXHAUSTED = 8,
  FAILED_PRECONDITION = 9,
  ABORTED = 10,
  OUT_OF_RANGE = 11,
  UNIMPLEMENTED = 12,
  INTERNAL = 13,
  UNAVAILABLE = 14,
  DATA_LOSS = 15,
  UNAUTHENTICATED = 16,
}

export class StatusError extends Error {
  readonly code: Code
  readonly desc: string

  constructor(code: Code, desc: string) {
    super(`${Code[code] ?? code}: ${desc}`)
    this.name = 'StatusError'
    this.code = code
    this.desc = desc
  }
}

export function statusError(code: Code, desc: string): StatusError {
  return new StatusError(code, desc)
}

// MessageTooLargeError is thrown (or set as a `cause`) by an adapter's
// handle when a marshaled envelop cannot fit the transport's message limit.
// The core never fragments; it maps this to RESOURCE_EXHAUSTED on the owning
// call (PROTOCOL.md §4.4). It also reclaims the refused frame's seq — the
// refusal MUST be synchronous in the sense that no later frame of the stream
// has been sent yet.
export class MessageTooLargeError extends Error {
  constructor(message = 'message too large for the transport') {
    super(message)
    this.name = 'MessageTooLargeError'
  }
}

// isMessageTooLarge walks the `cause` chain so adapters may wrap the error
// with their own context (the Go adapters wrap ErrMessageTooLarge the same
// way).
export function isMessageTooLarge(err: unknown): boolean {
  for (let e = err, i = 0; i < 8; i++) {
    if (e instanceof MessageTooLargeError) return true
    if (e instanceof Error && e.cause !== undefined) e = e.cause
    else return false
  }
  return false
}

// toStatusError maps an arbitrary thrown value to a StatusError.
export function toStatusError(err: unknown): StatusError {
  if (err instanceof StatusError) return err
  if (isMessageTooLarge(err)) {
    return new StatusError(Code.RESOURCE_EXHAUSTED, err instanceof Error ? err.message : String(err))
  }
  if (err instanceof Error) {
    // DOMException names used by AbortSignal.abort() / AbortSignal.timeout().
    if (err.name === 'AbortError') return new StatusError(Code.CANCELLED, err.message || 'aborted')
    if (err.name === 'TimeoutError') return new StatusError(Code.DEADLINE_EXCEEDED, err.message || 'deadline exceeded')
    return new StatusError(Code.UNKNOWN, err.message)
  }
  return new StatusError(Code.UNKNOWN, String(err))
}

// abortCause returns the status error describing why an aborted signal ended,
// preferring the abort reason when it is already a status error — the
// equivalent of Go's ctxErr (context.Cause + toStatusErr).
export function abortCause(signal: AbortSignal): StatusError {
  return toStatusError(signal.reason)
}
