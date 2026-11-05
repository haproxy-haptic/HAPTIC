import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { setImmediate } from 'node:timers/promises';
import { brotliCompressSync } from 'node:zlib';
import test from 'node:test';
import initializeBrotli from './brotli-dec-wasm-2.3.2/brotli_dec_wasm.js';
import { fetchBrotliWasm } from './playground-wasm-loader.mjs';

await initializeBrotli({ module_or_path: await readFile(new URL('./brotli-dec-wasm-2.3.2/brotli_dec_wasm_bg.wasm', import.meta.url)) });
const decoded = Buffer.from('verified playground engine');
const compressed = brotliCompressSync(decoded);
const identity = bytes => ({ size: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') });
const expected = { compressed: identity(compressed), decoded: identity(decoded) };

function clock(t) {
  let now = 0;
  let sequence = 0;
  const timers = new Map();
  t.mock.method(globalThis, 'setTimeout', (callback, milliseconds) => {
    timers.set(++sequence, { callback, at: now + milliseconds });
    return sequence;
  });
  t.mock.method(globalThis, 'clearTimeout', timer => timers.delete(timer));
  return {
    timers,
    async advance(milliseconds) {
      await setImmediate();
      now += milliseconds;
      for (const [id, timer] of timers) {
        if (timer.at <= now) { timers.delete(id); timer.callback(); }
      }
      await setImmediate();
    },
  };
}

function stalledResponse(signal, partial) {
  return new Response(new ReadableStream({ start(controller) {
    if (partial) controller.enqueue(compressed.subarray(0, 2));
    signal.addEventListener('abort', () => controller.error(signal.reason), { once: true });
  } }));
}

for (const stage of ['headers', 'body']) {
  test(`a stalled ${stage} request restarts with an empty verified download`, async t => {
    const time = clock(t);
    let attempts = 0;
    const resets = [];
    t.mock.method(globalThis, 'fetch', async (url, { signal }) => {
      attempts++;
      if (attempts > 1) return new Response(compressed);
      if (stage === 'body') return stalledResponse(signal, true);
      return new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
    });
    const result = fetchBrotliWasm('/engine.br', expected, progress => resets.push(progress.loaded));
    await time.advance(5000);
    assert.deepEqual(Buffer.from(await result), decoded);
    assert.equal(attempts, 2);
    assert.equal(resets.filter(loaded => loaded === 0).length, 2);
    assert.equal(time.timers.size, 0);
  });
}

test('three idle attempts fail and release all download timers', async t => {
  const time = clock(t);
  let attempts = 0;
  t.mock.method(globalThis, 'fetch', async (url, { signal }) => {
    attempts++;
    return stalledResponse(signal, true);
  });
  const rejected = assert.rejects(fetchBrotliWasm('/engine.br', expected, () => {}), /download stalled/);
  for (let attempt = 0; attempt < 3; attempt++) await time.advance(5000);
  await rejected;
  assert.equal(attempts, 3);
  assert.equal(time.timers.size, 0);
});

test('network progress extends the idle deadline without restarting a slow active transfer', async t => {
  const time = clock(t);
  let stream;
  let attempts = 0;
  t.mock.method(globalThis, 'fetch', async () => {
    attempts++;
    return new Response(new ReadableStream({ start(controller) { stream = controller; } }));
  });
  const result = fetchBrotliWasm('/engine.br', expected, () => {});
  await time.advance(4000);
  stream.enqueue(compressed.subarray(0, 2));
  await time.advance(4000);
  stream.enqueue(compressed.subarray(2));
  stream.close();
  assert.deepEqual(Buffer.from(await result), decoded);
  assert.equal(attempts, 1);
  assert.equal(time.timers.size, 0);
});

for (const failure of ['truncated', 'oversized', 'compressed checksum', 'decoded checksum', 'HTTP 404']) {
  test(`${failure} fails immediately without a retry`, async t => {
    const time = clock(t);
    let attempts = 0;
    const wanted = structuredClone(expected);
    if (failure === 'compressed checksum') wanted.compressed.sha256 = '0'.repeat(64);
    if (failure === 'decoded checksum') wanted.decoded.sha256 = '0'.repeat(64);
    t.mock.method(globalThis, 'fetch', async () => {
      attempts++;
      if (failure === 'HTTP 404') return new Response('', { status: 404 });
      if (failure === 'truncated') return new Response(compressed.subarray(0, -1));
      if (failure === 'oversized') return new Response(Buffer.concat([compressed, Buffer.from([0])]));
      return new Response(compressed);
    });
    await assert.rejects(fetchBrotliWasm('/engine.br', wanted, () => {}), /incomplete|exceeds|checksum mismatch|HTTP 404/);
    assert.equal(attempts, 1);
    assert.equal(time.timers.size, 0);
  });
}
