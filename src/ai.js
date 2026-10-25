const TIMEOUT_MS = 20_000;

class ModelError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ModelError';
  }
}

/** Normalize an OpenAI-compatible base URL without sending credentials in URLs. */
export function normalizeEndpoint(baseURL) {
  if (typeof baseURL !== 'string' || !baseURL.trim()) {
    throw new ModelError('请填写 AI 服务地址。');
  }
  let url;
  try {
    url = new URL(baseURL.trim());
  } catch {
    throw new ModelError('AI 服务地址无效，请填写完整的 HTTPS 地址。');
  }
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && local)) {
    throw new ModelError('AI 服务需要 HTTPS；本机 localhost 服务可使用 HTTP。');
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new ModelError('服务地址请勿包含账号、密码、查询参数或片段。');
  }
  const path = url.pathname.replace(/\/+$/, '');
  url.pathname = path.endsWith('/chat/completions')
    ? path
    : `${path || '/v1'}/chat/completions`;
  return url.toString();
}

function abortError() {
  const error = new Error('已取消 AI 请求。');
  error.name = 'AbortError';
  return error;
}

function parseMove(content, board) {
  if (typeof content !== 'string' || !content.trim()) {
    throw new ModelError('模型未返回落子位置，请重试或使用内置 AI。');
  }
  let move;
  const cleaned = content.trim().replace(/^```(?:json)?\s*/i, '').replace(/\s*```$/, '');
  try {
    move = JSON.parse(cleaned);
  } catch {
    // Accept a short explanatory prefix, but never pick arbitrarily among multiple moves.
    const candidates = cleaned.match(/\{[^{}]*\}/g);
    if (!candidates || candidates.length !== 1) {
      throw new ModelError('模型返回格式错误，需要 JSON 格式的 row 和 col。');
    }
    try {
      move = JSON.parse(candidates[0]);
    } catch {
      throw new ModelError('模型返回格式错误，需要 JSON 格式的 row 和 col。');
    }
  }
  if (!move || typeof move !== 'object' || Array.isArray(move)
    || !Number.isInteger(move.row) || !Number.isInteger(move.col)
    || move.row < 0 || move.row >= 15 || move.col < 0 || move.col >= 15) {
    throw new ModelError('模型给出了无效坐标，row 和 col 必须是 0–14 的整数。');
  }
  if (board[move.row][move.col] !== 0) {
    throw new ModelError('模型选择的位置已有棋子，请重试或使用内置 AI。');
  }
  return { row: move.row, col: move.col };
}

/** Request one legal move. API keys are kept in memory and never logged here. */
export async function requestModelMove(game, config, { signal } = {}) {
  if (signal?.aborted) throw abortError();
  const endpoint = normalizeEndpoint(config?.baseURL);
  if (typeof config?.model !== 'string' || !config.model.trim()) {
    throw new ModelError('请填写模型名称。');
  }
  if (!Array.isArray(game?.board) || game.board.length !== 15
    || !game.board.every(row => Array.isArray(row) && row.length === 15
      && row.every(cell => cell === 0 || cell === 1 || cell === 2))
    || ![1, 2].includes(game.currentPlayer)) {
    throw new ModelError('棋盘数据无效，无法请求 AI 落子。');
  }
  if (game.winner || game.draw || !game.board.some(row => row.includes(0))) {
    throw new ModelError('本局已结束，请开始新棋局。');
  }

  const controller = new AbortController();
  let timedOut = false;
  const onAbort = () => controller.abort();
  signal?.addEventListener('abort', onAbort, { once: true });
  const timeout = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, TIMEOUT_MS);

  const headers = { 'Content-Type': 'application/json' };
  if (typeof config.apiKey === 'string' && config.apiKey.trim()) {
    headers.Authorization = `Bearer ${config.apiKey.trim()}`;
  }

  try {
    const response = await fetch(endpoint, {
      method: 'POST',
      headers,
      signal: controller.signal,
      body: JSON.stringify({
        model: config.model.trim(),
        temperature: 0.2,
        messages: [
          {
            role: 'system',
            content: 'You are a strong Gomoku player. The board is 15 by 15. '
              + 'Freestyle rules: five or more consecutive stones horizontally, vertically, '
              + 'or diagonally win; there are no forbidden moves. '
              + 'Coordinates are zero-based row and col, each from 0 to 14. '
              + '0 means empty, 1 means black, 2 means white. '
              + 'Win immediately if possible, otherwise block an immediate opponent win. '
              + 'Choose exactly one EMPTY intersection. Return ONLY valid JSON '
              + 'with integer coordinates: {"row":7,"col":7}.',
          },
          {
            role: 'user',
            content: `Your stone is ${game.currentPlayer}. It is your turn.\n`
              + `Board rows 0–14, columns 0–14:\n${JSON.stringify(game.board)}`,
          },
        ],
      }),
    });
    if (!response.ok) {
      const messages = {
        401: 'AI 服务认证失败，请检查 API Key。',
        403: 'AI 服务拒绝访问，请检查密钥权限和服务地址。',
        404: '未找到模型或接口，请检查服务地址和模型名称。',
        429: 'AI 服务请求过于频繁或额度不足，请稍后重试。',
      };
      throw new ModelError(messages[response.status]
        || `AI 服务暂时不可用（HTTP ${response.status}），请稍后重试。`);
    }
    let result;
    try {
      result = await response.json();
    } catch (error) {
      if (controller.signal.aborted) throw error;
      throw new ModelError('AI 服务返回了无法解析的响应。');
    }
    if (controller.signal.aborted) throw abortError();
    const content = result?.choices?.[0]?.message?.content;
    return parseMove(content, game.board);
  } catch (error) {
    if (signal?.aborted) throw abortError();
    if (timedOut) throw new ModelError('AI 响应超时（20 秒），请重试或使用内置 AI。');
    if (error instanceof ModelError) throw error;
    throw new ModelError('无法连接 AI 服务，请检查地址、网络及服务端 CORS 设置。');
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener('abort', onAbort);
  }
}
