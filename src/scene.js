import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import { RoundedBoxGeometry } from 'three/addons/geometries/RoundedBoxGeometry.js';
import { RoomEnvironment } from 'three/addons/environments/RoomEnvironment.js';

const SIZE = 15;
const TOP = 0.85;

function woodTexture() {
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = 1024;
  const ctx = canvas.getContext('2d');
  ctx.fillStyle = '#cfa066';
  ctx.fillRect(0, 0, 1024, 1024);
  let seed = 17;
  const random = () => { seed = (seed * 16807) % 2147483647; return seed / 2147483647; };
  for (let i = 0; i < 3400; i++) {
    const x = random() * 1024;
    const width = random() * 1.8 + 0.15;
    const wave = 3 + random() * 12;
    ctx.strokeStyle = random() > 0.48 ? `rgba(102,59,22,${random() * 0.15})` : `rgba(255,235,178,${random() * 0.25})`;
    ctx.lineWidth = width;
    ctx.beginPath();
    for (let y = 0; y <= 1024; y += 16) {
      const xx = x + Math.sin(y / 180 + x / 90) * wave + Math.sin(y / 65 + x / 120) * 1.5;
      if (y === 0) ctx.moveTo(xx, y); else ctx.lineTo(xx, y);
    }
    ctx.stroke();
  }
  const grain = new THREE.CanvasTexture(canvas);
  grain.colorSpace = THREE.SRGBColorSpace;
  grain.anisotropy = 8;
  return grain;
}

function labelTexture(text) {
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = 64;
  const ctx = canvas.getContext('2d');
  ctx.fillStyle = '#765535';
  ctx.font = '26px Georgia';
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillText(text, 32, 34);
  return new THREE.CanvasTexture(canvas);
}

export function createBoardScene(container, onPlace) {
  const scene = new THREE.Scene();
  let needsRender = true;
  scene.background = new THREE.Color('#eae7df');
  const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: false });
  renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
  renderer.shadowMap.enabled = true;
  renderer.shadowMap.type = THREE.PCFSoftShadowMap;
  renderer.shadowMap.autoUpdate = false;
  renderer.shadowMap.needsUpdate = true;
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 0.95;
  container.appendChild(renderer.domElement);
  const canvas = renderer.domElement;
  canvas.tabIndex = 0;
  canvas.setAttribute('role', 'application');
  canvas.setAttribute('aria-label', '三维五子棋棋盘。方向键选择交叉点，回车或空格落子。鼠标点击落子，拖动旋转视角。');

  const camera = new THREE.PerspectiveCamera(39, 1, 0.1, 150);
  const controls = new OrbitControls(camera, canvas);
  controls.target.set(0, 0.3, 1.0);
  controls.enableDamping = true;
  controls.dampingFactor = 0.08;
  controls.enablePan = false;
  controls.enableZoom = false;
  controls.minPolarAngle = 0.1;
  controls.maxPolarAngle = Math.PI / 2.8;
  controls.rotateSpeed = 0.45;
  controls.addEventListener('change', () => { needsRender = true; });
  const pmrem = new THREE.PMREMGenerator(renderer);
  const room = new RoomEnvironment();
  const environment = pmrem.fromScene(room, 0.04);
  scene.environment = environment.texture;
  scene.environmentIntensity = 0.62;
  room.dispose();
  pmrem.dispose();
  scene.add(new THREE.HemisphereLight('#fffaee', '#aba38b', 1.2));
  const keyLight = new THREE.DirectionalLight('#fff6e1', 2.5);
  keyLight.position.set(-10, 22, -6);
  keyLight.castShadow = true;
  keyLight.shadow.mapSize.set(2048, 2048);
  Object.assign(keyLight.shadow.camera, { left: -18, right: 18, top: 18, bottom: -18, near: 1, far: 65 });
  keyLight.shadow.normalBias = 0.025;
  keyLight.shadow.bias = -0.0001;
  keyLight.shadow.radius = 4;
  scene.add(keyLight);

  const table = new THREE.Mesh(new THREE.PlaneGeometry(200, 200), new THREE.MeshStandardMaterial({ color: '#dfdcd2', roughness: 0.95 }));
  table.rotation.x = -Math.PI / 2;
  table.position.y = -0.18;
  table.receiveShadow = true;
  scene.add(table);

  const wood = woodTexture();
  const boardMaterial = new THREE.MeshStandardMaterial({ map: wood, color: '#f4cf92', roughness: 0.4, metalness: 0.02, bumpMap: wood, bumpScale: 0.025 });
  const board = new THREE.Mesh(new RoundedBoxGeometry(16.45, 0.8, 16.45, 3, 0.12), boardMaterial);
  board.position.y = 0.45;
  board.castShadow = board.receiveShadow = true;
  scene.add(board);
  const lower = new THREE.Mesh(new RoundedBoxGeometry(16.35, 0.13, 16.35, 2, 0.05), new THREE.MeshStandardMaterial({ map: wood, color: '#996331', roughness: 0.65 }));
  lower.position.y = 0.04;
  lower.castShadow = lower.receiveShadow = true;
  scene.add(lower);
  for (const x of [-5.8, 5.8]) {
    const foot = new THREE.Mesh(new RoundedBoxGeometry(1.6, 0.25, 12, 2, 0.08), boardMaterial);
    foot.position.set(x, -0.05, 0);
    foot.castShadow = true;
    scene.add(foot);
  }
  const points = [];
  for (let i = -7; i <= 7; i++) points.push(i, TOP + 0.006, -7, i, TOP + 0.006, 7, -7, TOP + 0.006, i, 7, TOP + 0.006, i);
  const grid = new THREE.BufferGeometry();
  grid.setAttribute('position', new THREE.Float32BufferAttribute(points, 3));
  scene.add(new THREE.LineSegments(grid, new THREE.LineBasicMaterial({ color: '#604124', transparent: true, opacity: 0.65 })));
  const starMat = new THREE.MeshBasicMaterial({ color: '#67472b' });
  for (const [x, z] of [[0, 0], [-4, -4], [-4, 4], [4, -4], [4, 4]]) {
    const star = new THREE.Mesh(new THREE.CircleGeometry(0.095, 24), starMat);
    star.rotation.x = -Math.PI / 2;
    star.position.set(x, TOP + 0.012, z);
    scene.add(star);
  }
  for (let i = 0; i < SIZE; i++) {
    for (const isColumn of [true, false]) {
      const label = new THREE.Mesh(new THREE.PlaneGeometry(0.43, 0.43), new THREE.MeshBasicMaterial({ map: labelTexture(isColumn ? String.fromCharCode(65 + i) : String(15 - i)), transparent: true, depthWrite: false }));
      label.rotation.x = -Math.PI / 2;
      label.position.set(isColumn ? i - 7 : -7.65, TOP + 0.015, isColumn ? 7.65 : i - 7);
      scene.add(label);
    }
  }

  const stoneGeometry = new THREE.SphereGeometry(0.465, 40, 24);
  stoneGeometry.scale(1, 0.46, 1);
  const materials = {
    1: new THREE.MeshPhysicalMaterial({ color: '#141b1c', roughness: 0.2, metalness: 0.08, clearcoat: 0.65, clearcoatRoughness: 0.2 }),
    2: new THREE.MeshPhysicalMaterial({ color: '#f5f2e9', roughness: 0.21, metalness: 0.02, clearcoat: 0.75, clearcoatRoughness: 0.17 }),
  };
  function makeStone(player) {
    const mesh = new THREE.Mesh(stoneGeometry, materials[player]);
    mesh.castShadow = mesh.receiveShadow = true;
    return mesh;
  }

  // Turned wood bowls and spare stones make the board feel like a physical set.
  for (const [x, z, player] of [[10, -5.5, 2], [-10, 5, 1]]) {
    const bowlPoints = [new THREE.Vector2(0.01, 0), new THREE.Vector2(0.8, 0), new THREE.Vector2(1.25, 0.25), new THREE.Vector2(1.46, 0.7), new THREE.Vector2(1.48, 1.03), new THREE.Vector2(1.38, 1.08), new THREE.Vector2(1.29, 0.66), new THREE.Vector2(1.05, 0.4), new THREE.Vector2(0.01, 0.27)];
    const bowl = new THREE.Mesh(new THREE.LatheGeometry(bowlPoints, 48), new THREE.MeshStandardMaterial({ map: wood, color: '#745033', roughness: 0.42, side: THREE.DoubleSide }));
    bowl.position.set(x, -0.12, z);
    bowl.castShadow = bowl.receiveShadow = true;
    scene.add(bowl);
    for (let i = 0; i < 11; i++) {
      const stone = makeStone(player);
      const angle = i * 2.4;
      const radius = i < 7 ? 0.75 : 0.35;
      stone.position.set(x + Math.cos(angle) * radius, 0.5 + (i > 6 ? 0.32 : 0), z + Math.sin(angle) * radius);
      stone.rotation.set(Math.sin(i) * 0.23, angle, Math.cos(i) * 0.2);
      scene.add(stone);
    }
  }

  const placed = new THREE.Group();
  scene.add(placed);
  const ghost = new THREE.Mesh(stoneGeometry, new THREE.MeshPhysicalMaterial({ color: '#244c41', transparent: true, opacity: 0.38, roughness: 0.4, depthWrite: false }));
  ghost.visible = false;
  scene.add(ghost);
  const marker = new THREE.Mesh(new THREE.TorusGeometry(0.11, 0.018, 8, 32), new THREE.MeshBasicMaterial({ color: '#c99557' }));
  marker.rotation.x = -Math.PI / 2;
  marker.visible = false;
  scene.add(marker);
  let winLine = null;
  let game = null;
  let interactive = true;
  let keyboardCell = { row: 7, col: 7 };
  let topView = false;
  let cameraTween = null;
  const animations = [];
  const raycaster = new THREE.Raycaster();
  const pointer = new THREE.Vector2();
  const hitPlane = new THREE.Plane(new THREE.Vector3(0, 1, 0), -TOP);
  const hit = new THREE.Vector3();
  function cellAt(event) {
    const rect = canvas.getBoundingClientRect();
    pointer.set(((event.clientX - rect.left) / rect.width) * 2 - 1, -((event.clientY - rect.top) / rect.height) * 2 + 1);
    raycaster.setFromCamera(pointer, camera);
    if (!raycaster.ray.intersectPlane(hitPlane, hit)) return null;
    const col = Math.round(hit.x + 7), row = Math.round(hit.z + 7);
    return row >= 0 && row < SIZE && col >= 0 && col < SIZE && Math.abs(hit.x - (col - 7)) < 0.49 && Math.abs(hit.z - (row - 7)) < 0.49 ? { row, col } : null;
  }
  function showGhost(cell) {
    needsRender = true;
    const allowed = cell && interactive && game && !game.winner && !game.draw && !game.board[cell.row][cell.col];
    ghost.visible = Boolean(allowed);
    canvas.style.cursor = allowed ? 'pointer' : 'grab';
    if (allowed) {
      ghost.position.set(cell.col - 7, TOP + 0.22, cell.row - 7);
      ghost.material.color.set(game.currentPlayer === 1 ? '#172320' : '#ffffff');
    }
  }
  let pointerDown = null;
  canvas.addEventListener('pointerdown', e => { pointerDown = { x: e.clientX, y: e.clientY, id: e.pointerId }; });
  canvas.addEventListener('pointermove', e => showGhost(cellAt(e)));
  canvas.addEventListener('pointerleave', () => { ghost.visible = false; needsRender = true; });
  canvas.addEventListener('pointercancel', () => { pointerDown = null; });
  canvas.addEventListener('pointerup', e => {
    if (e.button === 0 && pointerDown?.id === e.pointerId && Math.hypot(e.clientX - pointerDown.x, e.clientY - pointerDown.y) < 6) {
      const cell = cellAt(e);
      if (cell && interactive) { keyboardCell = cell; onPlace(cell.row, cell.col); }
    }
    pointerDown = null;
  });
  canvas.addEventListener('focus', () => showGhost(keyboardCell));
  canvas.addEventListener('blur', () => { ghost.visible = false; needsRender = true; });
  canvas.addEventListener('keydown', e => {
    const directions = { ArrowUp: [-1, 0], ArrowDown: [1, 0], ArrowLeft: [0, -1], ArrowRight: [0, 1] };
    if (directions[e.key]) {
      e.preventDefault();
      const [dr, dc] = directions[e.key];
      keyboardCell = { row: THREE.MathUtils.clamp(keyboardCell.row + dr, 0, 14), col: THREE.MathUtils.clamp(keyboardCell.col + dc, 0, 14) };
      showGhost(keyboardCell);
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      if (interactive) onPlace(keyboardCell.row, keyboardCell.col);
      showGhost(keyboardCell);
    }
  });

  function sync(nextGame, canPlay, animate = true) {
    needsRender = true;
    renderer.shadowMap.needsUpdate = true;
    game = nextGame;
    interactive = canPlay;
    ghost.visible = false;
    while (placed.children.length > game.moves.length) placed.remove(placed.children.at(-1));
    game.moves.forEach((move, i) => {
      const existing = placed.children[i];
      if (existing && (existing.userData.row !== move.row || existing.userData.col !== move.col || existing.userData.player !== move.player)) {
        while (placed.children.length > i) placed.remove(placed.children.at(-1));
      }
      if (!placed.children[i]) {
        const stone = makeStone(move.player);
        stone.userData = move;
        stone.position.set(move.col - 7, TOP + 0.21, move.row - 7);
        placed.add(stone);
        if (animate && i === game.moves.length - 1) animations.push({ stone, start: performance.now() });
      }
    });
    const last = game.moves.at(-1);
    marker.visible = Boolean(last);
    if (last) marker.position.set(last.col - 7, TOP + 0.435, last.row - 7);
    if (winLine) { scene.remove(winLine); winLine.geometry.dispose(); winLine.material.dispose(); winLine = null; }
    if (game.winningLine?.length) {
      const first = game.winningLine[0], end = game.winningLine.at(-1);
      const a = new THREE.Vector3(first.col - 7, TOP + 0.48, first.row - 7);
      const b = new THREE.Vector3(end.col - 7, TOP + 0.48, end.row - 7);
      winLine = new THREE.Mesh(new THREE.CylinderGeometry(0.035, 0.035, a.distanceTo(b), 8), new THREE.MeshBasicMaterial({ color: '#e7b76c' }));
      winLine.position.copy(a).add(b).multiplyScalar(0.5);
      winLine.quaternion.setFromUnitVectors(new THREE.Vector3(0, 1, 0), b.sub(a).normalize());
      scene.add(winLine);
    }
  }
  function defaultPosition() {
    const distance = Math.max(30, 21 / (2 * Math.tan(THREE.MathUtils.degToRad(19.5)) * camera.aspect));
    return topView ? new THREE.Vector3(0, distance + 1, 1.01) : new THREE.Vector3(0, distance * 0.81, distance * 0.59 + 1);
  }
  function resetView(top = false) {
    topView = top;
    cameraTween = { from: camera.position.clone(), to: defaultPosition(), start: performance.now() };
  }
  const resize = new ResizeObserver(() => {
    const { width, height } = container.getBoundingClientRect();
    if (!width || !height) return;
    renderer.setSize(width, height);
    needsRender = true;
    camera.aspect = width / height;
    camera.updateProjectionMatrix();
    camera.position.copy(defaultPosition());
    controls.update();
  });
  resize.observe(container);
  const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  let stopped = false;
  renderer.setAnimationLoop(() => {
    if (stopped) return;
    const now = performance.now();
    if (cameraTween) {
      needsRender = true;
      const t = reducedMotion ? 1 : Math.min((now - cameraTween.start) / 600, 1);
      camera.position.lerpVectors(cameraTween.from, cameraTween.to, 1 - (1 - t) ** 3);
      if (t === 1) cameraTween = null;
    }
    for (let i = animations.length - 1; i >= 0; i--) {
      needsRender = true;
      renderer.shadowMap.needsUpdate = true;
      const anim = animations[i];
      const t = reducedMotion ? 1 : Math.min((now - anim.start) / 250, 1);
      anim.stone.position.y = TOP + 0.21 + (1 - t) ** 3 * 1.2;
      if (t === 1) animations.splice(i, 1);
    }
    controls.update();
    if (needsRender) { renderer.render(scene, camera); needsRender = false; }
  });
  canvas.addEventListener('webglcontextlost', e => {
    e.preventDefault();
    stopped = true;
    container.dispatchEvent(new CustomEvent('rendererror'));
  });
  return { sync, resetView, toggleView() { resetView(!topView); return topView; }, dispose() { stopped = true; renderer.setAnimationLoop(null); resize.disconnect(); controls.dispose(); environment.dispose(); scene.traverse(obj => { obj.geometry?.dispose(); const mats = Array.isArray(obj.material) ? obj.material : [obj.material]; mats.forEach(mat => { mat?.map?.dispose(); mat?.dispose(); }); }); renderer.dispose(); } };
}
