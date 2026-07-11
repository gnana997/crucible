// The crucible wire frame codec — the one hand-written piece OpenAPI can't
// model. Specified language-neutrally in docs/wire.md; conformance fixtures
// live in sdks/fixtures and are exercised by test/frames.test.ts.

/** Frame type bytes. Frozen — they travel on the wire. */
export const FrameType = {
  stdout: 1,
  stderr: 2,
  exit: 3,
  stdin: 4,
  stdinClose: 5,
} as const;

export type FrameTypeName = keyof typeof FrameType;

/** Fixed header: 1 type byte + 3 reserved + uint32 big-endian payload length. */
export const FRAME_HEADER_SIZE = 8;

/** Max payload per frame; larger logical writes are chunked. */
export const MAX_PAYLOAD_SIZE = 64 * 1024;

export interface Frame {
  type: number;
  payload: Uint8Array;
}

/** encodeFrame serializes one frame. Throws if payload exceeds MAX_PAYLOAD_SIZE. */
export function encodeFrame(type: number, payload: Uint8Array): Uint8Array {
  if (payload.length > MAX_PAYLOAD_SIZE) {
    throw new RangeError(`frame payload ${payload.length} > MAX_PAYLOAD_SIZE ${MAX_PAYLOAD_SIZE}`);
  }
  const out = new Uint8Array(FRAME_HEADER_SIZE + payload.length);
  out[0] = type;
  new DataView(out.buffer).setUint32(4, payload.length, false); // big-endian
  out.set(payload, FRAME_HEADER_SIZE);
  return out;
}

/**
 * encodeChunked serializes one logical write as one or more frames of the
 * same type, splitting payloads larger than MAX_PAYLOAD_SIZE (the writer
 * half of the chunking rule — see exec_chunked.bin in the fixtures).
 */
export function encodeChunked(type: number, payload: Uint8Array): Uint8Array[] {
  const frames: Uint8Array[] = [];
  let off = 0;
  do {
    const end = Math.min(off + MAX_PAYLOAD_SIZE, payload.length);
    frames.push(encodeFrame(type, payload.subarray(off, end)));
    off = end;
  } while (off < payload.length);
  return frames;
}

/**
 * FrameDecoder is an incremental decoder: push() bytes as they arrive (from
 * a fetch body reader or WebSocket messages — chunk boundaries carry no
 * meaning), collect complete frames, and call end() at EOF so truncation
 * mid-frame is an error rather than silence.
 */
export class FrameDecoder {
  #buf = new Uint8Array(0);

  /** push appends bytes and returns every frame completed by them. */
  push(bytes: Uint8Array): Frame[] {
    if (this.#buf.length === 0) {
      this.#buf = bytes.slice();
    } else {
      const merged = new Uint8Array(this.#buf.length + bytes.length);
      merged.set(this.#buf);
      merged.set(bytes, this.#buf.length);
      this.#buf = merged;
    }
    const frames: Frame[] = [];
    for (;;) {
      if (this.#buf.length < FRAME_HEADER_SIZE) return frames;
      const view = new DataView(this.#buf.buffer, this.#buf.byteOffset);
      const size = view.getUint32(4, false);
      if (size > MAX_PAYLOAD_SIZE) {
        throw new RangeError(`frame size ${size} > MAX_PAYLOAD_SIZE ${MAX_PAYLOAD_SIZE}`);
      }
      if (this.#buf.length < FRAME_HEADER_SIZE + size) return frames;
      frames.push({
        type: this.#buf[0]!,
        payload: this.#buf.slice(FRAME_HEADER_SIZE, FRAME_HEADER_SIZE + size),
      });
      this.#buf = this.#buf.slice(FRAME_HEADER_SIZE + size);
    }
  }

  /** end asserts the stream finished on a frame boundary. */
  end(): void {
    if (this.#buf.length !== 0) {
      throw new RangeError(`stream truncated mid-frame (${this.#buf.length} trailing bytes)`);
    }
  }
}
