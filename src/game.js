export const BOARD_SIZE = 15;

const DIRECTIONS = [[0, 1], [1, 0], [1, 1], [1, -1]];
const inside = (row, col) => row >= 0 && col >= 0 && row < BOARD_SIZE && col < BOARD_SIZE;

export function createGame() {
  return {
    board: Array.from({ length: BOARD_SIZE }, () => Array(BOARD_SIZE).fill(0)),
    moves: [],
    currentPlayer: 1,
    winner: 0,
    winningLine: [],
    draw: false,
  };
}

function findWinningLine(board, row, col, player) {
  for (const [dr, dc] of DIRECTIONS) {
    const line = [{ row, col }];
    for (const sign of [-1, 1]) {
      for (let distance = 1; ; distance++) {
        const r = row + dr * distance * sign;
        const c = col + dc * distance * sign;
        if (!inside(r, c) || board[r][c] !== player) break;
        if (sign < 0) line.unshift({ row: r, col: c });
        else line.push({ row: r, col: c });
      }
    }
    if (line.length >= 5) return line;
  }
  return [];
}

export function playMove(game, row, col) {
  if (game.winner || game.draw || !Number.isInteger(row) || !Number.isInteger(col)
    || !inside(row, col) || game.board[row][col] !== 0) return false;

  const player = game.currentPlayer;
  game.board[row][col] = player;
  game.moves.push({ row, col, player });
  game.winningLine = findWinningLine(game.board, row, col, player);
  game.winner = game.winningLine.length ? player : 0;
  game.draw = !game.winner && game.moves.length === BOARD_SIZE * BOARD_SIZE;
  game.currentPlayer = 3 - player;
  return true;
}

export function undoMoves(game, count = 1) {
  if (!Number.isInteger(count) || count < 1 || !game.moves.length) return false;
  const remaining = game.moves.slice(0, Math.max(0, game.moves.length - count));
  const restored = createGame();
  for (const { row, col } of remaining) playMove(restored, row, col);
  Object.assign(game, restored);
  return true;
}

function candidateMoves(board) {
  const candidates = new Set();
  let foundStone = false;
  for (let row = 0; row < BOARD_SIZE; row++) {
    for (let col = 0; col < BOARD_SIZE; col++) {
      if (!board[row][col]) continue;
      foundStone = true;
      for (let dr = -2; dr <= 2; dr++) {
        for (let dc = -2; dc <= 2; dc++) {
          const r = row + dr;
          const c = col + dc;
          if (inside(r, c) && !board[r][c]) candidates.add(r * BOARD_SIZE + c);
        }
      }
    }
  }
  if (!foundStone) return [{ row: 7, col: 7 }];
  return [...candidates].map(index => ({ row: Math.floor(index / BOARD_SIZE), col: index % BOARD_SIZE }));
}

// Assess both uninterrupted runs and five-cell windows, so broken threats count.
// The board is restored before returning; choosing a move never changes a game.
function positionScore(board, row, col, player) {
  board[row][col] = player;
  let score = 0;
  let strongThreats = 0;
  for (const [dr, dc] of DIRECTIONS) {
    let length = 1;
    let openEnds = 0;
    for (const sign of [-1, 1]) {
      let distance = 1;
      while (inside(row + dr * distance * sign, col + dc * distance * sign)
        && board[row + dr * distance * sign][col + dc * distance * sign] === player) {
        length++;
        distance++;
      }
      const r = row + dr * distance * sign;
      const c = col + dc * distance * sign;
      if (inside(r, c) && !board[r][c]) openEnds++;
    }
    if (length >= 5) {
      board[row][col] = 0;
      return 1_000_000_000;
    }
    if (length === 4 && openEnds === 2) score += 1_000_000;
    else if (length === 4 && openEnds === 1) score += 100_000;
    else if (length === 3 && openEnds === 2) score += 15_000;
    else if (length === 3 && openEnds === 1) score += 900;
    else if (length === 2 && openEnds === 2) score += 500;
    else if (length === 2 && openEnds === 1) score += 80;
    else if (openEnds) score += 5;

    let bestWindow = 0;
    for (let offset = -4; offset <= 0; offset++) {
      let stones = 0;
      let valid = true;
      for (let index = 0; index < 5; index++) {
        const r = row + (offset + index) * dr;
        const c = col + (offset + index) * dc;
        if (!inside(r, c) || (board[r][c] && board[r][c] !== player)) {
          valid = false;
          break;
        }
        if (board[r][c] === player) stones++;
      }
      if (valid) bestWindow = Math.max(bestWindow, stones);
    }
    if (bestWindow === 4) score += 50_000;
    else if (bestWindow === 3) score += 650;
    else if (bestWindow === 2) score += 45;
    if (bestWindow >= 4 || (length >= 3 && openEnds === 2)) strongThreats++;
  }
  if (strongThreats >= 2) score += 80_000 * strongThreats;
  board[row][col] = 0;
  return score;
}

function rankMoves(board, player) {
  return candidateMoves(board).map(({ row, col }) => {
    const attack = positionScore(board, row, col, player);
    const defense = positionScore(board, row, col, 3 - player);
    const centrality = 14 - Math.abs(row - 7) - Math.abs(col - 7);
    return { row, col, attack, defense, score: attack + defense * 1.08 + centrality };
  }).sort((a, b) => b.score - a.score || b.attack - a.attack || a.row - b.row || a.col - b.col);
}

export function chooseMove(game, difficulty = 'medium') {
  if (game.winner || game.draw) return null;
  const player = game.currentPlayer;
  const ranked = rankMoves(game.board, player);
  if (!ranked.length) return null;

  // Tactics override difficulty: finish five, otherwise stop an immediate five.
  const immediateWin = ranked.find(move => move.attack >= 1_000_000_000);
  const immediateBlock = ranked.find(move => move.defense >= 1_000_000_000);
  let selected = immediateWin || immediateBlock;
  if (!selected && difficulty === 'easy') {
    const pool = ranked.slice(0, Math.min(4, ranked.length));
    selected = pool[Math.floor(Math.random() * pool.length)];
  }
  if (!selected && difficulty === 'hard') {
    // A bounded opponent reply search penalizes moves that create a losing reply.
    // Keeping the shortlist small makes this responsive even on mobile devices.
    selected = ranked.slice(0, 10).map(move => {
      game.board[move.row][move.col] = player;
      const reply = rankMoves(game.board, 3 - player)[0];
      game.board[move.row][move.col] = 0;
      const replyDanger = reply ? reply.attack + reply.defense * 0.15 : 0;
      return { ...move, searchScore: move.score - replyDanger * 0.75 };
    }).sort((a, b) => b.searchScore - a.searchScore)[0];
  }
  selected ||= ranked[0];
  return { row: selected.row, col: selected.col };
}

export function serializeGame(game) {
  return JSON.stringify({ version: 1, moves: game.moves.map(({ row, col, player }) => ({ row, col, player })) });
}

export function restoreGame(raw) {
  let saved;
  try {
    saved = typeof raw === 'string' ? JSON.parse(raw) : raw;
  } catch {
    throw new Error('棋局文件不是有效的 JSON。');
  }
  if (!saved || typeof saved !== 'object' || Array.isArray(saved) || saved.version !== 1
    || !Array.isArray(saved.moves) || saved.moves.length > BOARD_SIZE * BOARD_SIZE) {
    throw new Error('棋局文件格式或版本不受支持。');
  }
  const game = createGame();
  for (const move of saved.moves) {
    if (!move || typeof move !== 'object' || Array.isArray(move)
      || move.player !== game.currentPlayer || !playMove(game, move.row, move.col)) {
      throw new Error('棋局包含无效落子，无法读取。');
    }
  }
  return game;
}
