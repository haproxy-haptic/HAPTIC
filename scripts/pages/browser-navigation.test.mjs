import assert from 'node:assert/strict';
import test from 'node:test';
import { navigate } from './browser-navigation.mjs';

function fixture(goto) {
  const frame = {};
  let observer;
  return {
    page: { on: (event, listener) => { observer = listener; },
      off: (event, listener) => { assert.equal(listener, observer); observer = null; },
      mainFrame: () => frame,
      goto: async (url, options) => {
        assert.deepEqual(options, { waitUntil: 'load', timeout: 30000 });
        return goto();
      } },
    response: (navigation = true, main = true) => ({ status: () => 503,
      request: () => ({ isNavigationRequest: () => navigation }), frame: () => main ? frame : {} }),
    emit(response) { observer(response); },
    listening: () => observer !== null,
  };
}

test('an HTTP failure returns without waiting for an error-page load event', async () => {
  const f = fixture(() => new Promise(() => {}));
  const response = f.response();
  const result = navigate(f.page, 'https://example.test/');
  f.emit(response);
  assert.equal(await result, response);
  assert.ok(!f.listening());
});

test('successful navigation keeps the full load gate and ignores unrelated HTTP errors', async () => {
  let complete;
  const f = fixture(() => new Promise(resolve => { complete = resolve; }));
  let settled = false;
  const result = navigate(f.page, 'https://example.test/').then(response => { settled = true; return response; });
  f.emit(f.response(false));
  f.emit(f.response(true, false));
  await Promise.resolve();
  assert.ok(!settled);
  const response = { status: () => 200 };
  complete(response);
  assert.equal(await result, response);
  assert.ok(!f.listening());
});

test('navigation failure keeps the original error and removes its response observer', async () => {
  const failure = new Error('load timeout');
  const f = fixture(() => Promise.reject(failure));
  await assert.rejects(navigate(f.page, 'https://example.test/'), error => error === failure);
  assert.ok(!f.listening());
});
