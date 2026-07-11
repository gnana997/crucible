// Conformance tests for the frame codec, driven by the recorded fixtures in
// sdks/fixtures — the four-step recipe from docs/wire.md. No daemon needed.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import {
  FRAME_HEADER_SIZE,
  MAX_PAYLOAD_SIZE,
  FrameDecoder,
  encodeFrame,
  type Frame,
} from "../src/frames.ts";

const fixturesDir = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "fixtures");

interface ManifestFrame {
  type: string;
  type_byte: number;
  payload_len: number;
  payload_sha256: string;
  payload_utf8?: string;
  exec_result?: Record<string, unknown>;
}
interface ManifestFixture {
  file: string;
  invalid?: boolean;
  frames?: ManifestFrame[];
}
interface Manifest {
  header: { size: number };
  max_payload_size: number;
  frame_types: Record<string, number>;
  fixtures: ManifestFixture[];
}

const manifest: Manifest = JSON.parse(readFileSync(join(fixturesDir, "manifest.json"), "utf8"));

function decodeAll(data: Uint8Array): Frame[] {
  const dec = new FrameDecoder();
  // Feed in awkward chunk sizes to prove boundaries carry no meaning.
  const frames: Frame[] = [];
  for (let off = 0; off < data.length; off += 3) {
    frames.push(...dec.push(data.subarray(off, Math.min(off + 3, data.length))));
  }
  dec.end();
  return frames;
}

test("manifest constants match the codec", () => {
  assert.equal(manifest.header.size, FRAME_HEADER_SIZE);
  assert.equal(manifest.max_payload_size, MAX_PAYLOAD_SIZE);
});

for (const fx of manifest.fixtures) {
  const data = new Uint8Array(readFileSync(join(fixturesDir, fx.file)));

  if (fx.invalid) {
    test(`${fx.file}: decoding must fail`, () => {
      assert.throws(() => decodeAll(data));
    });
    continue;
  }

  test(`${fx.file}: decodes to the manifest frames`, () => {
    const frames = decodeAll(data);
    const want = fx.frames ?? [];
    assert.equal(frames.length, want.length);
    frames.forEach((f, i) => {
      const w = want[i]!;
      assert.equal(f.type, w.type_byte, `frame ${i} type`);
      assert.equal(f.payload.length, w.payload_len, `frame ${i} payload length`);
      const sum = createHash("sha256").update(f.payload).digest("hex");
      assert.equal(sum, w.payload_sha256, `frame ${i} payload sha256`);
      if (w.payload_utf8 !== undefined) {
        assert.equal(new TextDecoder().decode(f.payload), w.payload_utf8, `frame ${i} payload text`);
      }
      if (w.type === "exit") {
        const res = JSON.parse(new TextDecoder().decode(f.payload)) as Record<string, unknown>;
        assert.deepEqual(res, w.exec_result, `frame ${i} exec result`);
      }
    });
  });
}

test("stdin_session.bin: encoder reproduces the recorded bytes", () => {
  const fx = manifest.fixtures.find((f) => f.file === "stdin_session.bin")!;
  const want = new Uint8Array(readFileSync(join(fixturesDir, fx.file)));
  const parts = (fx.frames ?? []).map((w) =>
    encodeFrame(w.type_byte, new TextEncoder().encode(w.payload_utf8 ?? "")),
  );
  const got = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0;
  for (const p of parts) {
    got.set(p, off);
    off += p.length;
  }
  assert.deepEqual(got, want);
});
