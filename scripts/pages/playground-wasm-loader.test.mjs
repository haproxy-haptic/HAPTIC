import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { setImmediate } from 'node:timers/promises';
import { brotliCompressSync, constants } from 'node:zlib';
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

function partialResponse(bytes, start, end) {
  return new Response(bytes.subarray(start, end + 1), {
    status: 206,
    headers: { 'Content-Range': `bytes ${start}-${end}/${bytes.length}` },
  });
}

for (const stage of ['headers', 'body']) {
  test(`a stalled ${stage} request resumes from its received byte count`, async t => {
    const time = clock(t);
    const requests = [];
    const start = stage === 'body' ? 2 : 0;
    t.mock.method(globalThis, 'fetch', async (url, { signal, headers }) => {
      requests.push(headers.Range);
      if (headers.Range) return partialResponse(compressed, start, compressed.length - 1);
      if (stage === 'body') return stalledResponse(signal, true);
      return new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
    });
    const result = fetchBrotliWasm('/engine.br', expected, () => {});
    await time.advance(5000);
    assert.deepEqual(Buffer.from(await result), decoded);
    assert.deepEqual(requests, [undefined, `bytes=${start}-${compressed.length - 1}`]);
    assert.equal(time.timers.size, 0);
  });
}

test('recovery uses contiguous bounded ranges through the end of a large engine', async t => {
  const time = clock(t);
  const rangeSize = 1024 * 1024;
  const engine = randomBytes(2 * rangeSize);
  const asset = brotliCompressSync(engine, { params: { [constants.BROTLI_PARAM_QUALITY]: 0 } });
  assert.ok(asset.length > 2 * rangeSize);
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, { signal, headers }) => {
    requests.push(headers.Range);
    if (!headers.Range) return stalledResponse(signal, false);
    const [, start, end] = /^bytes=(\d+)-(\d+)$/.exec(headers.Range);
    return partialResponse(asset, Number(start), Number(end));
  });
  const result = fetchBrotliWasm('/engine.br', { compressed: identity(asset), decoded: identity(engine) }, () => {});
  await time.advance(5000);
  assert.deepEqual(Buffer.from(await result), engine);
  assert.deepEqual(requests, [undefined, `bytes=0-${rangeSize - 1}`,
    `bytes=${rangeSize}-${2 * rangeSize - 1}`, `bytes=${2 * rangeSize}-${asset.length - 1}`]);
});

test('a server ignoring the range replaces the partial download with its full 200 response', async t => {
  const time = clock(t);
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, { signal, headers }) => {
    requests.push(headers.Range);
    return headers.Range ? new Response(compressed) : stalledResponse(signal, true);
  });
  const result = fetchBrotliWasm('/engine.br', expected, () => {});
  await time.advance(5000);
  assert.deepEqual(Buffer.from(await result), decoded);
  assert.deepEqual(requests, [undefined, `bytes=2-${compressed.length - 1}`]);
});

for (const failure of ['missing range', 'wrong start', 'wrong end', 'wrong total', 'truncated', 'oversized', 'corrupt']) {
  test(`an invalid recovery response fails immediately: ${failure}`, async t => {
    const time = clock(t);
    let requests = 0;
    t.mock.method(globalThis, 'fetch', async (url, { signal, headers }) => {
      requests++;
      if (!headers.Range) return stalledResponse(signal, true);
      const start = failure === 'wrong start' ? 0 : 2;
      const end = compressed.length - (failure === 'wrong end' ? 2 : 1);
      const size = compressed.length + (failure === 'wrong total' ? 1 : 0);
      const body = Buffer.from(compressed.subarray(2));
      if (failure === 'corrupt') body[0] ^= 1;
      const response = new Response(failure === 'truncated' ? body.subarray(1)
        : failure === 'oversized' ? Buffer.concat([body, Buffer.alloc(1)]) : body, { status: 206 });
      if (failure !== 'missing range') response.headers.set('Content-Range', `bytes ${start}-${end}/${size}`);
      return response;
    });
    const rejected = assert.rejects(fetchBrotliWasm('/engine.br', expected, () => {}),
      /unexpected byte range|incomplete|exceeds|checksum mismatch/);
    await time.advance(5000);
    await rejected;
    assert.equal(requests, 2);
    assert.equal(time.timers.size, 0);
  });
}

test('rejecting a range before reading it cancels its open response body', async t => {
  const time = clock(t);
  let cancelled = false;
  t.mock.method(globalThis, 'fetch', async (url, { signal, headers }) => {
    if (!headers.Range) return stalledResponse(signal, true);
    return new Response(new ReadableStream({ cancel() { cancelled = true; } }), { status: 206 });
  });
  const rejected = assert.rejects(fetchBrotliWasm('/engine.br', expected, () => {}), /unexpected byte range/);
  await time.advance(5000);
  await rejected;
  assert.ok(cancelled);
  assert.equal(time.timers.size, 0);
});

test('receiving the expected last byte without EOF still requires a completed response', async t => {
  const time = clock(t);
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, { signal, headers }) => {
    requests.push(headers.Range);
    if (headers.Range) return partialResponse(compressed, 0, compressed.length - 1);
    return new Response(new ReadableStream({ start(controller) {
      controller.enqueue(compressed);
      signal.addEventListener('abort', () => controller.error(signal.reason), { once: true });
    } }));
  });
  const result = fetchBrotliWasm('/engine.br', expected, () => {});
  await time.advance(5000);
  assert.deepEqual(Buffer.from(await result), decoded);
  assert.deepEqual(requests, [undefined, `bytes=0-${compressed.length - 1}`]);
});

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
