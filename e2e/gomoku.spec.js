import { test, expect } from '@playwright/test';
import { readFile } from 'node:fs/promises';

const APP_URL = 'http://127.0.0.1:5173';
const STORAGE = {
  auto: 'yijing.game.v1',
  saves: 'yijing.saves.v1',
  settings: 'yijing.settings.v1',
};
const fixture = (moves, options = { mode: 'local', difficulty: 'medium' }) => ({
  format: 'yijing-gomoku', version: 1,
  game: { version: 1, moves }, options, elapsed: 42,
});
const filePayload = data => ({
  name: 'gomoku-test.json', mimeType: 'application/json',
  buffer: Buffer.from(JSON.stringify(data)),
});

test('3D Gomoku: gameplay, saves, model integration and mobile layout', async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  // Exercise the same save path used by browsers on a plain HTTP LAN origin.
  await page.addInitScript(() => {
    Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true });
  });
  await page.setViewportSize({ width: 1440, height: 1050 });
  await page.goto(APP_URL);
  const canvas = page.locator('#board-scene canvas');
  const moveCount = page.locator('#move-count');
  const savedValue = key => page.evaluate(name => JSON.parse(localStorage.getItem(name)), key);
  const savedMoves = async () => {
    const snapshot = await savedValue(STORAGE.auto);
    const game = typeof snapshot.game === 'string' ? JSON.parse(snapshot.game) : snapshot.game;
    return game.moves;
  };
  async function importSnapshot(data, confirm = false) {
    await page.locator('#records-button').click();
    await page.locator('#import-file').setInputFiles(filePayload(data));
    if (confirm) {
      await expect(page.locator('#confirm-dialog')).toBeVisible();
      await page.locator('#confirm-action').click();
    }
    await expect(page.locator('#records-dialog')).not.toBeVisible();
  }

  await test.step('render a real WebGL board and play against the built-in AI', async () => {
    await expect(canvas).toBeVisible({ timeout: 45_000 });
    await expect(page.locator('.render-error')).toHaveCount(0);
    await expect(moveCount).toHaveText('00');
    await canvas.press('Enter');
    await expect(moveCount).toHaveText('02', { timeout: 20_000 });
    expect((await savedMoves())[0]).toEqual({ row: 7, col: 7, player: 1 });
    expect((await savedMoves())[1].player).toBe(2);
    await expect(page.locator('#status-text')).toContainText('轮到你了');
    await page.locator('#undo-button').click();
    await expect(moveCount).toHaveText('00');
    await expect(page.locator('#undo-button')).toBeDisabled();
    expect(await savedMoves()).toEqual([]);
  });

  await test.step('save without randomUUID and restore the autosave after reload', async () => {
    await canvas.press('Enter');
    await expect(moveCount).toHaveText('02', { timeout: 20_000 });
    const beforeReload = await savedMoves();
    await page.locator('#save-button').click();
    const saved = await savedValue(STORAGE.saves);
    expect(saved).toHaveLength(1);
    expect(saved[0].id).toMatch(/^game-/);
    expect(JSON.parse(saved[0].game).moves).toEqual(beforeReload);
    await page.reload();
    await expect(canvas).toBeVisible({ timeout: 45_000 });
    await expect(moveCount).toHaveText('02', { timeout: 20_000 });
    expect(await savedMoves()).toEqual(beforeReload);
    await page.locator('#records-button').click();
    await expect(page.locator('.saved-game')).toHaveCount(1);
    await expect(page.locator('.saved-game')).toContainText('2 手');
    await page.locator('#records-dialog [data-close]').click();
  });

  await test.step('switch to two players and complete five in a row', async () => {
    await page.locator('[data-mode="local"]').click();
    await expect(page.locator('#confirm-dialog')).toBeVisible();
    await page.locator('#confirm-action').click();
    await expect(moveCount).toHaveText('00');
    await expect(page.locator('[data-mode="local"]')).toHaveAttribute('aria-pressed', 'true');
    const nearWin = [];
    for (let i = 0; i < 4; i++) {
      nearWin.push({ row: 7, col: 3 + i, player: 1 });
      nearWin.push({ row: 0, col: i * 2, player: 2 });
    }
    await importSnapshot(fixture(nearWin));
    await expect(moveCount).toHaveText('08');
    await canvas.press('Enter'); // The keyboard cursor starts at the center, 7,7.
    await expect(moveCount).toHaveText('09');
    await expect(page.locator('#result-title')).toHaveText('黑方胜出。');
    await expect(page.locator('#status-text')).toContainText('黑棋胜出');
    await expect(page.locator('#result-description')).toContainText('已为你珍藏');
    await page.locator('#dismiss-result').click();
    await canvas.press('ArrowDown');
    await canvas.press('Enter');
    await expect(moveCount).toHaveText('09'); // Finished games reject further moves.
    await page.screenshot({ path: testInfo.outputPath('desktop-winning-game.png'), fullPage: true });
  });

  await test.step('export a replayable JSON document and reject a corrupt import', async () => {
    await page.locator('#records-button').click();
    const downloadPromise = page.waitForEvent('download');
    await page.locator('#export-button').click();
    const download = await downloadPromise;
    const exported = JSON.parse(await readFile(await download.path(), 'utf8'));
    expect(exported.format).toBe('yijing-gomoku');
    expect(exported.version).toBe(1);
    expect(exported.options.mode).toBe('local');
    expect(JSON.parse(exported.game).moves).toHaveLength(9);
    expect(exported).not.toHaveProperty('apiKey');
    const invalid = fixture([
      { row: 7, col: 7, player: 1 }, { row: 7, col: 7, player: 2 },
    ]);
    await page.locator('#import-file').setInputFiles(filePayload(invalid));
    await expect(page.locator('#toast')).toContainText('无法导入');
    await expect(moveCount).toHaveText('09');
    await page.locator('#records-dialog [data-close]').click();
  });

  let modelRequests = 0;
  const modelBodies = [];
  const fakeKey = 'test-only-not-a-real-api-key';
  await page.route('https://model.example/v1/chat/completions', async route => {
    const request = route.request();
    const headers = {
      'access-control-allow-origin': '*',
      'access-control-allow-methods': 'POST, OPTIONS',
      'access-control-allow-headers': 'authorization, content-type',
    };
    if (request.method() === 'OPTIONS') {
      await route.fulfill({ status: 204, headers });
      return;
    }
    modelRequests++;
    modelBodies.push(request.postDataJSON());
    expect(request.headers().authorization).toBe(`Bearer ${fakeKey}`);
    const move = modelRequests === 1 ? { row: 7, col: 8 } : { row: 7, col: 7 };
    await route.fulfill({
      status: 200, contentType: 'application/json', headers,
      body: JSON.stringify({ choices: [{ message: { content: JSON.stringify(move) } }] }),
    });
  });

  await test.step('save model preferences, keep the key private and play a mocked model', async () => {
    await page.locator('#settings-button').click();
    await page.locator('.engine-options label').nth(1).click();
    await page.locator('#api-url').fill('https://model.example/v1');
    await page.locator('#api-model').fill('test-gomoku-model');
    await page.locator('#api-key').fill(fakeKey);
    await expect(page.locator('#remember-key')).not.toBeChecked();
    await page.locator('#settings-form button[type="submit"]').click();
    let settings = await savedValue(STORAGE.settings);
    expect(settings.model).toBe('test-gomoku-model');
    expect(settings.apiKey).toBe('');
    expect(settings.rememberKey).toBe(false);

    await page.locator('[data-mode="ai"]').click();
    await expect(moveCount).toHaveText('00');
    // Previous winner inspection moved the cursor down once; return to center.
    await canvas.press('ArrowUp');
    await canvas.press('Enter');
    await expect(moveCount).toHaveText('02', { timeout: 20_000 });
    expect(modelRequests).toBe(1);
    expect(modelBodies[0].model).toBe('test-gomoku-model');
    expect((await savedMoves())[1]).toEqual({ row: 7, col: 8, player: 2 });

    await page.locator('#settings-button').click();
    await expect(page.locator('#api-key')).toHaveValue(fakeKey);
    await page.locator('#remember-key').check();
    await page.locator('#settings-form button[type="submit"]').click();
    settings = await savedValue(STORAGE.settings);
    expect(settings.apiKey).toBe(fakeKey);
    expect(settings.rememberKey).toBe(true);
    await page.locator('#settings-button').click();
    await page.locator('#remember-key').uncheck();
    await page.locator('#settings-form button[type="submit"]').click();
    expect((await savedValue(STORAGE.settings)).apiKey).toBe('');
  });

  await test.step('reject an occupied model move and continue with the built-in AI', async () => {
    await canvas.press('ArrowRight');
    await canvas.press('ArrowRight');
    await canvas.press('Enter');
    await expect(moveCount).toHaveText('04', { timeout: 20_000 });
    expect(modelRequests).toBe(2);
    await expect(page.locator('#toast')).toContainText('本手由内置棋友接续');
    const moves = await savedMoves();
    expect(new Set(moves.map(move => `${move.row},${move.col}`)).size).toBe(4);
    expect(moves[3].player).toBe(2);
    await expect(page.locator('#status-text')).toContainText('轮到你了');
  });

  await test.step('tolerate missing imported options and fit a mobile viewport', async () => {
    await importSnapshot(fixture([
      { row: 7, col: 7, player: 1 }, { row: 7, col: 8, player: 2 },
    ], null), true);
    await expect(moveCount).toHaveText('02');
    await expect(page.locator('[data-mode="ai"]')).toHaveAttribute('aria-pressed', 'true');
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(canvas).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
    expect(overflow).toBe(false);
    await page.locator('#view-button').click();
    await expect(page.locator('#view-button')).toHaveAttribute('aria-pressed', 'true');
    await page.locator('#reset-view').click();
    await expect(page.locator('#view-button')).toHaveAttribute('aria-pressed', 'false');
    await page.screenshot({ path: testInfo.outputPath('mobile-game.png'), fullPage: true });
  });

  expect(pageErrors).toEqual([]);
});
