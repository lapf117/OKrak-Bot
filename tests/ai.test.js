import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizeEndpoint, requestModelMove } from '../src/ai.js';

const makeGame = () => ({ board: Array.from({ length: 15 }, () => Array(15).fill(0)), currentPlayer: 2 });
const config = { baseURL: 'https://example.com/v1', model: 'test-model', apiKey: 'test-key' };
const response = (content, status = 200) => new Response(JSON.stringify({ choices: [{ message: { content } }] }), { status });

async function withFetch(mock, run) {
  const original = globalThis.fetch;
  globalThis.fetch = mock;
  try { return await run(); } finally { globalThis.fetch = original; }
}

test('normalizes base URLs and preserves complete compatible endpoints', () => {
  assert.equal(normalizeEndpoint('https://example.com'), 'https://example.com/v1/chat/completions');
  assert.equal(normalizeEndpoint('https://example.com/v1/'), 'https://example.com/v1/chat/completions');
  assert.equal(normalizeEndpoint('https://example.com/custom/chat/completions/'), 'https://example.com/custom/chat/completions');
  assert.equal(normalizeEndpoint('http://localhost:11434/v1'), 'http://localhost:11434/v1/chat/completions');
  assert.equal(normalizeEndpoint('http://[::1]:8080/v1'), 'http://[::1]:8080/v1/chat/completions');
});

test('rejects insecure remote URLs and URL credentials', () => {
  for (const value of ['', 'nope', 'http://example.com/v1', 'ftp://localhost/v1', 'https://user:pass@example.com', 'https://example.com?key=secret', 'https://example.com/#x']) {
    assert.throws(() => normalizeEndpoint(value));
  }
});

test('sends model, bearer auth and position, then returns a legal move', async () => {
  const game = makeGame();
  game.board[7][7] = 1;
  await withFetch(async (url, options) => {
    assert.equal(url, 'https://example.com/v1/chat/completions');
    assert.equal(options.method, 'POST');
    assert.equal(options.headers.Authorization, 'Bearer test-key');
    const body = JSON.parse(options.body);
    assert.equal(body.model, 'test-model');
    assert.equal(body.response_format, undefined);
    assert.match(body.messages[1].content, /Your stone is 2/);
    assert.ok(body.messages[1].content.includes(JSON.stringify(game.board)));
    return response('{"row":7,"col":8}');
  }, async () => assert.deepEqual(await requestModelMove(game, config), { row: 7, col: 8 }));
});

test('accepts fenced or single embedded JSON and optional local API keys', async () => {
  for (const content of ['```json\n{"row":3,"col":4}\n```', 'My move: {"row":3,"col":4}']) {
    await withFetch(async (_url, options) => {
      assert.equal(options.headers.Authorization, undefined);
      return response(content);
    }, async () => assert.deepEqual(await requestModelMove(makeGame(), { ...config, apiKey: '' }), { row: 3, col: 4 }));
  }
});

test('rejects occupied, out-of-range, fractional and malformed model moves', async () => {
  const game = makeGame();
  game.board[4][4] = 1;
  for (const content of ['{"row":4,"col":4}', '{"row":15,"col":0}', '{"row":-1,"col":0}', '{"row":1.5,"col":0}', '{"row":"3","col":0}', 'null', '[1,2]', 'I cannot play', '{"row":1,"col":2} or {"row":2,"col":3}']) {
    await withFetch(async () => response(content), async () => assert.rejects(requestModelMove(game, config)));
  }
});

test('reports useful errors without returning service response bodies', async () => {
  for (const [status, pattern] of [[401, /认证失败/], [403, /拒绝访问/], [404, /未找到/], [429, /额度不足/], [500, /HTTP 500/]]) {
    await withFetch(async () => new Response('sensitive provider response', { status }), async () => {
      await assert.rejects(requestModelMove(makeGame(), config), error => pattern.test(error.message) && !error.message.includes('sensitive'));
    });
  }
  await withFetch(async () => { throw new TypeError('fetch failed'); }, async () => assert.rejects(requestModelMove(makeGame(), config), /CORS/));
  await withFetch(async () => new Response('not json'), async () => assert.rejects(requestModelMove(makeGame(), config), /无法解析/));
});

test('pre-cancelled requests do not fetch', async () => {
  const controller = new AbortController();
  controller.abort();
  await withFetch(async () => assert.fail('fetch should not be called'), async () => {
    await assert.rejects(requestModelMove(makeGame(), config, { signal: controller.signal }), { name: 'AbortError' });
  });
});

test('caller cancellation aborts an in-flight request', async () => {
  const controller = new AbortController();
  await withFetch((_url, options) => new Promise((_resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
  }), async () => {
    const pending = requestModelMove(makeGame(), config, { signal: controller.signal });
    controller.abort();
    await assert.rejects(pending, { name: 'AbortError' });
  });
});

test('invalid configuration and completed games fail before fetching', async () => {
  await withFetch(async () => assert.fail('fetch should not be called'), async () => {
    await assert.rejects(requestModelMove(makeGame(), { ...config, model: '' }), /模型名称/);
    await assert.rejects(requestModelMove({ ...makeGame(), winner: 1 }, config), /已结束/);
    await assert.rejects(requestModelMove({ board: [], currentPlayer: 2 }, config), /棋盘数据/);
  });
});
