import './style.css';
import { createGame, playMove, undoMoves, chooseMove, serializeGame, restoreGame } from './game.js';
import { requestModelMove, normalizeEndpoint } from './ai.js';
import { createBoardScene } from './scene.js';

const paths = {
  grid: '<path d="M4 4h16v16H4zM4 9h16M4 15h16M9 4v16M15 4v16"/><circle cx="15" cy="9" r="2" fill="currentColor"/>',
  arrow: '<path d="M5 12h14m-5-5 5 5-5 5"/>',
  chevron: '<path d="m9 5 7 7-7 7"/>',
  spark: '<path d="m12 3 2.6 6.4L21 12l-6.4 2.6L12 21l-2.6-6.4L3 12l6.4-2.6zM20 2v4m-2-2h4"/>',
  people: '<circle cx="9" cy="8" r="3"/><path d="M3 21v-2a6 6 0 0 1 12 0v2M17 5a3 3 0 0 1 0 6m1 4a5 5 0 0 1 3 4v2"/>',
  undo: '<path d="m9 5-6 6 6 6M3 11h11a6 6 0 0 1 6 6v2"/>',
  refresh: '<path d="M20 8a8 8 0 1 0 0 8M20 3v5h-5"/>',
  save: '<path d="M5 3h12l4 4v14H3V3zM7 3v6h10V3M7 21v-8h10v8"/>',
  history: '<path d="M3 11a9 9 0 1 1 2 7M3 4v7h7m2-5v6l4 2"/>',
  settings: '<path d="M4 7h16M4 17h16"/><circle cx="9" cy="7" r="3" fill="currentColor"/><circle cx="15" cy="17" r="3" fill="currentColor"/>',
  sound: '<path d="m11 4-6 5H2v6h3l6 5zM15 8a6 6 0 0 1 0 8m3-11a10 10 0 0 1 0 14"/>',
  muted: '<path d="m11 4-6 5H2v6h3l6 5zM16 9l6 6m0-6-6 6"/>',
  view: '<path d="m12 3 10 6-10 6L2 9zM2 15l10 6 10-6"/>',
  expand: '<path d="M9 3H3v6m12-6h6v6M3 15v6h6m12-6v6h-6"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 6v6l4 2"/>',
  leaf: '<path d="M20 3C8 2 2 7 5 15s16 7 15-12ZM5 20 16 8"/>',
  close: '<path d="m6 6 12 12M18 6 6 18"/>',
  download: '<path d="M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5"/>',
  upload: '<path d="M12 16V4m-5 5 5-5 5 5M4 16v5h16v-5"/>',
  check: '<path d="m5 12 4 4L19 6"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v6m0-10v1"/>',
};
const icon = (name, cls = '') => `<svg class="icon ${cls}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths[name] || paths.grid}</svg>`;
const $ = selector => document.querySelector(selector);
const KEYS = { auto: 'yijing.game.v1', saves: 'yijing.saves.v1', settings: 'yijing.settings.v1', prefs: 'yijing.preferences.v1' };
let storageWarned = false;
function read(key, fallback) { try { return JSON.parse(localStorage.getItem(key)) ?? fallback; } catch { return fallback; } }
function write(key, value) {
  try { localStorage.setItem(key, JSON.stringify(value)); return true; }
  catch { if (!storageWarned) { storageWarned = true; toast('浏览器存储不可用，请导出棋局留存。'); } return false; }
}
function safeOptions(raw = {}) { raw = raw && typeof raw === 'object' ? raw : {}; return { mode: raw.mode === 'local' ? 'local' : 'ai', difficulty: ['easy', 'medium', 'hard'].includes(raw.difficulty) ? raw.difficulty : 'medium' }; }
const prefs = read(KEYS.prefs, {});
let options = safeOptions(prefs);
let sound = prefs?.sound !== false;
const storedConfig = read(KEYS.settings, {});
let config = { engine: storedConfig?.engine === 'remote' ? 'remote' : 'builtin', baseURL: typeof storedConfig?.baseURL === 'string' ? storedConfig.baseURL : '', model: typeof storedConfig?.model === 'string' ? storedConfig.model : '', apiKey: storedConfig?.rememberKey && typeof storedConfig?.apiKey === 'string' ? storedConfig.apiKey : '', rememberKey: Boolean(storedConfig?.rememberKey) };
let game = createGame();
let elapsed = 0;
let activeSaveId = null;
let restored = false;
const auto = read(KEYS.auto, null);
if (auto) {
  try { game = restoreGame(auto.game); options = safeOptions(auto.options); elapsed = Number.isFinite(auto.elapsed) ? Math.max(0, auto.elapsed) : 0; restored = game.moves.length > 0; }
  catch { /* A damaged autosave must not prevent a fresh game. */ }
}
let thinking = false;
let generation = 0;
let controller = null;
let scene;
let soundContext;

$('#app').innerHTML = `
  <header class="site-header">
    <a class="brand" href="./" aria-label="弈境首页"><span class="brand-mark">${icon('grid')}</span><span class="brand-name">弈境<span>Y I J I N G</span></span><span class="brand-divider"></span><span class="brand-description">一方棋盘，一刻专注</span></a>
    <nav aria-label="主导航"><button class="nav-button active" id="room-button">棋室<span class="nav-dot"></span></button><button class="nav-button" id="records-button">${icon('history')}棋局记录</button><button class="nav-settings" id="settings-button">${icon('settings')}<span>AI 设置</span></button></nav>
  </header>
  <main>
    <section class="intro"><div><div class="eyebrow"><span></span> A LITTLE SPACE TO SLOW DOWN</div><h1>落一子，<em>静一心。</em></h1><p>让思绪停一停，在黑白之间，找到你的节奏。</p></div><div class="intro-note"><span class="seal">弈</span><div>不急于输赢<br><span>享受每一步的可能</span></div></div></section>
    <section class="game-layout" aria-label="五子棋棋室">
      <aside class="game-sidebar">
        <div class="section-heading"><span>开始一场好棋</span><span class="small-cap">PLAY YOUR WAY</span></div>
        <div class="mode-switch" aria-label="对弈模式"><button data-mode="ai">${icon('spark')}人机对弈</button><button data-mode="local">${icon('people')}双人对弈</button></div>
        <div class="opponent-card"><div class="opponent-avatar">${icon('spark')}</div><div><strong id="opponent-title">山间棋友</strong><span id="opponent-description">内置 AI · 随时陪你下一局</span></div><span class="online-dot"></span></div>
        <div class="difficulty-section"><div class="field-heading"><span>棋友水平</span><span id="difficulty-note">刚刚好的挑战</span></div><div class="difficulty-switch" aria-label="AI 难度"><button data-difficulty="easy">入门</button><button data-difficulty="medium">进阶</button><button data-difficulty="hard">高手</button></div></div>
        <div class="sidebar-rule"></div>
        <div class="players"><div class="player-row" id="black-player"><span class="stone-avatar black"></span><div><strong id="black-label">你执黑</strong><span>先手 · 黑棋</span></div><span class="turn-tag">落子中</span></div><div class="player-line"></div><div class="player-row" id="white-player"><span class="stone-avatar white"></span><div><strong id="white-label">棋友执白</strong><span>后手 · 白棋</span></div><span class="turn-tag">落子中</span></div></div>
        <div class="sidebar-rule"></div>
        <button class="primary-button new-game" id="new-game">${icon('refresh')}开始新的一局${icon('arrow')}</button>
        <div class="secondary-actions"><button id="undo-button">${icon('undo')}悔棋</button><button id="save-button">${icon('save')}保存棋局</button></div>
        <button class="connect-card" id="connect-button"><span class="connect-icon">${icon('spark')}</span><span><strong>遇见你的 AI 棋友</strong><small id="connection-caption">接入模型，来点不一样的对弈</small></span>${icon('chevron')}</button>
        <div class="sidebar-footer">${icon('leaf')} 棋盘之上，只有此刻。</div>
      </aside>
      <div class="board-card" id="board-card">
        <div class="board-topbar"><div class="live-status" role="status" aria-live="polite"><span class="status-dot"></span><span id="status-text">轮到你了，落下第一子吧</span></div><span class="board-format">自由五子棋 <span>15 × 15</span></span></div>
        <div id="board-scene" class="board-scene"></div>
        <div class="scene-caption"><span class="scene-caption-line"></span> 方寸之间，自有天地</div>
        <div class="board-tools"><button id="view-button" class="tool-button" aria-label="切换俯视视角" title="切换俯视视角">${icon('view')}</button><button id="reset-view" class="tool-button" aria-label="重置视角" title="重置视角">${icon('refresh')}</button><span></span><button id="sound-button" class="tool-button" aria-label="关闭落子音效" title="落子音效">${icon('sound')}</button><button id="fullscreen-button" class="tool-button" aria-label="全屏棋盘" title="全屏棋盘">${icon('expand')}</button></div>
        <div class="board-bottombar"><div><span class="move-counter"><strong id="move-count">00</strong> 手</span><span class="bottom-divider"></span><span class="game-clock">${icon('clock')}<span id="game-time">00:00</span></span></div><span class="autosave"><span></span><span id="autosave-text">棋局自动保存</span></span></div>
        <div class="game-result" id="game-result" hidden><span class="result-eyebrow">这一局，落定</span><h2 id="result-title"></h2><p id="result-description"></p><button class="primary-button" id="play-again">再来一局 ${icon('arrow')}</button><button class="text-button" id="dismiss-result">欣赏棋局</button></div>
      </div>
    </section>
    <section class="below-board"><div class="interaction-hints"><span><span class="hint-icon">↖</span>点击交叉点落子</span><span>${icon('view')}拖动棋盘自由观察</span><span><span class="five-dots">●●●●●</span>五子连线，即为胜局</span></div><button id="rules-button">对弈小贴士 ${icon('info')}</button></section>
    <footer class="site-footer"><span>慢一点，也是一种进步。</span><span>MADE FOR A MOMENT OF CALM <span class="footer-flower">✳</span></span></footer>
  </main>
  <div id="toast" class="toast" role="status" aria-live="polite"></div>
  <dialog id="settings-dialog"><div class="modal-content"><div class="modal-heading"><div><span class="eyebrow">YOUR AI COMPANION</span><h2>认识一位新棋友</h2></div><button class="icon-button" data-close aria-label="关闭设置">${icon('close')}</button></div><p class="modal-intro">选一位内置棋友，或让你喜欢的 AI 模型执白。</p>
    <form id="settings-form"><div class="engine-options"><label><input type="radio" name="engine" value="builtin" /><span>${icon('leaf')}内置棋友<small>离线可玩，无需配置</small></span></label><label><input type="radio" name="engine" value="remote" /><span>${icon('spark')}外部 AI 模型<small>OpenAI 兼容接口</small></span></label></div><fieldset id="remote-fields"><label class="form-label">API 地址<input id="api-url" type="url" placeholder="https://api.example.com/v1" autocomplete="url" /><small>支持基础地址或完整的 /chat/completions 地址</small></label><label class="form-label">模型名称<input id="api-model" placeholder="例如 gpt-4o-mini 或 deepseek-chat" autocomplete="off" /></label><label class="form-label">API Key <span>可选，取决于服务商</span><input id="api-key" type="password" placeholder="输入你的 API Key" autocomplete="off" /></label><label class="checkbox-label"><input id="remember-key" type="checkbox" />在此浏览器记住密钥</label><p class="privacy-note">默认仅在当前页面内存中使用密钥。勾选后密钥保存在本机浏览器，请仅在私人设备上使用。请求直接发送至你填写的服务商，该接口需允许浏览器跨域访问。</p></fieldset><p class="form-error" id="settings-error" role="alert"></p><div class="modal-actions"><button type="button" class="plain-button" data-close>稍后再说</button><button class="primary-button" type="submit">${icon('check')}保存设置</button></div></form></div></dialog>
  <dialog id="records-dialog"><div class="modal-content"><div class="modal-heading"><div><span class="eyebrow">EVERY MOVE MATTERS</span><h2>留住每一局</h2></div><button class="icon-button" data-close aria-label="关闭棋局记录">${icon('close')}</button></div><p class="modal-intro">棋局保存在当前浏览器，也可以导出随身带走。</p><div id="saved-games" class="saved-games"></div><div class="modal-actions records-actions"><button class="plain-button" id="import-button">${icon('upload')}导入棋局</button><button class="primary-button" id="export-button">${icon('download')}导出当前棋局</button></div><input type="file" id="import-file" accept=".json,application/json" hidden /></div></dialog>
  <dialog id="confirm-dialog"><div class="modal-content compact-modal"><div class="modal-heading"><h2 id="confirm-title">开启新的一局？</h2><button class="icon-button" data-close aria-label="取消">${icon('close')}</button></div><p id="confirm-description" class="modal-intro"></p><div class="modal-actions"><button class="plain-button" data-close>继续这局</button><button class="primary-button" id="confirm-action">继续</button></div></div></dialog>
  <dialog id="rules-dialog"><div class="modal-content"><div class="modal-heading"><div><span class="eyebrow">A SIMPLE PLEASURE</span><h2>从第一子开始</h2></div><button class="icon-button" data-close aria-label="关闭规则">${icon('close')}</button></div><div class="rules-list"><p><b>01</b><span><strong>黑先白后，交替落子</strong>在 15 × 15 棋盘的空交叉点落子，落子后不能移动。</span></p><p><b>02</b><span><strong>五子相连，就赢了</strong>横、竖或斜向连续五子及以上获胜。本棋室采用自由规则，无禁手；满盘无胜者为和棋。</span></p><p><b>03</b><span><strong>找到舒服的角度</strong>点击落子，拖动旋转。工具栏可以切换俯视、重置视角或开启全屏。键盘方向键选点，回车或空格落子。</span></p><p><b>04</b><span><strong>随时停下，也随时回来</strong>棋局自动保存；“保存棋局”可留下最多 30 局。人机悔棋回退一轮，双人悔棋回退一步。外部 AI 不可用时由内置棋友接续。</span></p></div></div></dialog>
`;

let toastTimer;
function toast(message) {
  const el = $('#toast');
  if (!el) return;
  el.textContent = message;
  el.classList.add('visible');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('visible'), 4500);
}
function playSound() {
  if (!sound) return;
  try {
    soundContext ??= new (window.AudioContext || window.webkitAudioContext)();
    if (soundContext.state === 'suspended') void soundContext.resume().catch(() => {});
    const osc = soundContext.createOscillator();
    const gain = soundContext.createGain();
    osc.type = 'triangle';
    osc.frequency.setValueAtTime(720, soundContext.currentTime);
    osc.frequency.exponentialRampToValueAtTime(170, soundContext.currentTime + 0.055);
    gain.gain.setValueAtTime(0.09, soundContext.currentTime);
    gain.gain.exponentialRampToValueAtTime(0.001, soundContext.currentTime + 0.09);
    osc.connect(gain).connect(soundContext.destination);
    osc.start();
    osc.stop(soundContext.currentTime + 0.1);
  } catch { /* Audio is optional on browsers without Web Audio. */ }
}
function snapshot() { return { format: 'yijing-gomoku', version: 1, game: serializeGame(game), options: { ...options }, elapsed }; }
function autosave() {
  const ok = write(KEYS.auto, snapshot());
  $('#autosave-text').textContent = ok ? '棋局自动保存' : '自动保存不可用';
  $('.autosave').classList.toggle('unavailable', !ok);
}
function savePreferences() { write(KEYS.prefs, { ...options, sound }); }
function clockText() { const s = Math.floor(elapsed); return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`; }
function canHumanPlay() { return !thinking && !game.winner && !game.draw && (options.mode === 'local' || game.currentPlayer === 1); }
function render(animate = true) {
  document.querySelectorAll('[data-mode]').forEach(btn => { const active = btn.dataset.mode === options.mode; btn.classList.toggle('selected', active); btn.setAttribute('aria-pressed', active); });
  document.querySelectorAll('[data-difficulty]').forEach(btn => { const active = btn.dataset.difficulty === options.difficulty; btn.classList.toggle('selected', active); btn.setAttribute('aria-pressed', active); btn.disabled = options.mode === 'local' || config.engine === 'remote'; });
  $('#difficulty-note').textContent = options.mode === 'local' ? '与身边的人一起' : config.engine === 'remote' ? '由模型自主思考' : { easy: '轻松入门，慢慢来', medium: '刚刚好的挑战', hard: '认真过招，步步为营' }[options.difficulty];
  $('#opponent-title').textContent = options.mode === 'local' ? '面对面的棋友' : config.engine === 'remote' ? config.model || 'AI 模型棋友' : '山间棋友';
  $('#opponent-description').textContent = options.mode === 'local' ? '同一设备 · 共享一方棋盘' : config.engine === 'remote' ? '外部模型 · 执白与你对弈' : '内置 AI · 随时陪你下一局';
  $('#black-label').textContent = options.mode === 'local' ? '黑方棋友' : '你执黑';
  $('#white-label').textContent = options.mode === 'local' ? '白方棋友' : '棋友执白';
  $('#black-player').classList.toggle('current', !game.winner && !game.draw && game.currentPlayer === 1);
  $('#white-player').classList.toggle('current', !game.winner && !game.draw && game.currentPlayer === 2);
  $('#white-player .turn-tag').textContent = thinking ? '思考中' : '落子中';
  $('#move-count').textContent = String(game.moves.length).padStart(2, '0');
  $('#game-time').textContent = clockText();
  $('#undo-button').disabled = !game.moves.length;
  $('#save-button').disabled = !game.moves.length;
  $('#sound-button').innerHTML = icon(sound ? 'sound' : 'muted');
  $('#sound-button').setAttribute('aria-label', sound ? '关闭落子音效' : '开启落子音效');
  $('#sound-button').setAttribute('aria-pressed', sound);
  $('#connection-caption').textContent = config.engine === 'remote' ? '已接入模型 · 点击管理设置' : '接入模型，来点不一样的对弈';
  let status;
  if (game.winner) status = `${game.winner === 1 ? '黑' : '白'}棋胜出，精彩的一局`;
  else if (game.draw) status = '棋盘已满，旗鼓相当的一局';
  else if (thinking) status = '棋友正在思考，请稍候…';
  else if (options.mode === 'local') status = `轮到${game.currentPlayer === 1 ? '黑' : '白'}方落子`;
  else status = game.moves.length ? '轮到你了，从容落子' : '轮到你了，落下第一子吧';
  $('#status-text').textContent = status;
  $('.live-status').classList.toggle('thinking', thinking);
  scene?.sync(game, canHumanPlay(), animate);
}
function cancelAI() { generation++; controller?.abort(); controller = null; thinking = false; }
function finishMove() {
  playSound();
  render();
  autosave();
  if (game.winner || game.draw) {
    const saved = saveGame(true);
    $('#result-title').textContent = game.draw ? '棋逢对手。' : options.mode === 'ai' ? game.winner === 1 ? '这一局，你赢了。' : '好棋，下次再会。' : `${game.winner === 1 ? '黑' : '白'}方胜出。`;
    $('#result-description').textContent = (game.draw ? '没有输赢，也是很好的结局。' : `历经 ${game.moves.length} 手，${game.winner === 1 ? '黑' : '白'}棋连成一线。`) + (saved ? '这一局已为你珍藏。' : '可导出棋局留念。');
    $('#game-result').hidden = false;
  }
}
function onPlace(row, col) {
  if (!canHumanPlay()) return;
  if (playMove(game, row, col)) { finishMove(); scheduleAI(); }
}
async function scheduleAI() {
  if (options.mode !== 'ai' || game.currentPlayer !== 2 || game.winner || game.draw || thinking) return;
  thinking = true;
  const ownGeneration = ++generation;
  controller = new AbortController();
  const signal = controller.signal;
  render(false);
  await new Promise(resolve => setTimeout(resolve, 420));
  if (ownGeneration !== generation) return;
  let move;
  try {
    if (config.engine === 'remote') move = await requestModelMove(game, config, { signal });
    else move = chooseMove(game, options.difficulty);
  } catch (error) {
    if (ownGeneration !== generation || signal.aborted) return;
    toast(`${error.message} 本手由内置棋友接续。`);
    move = chooseMove(game, options.difficulty);
  }
  if (ownGeneration !== generation) return;
  thinking = false;
  controller = null;
  if (move && playMove(game, move.row, move.col)) finishMove(); else render(false);
}
function resetGame(nextOptions = options) {
  cancelAI();
  game = createGame();
  options = { ...nextOptions };
  elapsed = 0;
  activeSaveId = null;
  $('#game-result').hidden = true;
  savePreferences();
  autosave();
  render(false);
}
let confirmedAction = null;
function confirmAction(title, description, action) {
  $('#confirm-title').textContent = title;
  $('#confirm-description').textContent = description;
  confirmedAction = action;
  $('#confirm-dialog').showModal();
}
function newGame(nextOptions = options) {
  if (game.moves.length && !game.winner && !game.draw) confirmAction('开启新的一局？', '当前自动存档将被替换。想留住这一局，可以先关闭弹窗并点击“保存棋局”。', () => resetGame(nextOptions));
  else resetGame(nextOptions);
}
function getSaves() { const data = read(KEYS.saves, []); return Array.isArray(data) ? data.filter(x => x && typeof x.id === 'string' && typeof x.timestamp === 'number' && x.game).slice(0, 30) : []; }
function saveGame(quiet = false) {
  if (!game.moves.length) return;
  const entries = getSaves();
  activeSaveId ??= typeof crypto.randomUUID === 'function' ? crypto.randomUUID() : `game-${Date.now()}-${Array.from(crypto.getRandomValues(new Uint32Array(2)), n => n.toString(16)).join('')}`;
  const entry = { ...snapshot(), id: activeSaveId, timestamp: Date.now() };
  const saved = write(KEYS.saves, [entry, ...entries.filter(x => x.id !== activeSaveId)].slice(0, 30));
  if (!quiet) toast(saved ? '这一局，已经为你留好了。' : '保存失败，可以导出棋局。');
  return saved;
}
function loadSnapshot(entry) {
  const nextGame = restoreGame(entry.game);
  cancelAI();
  game = nextGame;
  options = safeOptions(entry.options);
  elapsed = Number.isFinite(entry.elapsed) ? Math.max(0, Math.min(entry.elapsed, 31536000)) : 0;
  activeSaveId = entry.id || null;
  $('#game-result').hidden = true;
  savePreferences();
  autosave();
  render(false);
  scheduleAI();
  toast('棋局已恢复，接着下一手。');
}
function showRecords() {
  const list = $('#saved-games');
  list.replaceChildren();
  const entries = getSaves();
  if (!entries.length) list.innerHTML = `<div class="empty-state"><span>${icon('history')}</span><h3>好棋，值得留念</h3><p>落下第一子，然后点击“保存棋局”。<br>属于你的对弈故事，会留在这里。</p></div>`;
  entries.forEach(entry => {
    let saved;
    try { saved = restoreGame(entry.game); } catch { return; }
    const row = document.createElement('button');
    row.className = 'saved-game';
    const title = document.createElement('strong');
    title.textContent = `${entry.options?.mode === 'local' ? '双人对弈' : '与 AI 棋友'} · ${saved.moves.length} 手`;
    const detail = document.createElement('small');
    detail.textContent = `${new Date(entry.timestamp).toLocaleString('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })} · ${saved.winner ? `${saved.winner === 1 ? '黑' : '白'}方胜` : saved.draw ? '和棋' : '未完待续'}`;
    const text = document.createElement('span');
    text.append(title, detail);
    row.innerHTML = `<span class="record-icon">${icon('grid')}</span>`;
    row.append(text);
    row.insertAdjacentHTML('beforeend', icon('arrow'));
    row.addEventListener('click', () => {
      $('#records-dialog').close();
      if (game.moves.length && !game.winner && !game.draw) confirmAction('回到这场对弈？', '当前自动存档将被所选棋局替换，手动保存的棋局会保留。', () => loadSnapshot(entry));
      else loadSnapshot(entry);
    });
    list.append(row);
  });
  $('#records-dialog').showModal();
}
function showSettings() {
  $(`input[name="engine"][value="${config.engine}"]`).checked = true;
  $('#api-url').value = config.baseURL;
  $('#api-model').value = config.model;
  $('#api-key').value = config.apiKey;
  $('#remember-key').checked = config.rememberKey;
  $('#remote-fields').disabled = config.engine !== 'remote';
  $('#settings-error').textContent = '';
  $('#settings-dialog').showModal();
}
document.querySelectorAll('[data-close]').forEach(btn => btn.addEventListener('click', () => btn.closest('dialog').close()));
document.querySelectorAll('dialog').forEach(dialog => dialog.addEventListener('click', event => { if (event.target === dialog) dialog.close(); }));
document.querySelectorAll('[data-mode]').forEach(btn => btn.addEventListener('click', () => { if (options.mode !== btn.dataset.mode) newGame({ ...options, mode: btn.dataset.mode }); }));
document.querySelectorAll('[data-difficulty]').forEach(btn => btn.addEventListener('click', () => { options.difficulty = btn.dataset.difficulty; savePreferences(); autosave(); render(false); }));
$('#new-game').addEventListener('click', () => newGame());
$('#play-again').addEventListener('click', () => resetGame());
$('#dismiss-result').addEventListener('click', () => { $('#game-result').hidden = true; });
$('#confirm-action').addEventListener('click', () => { $('#confirm-dialog').close(); confirmedAction?.(); confirmedAction = null; });
$('#undo-button').addEventListener('click', () => {
  cancelAI();
  const last = game.moves.at(-1);
  undoMoves(game, options.mode === 'ai' && last?.player === 2 ? 2 : 1);
  $('#game-result').hidden = true;
  autosave();
  render(false);
  scheduleAI();
});
$('#save-button').addEventListener('click', () => saveGame());
$('#records-button').addEventListener('click', showRecords);
$('#room-button').addEventListener('click', () => $('#board-card').scrollIntoView({ behavior: 'smooth', block: 'center' }));
$('#settings-button').addEventListener('click', showSettings);
$('#connect-button').addEventListener('click', showSettings);
$('#rules-button').addEventListener('click', () => $('#rules-dialog').showModal());
$('#sound-button').addEventListener('click', () => { sound = !sound; savePreferences(); render(false); if (sound) playSound(); });
$('#view-button').addEventListener('click', () => { const top = scene?.toggleView(); $('#view-button').classList.toggle('selected', top); $('#view-button').setAttribute('aria-pressed', Boolean(top)); });
$('#reset-view').addEventListener('click', () => { scene?.resetView(); $('#view-button').classList.remove('selected'); $('#view-button').setAttribute('aria-pressed', false); });
$('#fullscreen-button').addEventListener('click', async () => {
  try { if (document.fullscreenElement) await document.exitFullscreen(); else await $('#board-card').requestFullscreen(); }
  catch { toast('当前浏览器不支持全屏，仍可正常对弈。'); }
});
document.querySelectorAll('input[name="engine"]').forEach(radio => radio.addEventListener('change', () => { $('#remote-fields').disabled = radio.value !== 'remote'; }));
$('#settings-form').addEventListener('submit', event => {
  event.preventDefault();
  const next = { engine: $('input[name="engine"]:checked').value, baseURL: $('#api-url').value.trim(), model: $('#api-model').value.trim(), apiKey: $('#api-key').value.trim(), rememberKey: $('#remember-key').checked };
  if (next.engine === 'remote') {
    try { normalizeEndpoint(next.baseURL); if (!next.model) throw new Error('请填写模型名称。'); }
    catch (error) { $('#settings-error').textContent = error.message; return; }
  }
  const saved = write(KEYS.settings, { ...next, apiKey: next.rememberKey ? next.apiKey : '' });
  config = next;
  cancelAI();
  $('#settings-dialog').close();
  toast(saved ? '棋友设置已保存，下次落子时生效。' : '设置已应用，浏览器存储不可用。');
  render(false);
  scheduleAI();
});
$('#export-button').addEventListener('click', () => {
  const url = URL.createObjectURL(new Blob([JSON.stringify(snapshot(), null, 2)], { type: 'application/json' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = `弈境-${new Date().toISOString().slice(0, 10)}-${game.moves.length}手.json`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  toast('棋局已导出，可在其他浏览器导入。');
});
$('#import-button').addEventListener('click', () => $('#import-file').click());
$('#import-file').addEventListener('change', async event => {
  const file = event.target.files[0];
  if (!file) return;
  try {
    if (file.size > 100000) throw new Error('棋局文件过大，请选择弈境导出的 JSON 文件。');
    const data = JSON.parse(await file.text());
    if (data.format !== 'yijing-gomoku' || data.version !== 1) throw new Error('不是有效的弈境棋局文件。');
    restoreGame(data.game);
    $('#records-dialog').close();
    if (game.moves.length && !game.winner && !game.draw) confirmAction('载入这份棋局？', '当前自动存档将被导入的棋局替换，手动保存的棋局会保留。', () => loadSnapshot(data));
    else loadSnapshot(data);
  } catch { toast('无法导入：请检查文件是否为完整、合法的弈境棋局。'); }
  finally { event.target.value = ''; }
});
function renderError() {
  $('#board-scene').innerHTML = `<div class="render-error">${icon('info')}<h3>棋盘暂时无法呈现</h3><p>请使用支持 WebGL 的现代浏览器，并开启硬件加速。</p><button class="primary-button" id="reload-renderer">重新加载</button></div>`;
  $('#reload-renderer').addEventListener('click', () => location.reload());
}
try { scene = createBoardScene($('#board-scene'), onPlace); }
catch (error) { console.error('3D rendering unavailable:', error.message); renderError(); }
$('#board-scene').addEventListener('rendererror', renderError);
render(false);
if (restored) toast('欢迎回来，上次的棋局还在。');
scheduleAI();
let lastTick = Date.now();
let ticks = 0;
setInterval(() => {
  const now = Date.now();
  if (game.moves.length && !game.winner && !game.draw && !document.hidden) elapsed += Math.min((now - lastTick) / 1000, 5);
  lastTick = now;
  $('#game-time').textContent = clockText();
  if (++ticks % 15 === 0 && game.moves.length) autosave();
}, 1000);
window.addEventListener('pagehide', autosave);
