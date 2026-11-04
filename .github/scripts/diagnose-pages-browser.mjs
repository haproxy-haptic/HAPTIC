import * as playwright from '../../scripts/visual-qa/node_modules/playwright/index.mjs';
import { appendFileSync, readFileSync } from 'node:fs';

function record(event) {
  const line = JSON.stringify({ time: new Date().toISOString(), ...event });
  appendFileSync('browser-diagnostic.jsonl', line + '\n');
  console.log(line);
}

function memory() {
  try { return readFileSync('/sys/fs/cgroup/memory.current', 'utf8').trim(); }
  catch { return null; }
}

for (const name of ['chromium', 'firefox']) {
  const launch = playwright[name].launch.bind(playwright[name]);
  playwright[name].launch = async (...options) => {
    const browser = await launch(...options);
    record({ browser: name, type: 'launched', version: browser.version() });
    const newContext = browser.newContext.bind(browser);
    browser.newContext = async (...args) => {
      const context = await newContext(...args);
      const pending = new Map();
      context.on('request', request => pending.set(request, { url: request.url(), start: Date.now() }));
      context.on('requestfinished', request => {
        const requestStarted = pending.get(request);
        record({ browser: name, type: 'finished', ...requestStarted, duration: Date.now() - requestStarted.start });
        pending.delete(request);
      });
      context.on('requestfailed', request => {
        record({ browser: name, type: 'requestfailed', url: request.url(), failure: request.failure() });
        pending.delete(request);
      });
      context.on('response', response => {
        if (response.status() >= 400) record({ browser: name, type: 'response', url: response.url(), status: response.status() });
      });
      context.on('page', page => {
        page.on('pageerror', error => record({ browser: name, type: 'pageerror', message: error.message }));
        page.on('console', message => record({ browser: name, type: 'console', level: message.type(), message: message.text() }));
        page.on('worker', worker => record({ browser: name, type: 'worker', url: worker.url() }));
        const wait = page.waitForFunction.bind(page);
        page.waitForFunction = async (...parameters) => {
          try { return await wait(...parameters); }
          catch (error) {
            const state = await page.evaluate(() => ({ url: location.href,
              chip: document.querySelector('#chip')?.textContent,
              stats: window.hapticPlayground?.stats(), body: document.body.innerText.slice(-3000) }));
            record({ browser: name, type: 'waitFailure', error: error.message, state, memory: memory(),
              pending: [...pending.values()].map(request => ({ ...request, duration: Date.now() - request.start })) });
            await page.screenshot({ path: `browser-diagnostic-${name}.png` });
            throw error;
          }
        };
      });
      const close = context.close.bind(context);
      context.close = async (...options) => {
        record({ browser: name, type: 'contextClosing', memory: memory(), pending: [...pending.values()] });
        return close(...options);
      };
      return context;
    };
    return browser;
  };
}
