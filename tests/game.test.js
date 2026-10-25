import test from 'node:test';
import assert from 'node:assert/strict';
import {
  BOARD_SIZE, createGame, playMove, undoMoves, chooseMove, serializeGame, restoreGame,
} from '../src/game.js';

function blackThreat(coords, white = [[14, 0], [14, 2], [14, 4], [14, 6]]) {
  const game = createGame();
  coords.forEach(([row, col], index) => {
    assert.equal(playMove(game, row, col), true);
    if (index < white.length) assert.equal(playMove(game, ...white[index]), true);
  });
  return game;
}

test('new board is empty and black starts; invalid moves have no side effects', () => {
  const game = createGame();
  assert.equal(game.board.length, BOARD_SIZE);
  assert.equal(game.currentPlayer, 1);
  for (const [row, col] of [[-1, 0], [15, 1], [0.2, 1], [0, NaN], ['0', 2]]) {
    assert.equal(playMove(game, row, col), false);
  }
  assert.equal(game.moves.length, 0);
  assert.equal(playMove(game, 7, 7), true);
  assert.equal(playMove(game, 7, 7), false);
  assert.equal(game.currentPlayer, 2);
  assert.deepEqual(game.moves, [{ row: 7, col: 7, player: 1 }]);
});

for (const [name, coords] of [
  ['horizontal', [[0, 0], [0, 1], [0, 2], [0, 3], [0, 4]]],
  ['vertical', [[0, 0], [1, 0], [2, 0], [3, 0], [4, 0]]],
  ['diagonal', [[0, 0], [1, 1], [2, 2], [3, 3], [4, 4]]],
  ['reverse diagonal', [[0, 14], [1, 13], [2, 12], [3, 11], [4, 10]]],
]) {
  test(`detects ${name} victory at the board boundary and rejects later moves`, () => {
    const game = blackThreat(coords);
    assert.equal(game.winner, 1);
    assert.equal(game.winningLine.length, 5);
    assert.equal(game.draw, false);
    assert.equal(playMove(game, 10, 10), false);
    assert.equal(chooseMove(game), null);
  });
}

test('freestyle permits connecting six stones with one move', () => {
  const game = blackThreat([[5, 0], [5, 1], [5, 2], [5, 4], [5, 5]],
    [[14, 0], [14, 2], [14, 4], [14, 6], [14, 8]]);
  assert.equal(playMove(game, 5, 3), true);
  assert.equal(game.winner, 1);
  assert.equal(game.winningLine.length, 6);
});

test('undo removes winner and restores turns, including a whole two-move turn', () => {
  const game = blackThreat([[0, 0], [0, 1], [0, 2], [0, 3], [0, 4]]);
  assert.equal(undoMoves(game), true);
  assert.equal(game.winner, 0);
  assert.deepEqual(game.winningLine, []);
  assert.equal(game.currentPlayer, 1);
  assert.equal(game.board[0][4], 0);
  assert.equal(undoMoves(game, 2), true);
  assert.equal(game.currentPlayer, 1);
  assert.equal(game.moves.length, 6);
  assert.equal(undoMoves(game, 999), true);
  assert.deepEqual(game, createGame());
  assert.equal(undoMoves(game), false);
});

test('serialization restores a completed game exactly', () => {
  const game = blackThreat([[0, 0], [0, 1], [0, 2], [0, 3], [0, 4]]);
  assert.deepEqual(restoreGame(serializeGame(game)), game);
  assert.deepEqual(restoreGame(JSON.parse(serializeGame(game))), game);
});

test('a full board without five is a draw and undo resumes play', () => {
  const cells = [[], []];
  for (let row = 0; row < BOARD_SIZE; row++) {
    for (let col = 0; col < BOARD_SIZE; col++) {
      cells[(row + 2 * col) % 4 < 2 ? 0 : 1].push([row, col]);
    }
  }
  const game = createGame();
  for (let turn = 0; turn < BOARD_SIZE * BOARD_SIZE; turn++) {
    assert.equal(playMove(game, ...cells[game.currentPlayer - 1].shift()), true);
  }
  assert.equal(game.draw, true);
  assert.equal(game.winner, 0);
  assert.equal(chooseMove(game), null);
  assert.deepEqual(restoreGame(serializeGame(game)), game);
  assert.equal(undoMoves(game), true);
  assert.equal(game.draw, false);
  assert.equal(game.moves.length, 224);
});

test('restore rejects corrupt, duplicate, out-of-turn and post-victory moves', () => {
  const finished = JSON.parse(serializeGame(blackThreat([[0, 0], [0, 1], [0, 2], [0, 3], [0, 4]])));
  finished.moves.push({ row: 10, col: 10, player: 2 });
  for (const invalid of ['broken', null, {}, { version: 2, moves: [] },
    { version: 1, moves: [{ row: 0, col: 0, player: 2 }] },
    { version: 1, moves: [{ row: 15, col: 0, player: 1 }] },
    { version: 1, moves: [{ row: 0, col: 0, player: 1 }, { row: 0, col: 0, player: 2 }] },
    finished]) assert.throws(() => restoreGame(invalid));
});

for (const difficulty of ['easy', 'medium', 'hard']) {
  test(`${difficulty} AI opens in the center and leaves state untouched`, () => {
    const game = createGame();
    const before = structuredClone(game);
    assert.deepEqual(chooseMove(game, difficulty), { row: 7, col: 7 });
    assert.deepEqual(game, before);
  });

  test(`${difficulty} AI takes an immediate win`, () => {
    const game = blackThreat([[0, 0], [0, 1], [0, 2], [0, 3]]);
    const before = structuredClone(game);
    assert.deepEqual(chooseMove(game, difficulty), { row: 0, col: 4 });
    assert.deepEqual(game, before);
  });

  test(`${difficulty} AI blocks a broken four`, () => {
    const game = blackThreat([[5, 0], [5, 1], [5, 3], [5, 4]], [[14, 0], [14, 2], [14, 4]]);
    assert.equal(game.currentPlayer, 2);
    assert.deepEqual(chooseMove(game, difficulty), { row: 5, col: 2 });
  });
}

test('hard AI returns a legal move in a developing position without mutating it', () => {
  const game = createGame();
  for (const coords of [[7, 7], [7, 8], [8, 7], [6, 7], [8, 8], [8, 6]]) playMove(game, ...coords);
  const before = structuredClone(game);
  const move = chooseMove(game, 'hard');
  assert.deepEqual(game, before);
  assert.equal(playMove(game, move.row, move.col), true);
});
