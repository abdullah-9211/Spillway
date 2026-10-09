"use client";

import { useEffect, useRef, useState } from "react";
import { FAULT_WORD, HOP_MS, signature, type SceneNode } from "@/lib/scene";

export type ScenePhase = "idle" | "waiting" | "playing" | "done";

type Props = {
  nodes: SceneNode[];
  phase: ScenePhase;
  /** Changes to restart a replay. */
  playKey: number;
  onDone?: () => void;
  /** Click on a provider node; omit to make the nodes inert. */
  onNode?: (provider: string) => void;
};

const HEIGHT = 210;
const R = 20;
const POOL = 900;

/**
 * The playground's stage, drawn with three.js: the request is a packet of light that travels from stop to stop.
 * A failed provider shakes and bursts red, an answering one blooms green. Colours come from the theme's own tokens,
 * so it follows dark and light. It is decoration over words: every stop also has a label here and the route list
 * under the answer carries the same facts for screen readers. Without WebGL, or with reduced motion, it draws the
 * final picture once and does not animate.
 */
export function RouteScene({ nodes, phase, playKey, onDone, onNode }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const live = useRef({ nodes, phase, playKey, onDone, onNode });
  const [ok, setOk] = useState(true);
  const sig = signature(nodes);

  // The animation loop reads the latest props from here, so a new prop never rebuilds the scene.
  useEffect(() => {
    live.current = { nodes, phase, playKey, onDone, onNode };
  });

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    let stop = () => {};
    let dead = false;
    Promise.all([
      import("three"),
      import("three/examples/jsm/postprocessing/EffectComposer.js"),
      import("three/examples/jsm/postprocessing/RenderPass.js"),
      import("three/examples/jsm/postprocessing/UnrealBloomPass.js"),
      import("three/examples/jsm/postprocessing/OutputPass.js"),
    ])
      .then(([THREE, ec, rp, ub, op]) => {
        if (dead) return;
        const off = build(THREE, { EffectComposer: ec.EffectComposer, RenderPass: rp.RenderPass, UnrealBloomPass: ub.UnrealBloomPass, OutputPass: op.OutputPass }, el, live);
        if (off) stop = off;
        else setOk(false);
      })
      .catch(() => setOk(false));
    return () => {
      dead = true;
      stop();
    };
  }, [sig]);

  const n = nodes.length;
  return (
    <div className="scene" ref={host} style={{ height: HEIGHT }} data-phase={phase}>
      {!ok && <p className="small scene__off">The animation needs WebGL. The route is listed below.</p>}
      <ul className="scene__labels" aria-hidden="true">
        {nodes.map((nd, i) => (
          <li key={nd.id} style={{ left: `${((i + 0.5) / n) * 100}%` }} className={`scene__lab ${nd.state}`}>
            <span className="hn">{nd.label}</span>
            {nd.fault && <span className="scene__fault">{FAULT_WORD[nd.fault]}</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}

type Live = { current: Props };
type Three = typeof import("three");

const cssColor = (el: HTMLElement, name: string, fallback: string) => getComputedStyle(el).getPropertyValue(name).trim() || fallback;

type Post = { EffectComposer: typeof import("three/examples/jsm/postprocessing/EffectComposer.js").EffectComposer; RenderPass: typeof import("three/examples/jsm/postprocessing/RenderPass.js").RenderPass; UnrealBloomPass: typeof import("three/examples/jsm/postprocessing/UnrealBloomPass.js").UnrealBloomPass; OutputPass: typeof import("three/examples/jsm/postprocessing/OutputPass.js").OutputPass };

function build(THREE: Three, post: Post, el: HTMLElement, live: Live): (() => void) | null {
  let renderer: InstanceType<Three["WebGLRenderer"]>;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true });
  } catch {
    return null;
  }
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.domElement.className = "scene__canvas";
  renderer.domElement.setAttribute("aria-hidden", "true");
  el.prepend(renderer.domElement);

  const reduced = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
  const scene = new THREE.Scene();
  const camera = new THREE.OrthographicCamera(0, 1, HEIGHT, 0, -10, 10);
  const composer = new post.EffectComposer(renderer);
  composer.addPass(new post.RenderPass(scene, camera));
  const bloom = new post.UnrealBloomPass(new THREE.Vector2(1, 1), 0.7, 0.55, 0.12);
  composer.addPass(bloom);
  composer.addPass(new post.OutputPass());
  let flash = 0; // 0..1, decays: a bright pulse on a result
  let shakeCam = 0;
  const mouse = { x: 0.5, y: 0.5, sx: 0.5, sy: 0.5 };

  // --- colours from the theme tokens ---
  const col = { ok: new THREE.Color(), fail: new THREE.Color(), mute: new THREE.Color(), line: new THREE.Color(), text: new THREE.Color(), bg: new THREE.Color(), accent: new THREE.Color(), wait: new THREE.Color() };
  let light = false;
  const readColors = () => {
    const set = (c: InstanceType<Three["Color"]>, v: string, fb: string) => c.set(v || fb);
    set(col.ok, cssColor(el, "--ok", "#27a644"), "#27a644");
    set(col.fail, cssColor(el, "--fail", "#c64545"), "#c64545");
    set(col.mute, cssColor(el, "--subtle", "#8a8f98"), "#8a8f98");
    set(col.line, cssColor(el, "--line2", "#34343a"), "#34343a");
    set(col.text, cssColor(el, "--text", "#f7f8f8"), "#f7f8f8");
    set(col.bg, cssColor(el, "--bg", "#010102"), "#010102");
    set(col.accent, cssColor(el, "--accent", "#5e6ad2"), "#5e6ad2");
    set(col.wait, cssColor(el, "--wait", "#e8a55a"), "#e8a55a");
    light = col.bg.r + col.bg.g + col.bg.b > 1.5;
  };
  readColors();
  renderer.setClearColor(col.bg, 1);
  const themeWatch = new MutationObserver(() => {
    readColors();
    renderer.setClearColor(col.bg, 1);
    restyle();
  });
  themeWatch.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

  // --- shared textures and geometry ---
  const glowTex = (() => {
    const c = document.createElement("canvas");
    c.width = c.height = 128;
    const g = c.getContext("2d");
    if (g) {
      const gr = g.createRadialGradient(64, 64, 0, 64, 64, 64);
      gr.addColorStop(0, "rgba(255,255,255,1)");
      gr.addColorStop(0.35, "rgba(255,255,255,0.35)");
      gr.addColorStop(1, "rgba(255,255,255,0)");
      g.fillStyle = gr;
      g.fillRect(0, 0, 128, 128);
    }
    return new THREE.CanvasTexture(c);
  })();
  const disc = new THREE.CircleGeometry(R - 2, 40);
  const ring = new THREE.RingGeometry(R - 1.5, R + 0.5, 48);
  const wave = new THREE.RingGeometry(0.92, 1, 64);
  const bar = new THREE.PlaneGeometry(1, 2);
  const barL = new THREE.PlaneGeometry(1, 2).translate(0.5, 0, 0); // anchored at its left end, so scaling grows it rightwards
  const cageG = new THREE.EdgesGeometry(new THREE.IcosahedronGeometry(R * 0.55, 0));
  const owned: { dispose(): void }[] = [glowTex, disc, ring, wave, bar, barL, cageG];
  const mat = <T extends InstanceType<Three["Material"]>>(m: T) => (owned.push(m), m);

  // --- nodes ---
  type N = { data: SceneNode; x: number; y: number; ringM: InstanceType<Three["MeshBasicMaterial"]>; fillM: InstanceType<Three["MeshBasicMaterial"]>; glow: InstanceType<Three["Sprite"]>; group: InstanceType<Three["Group"]>; shown: SceneNode["state"]; born: number; shake: number; hover: number; cage: InstanceType<Three["LineSegments"]>; cageM: InstanceType<Three["LineBasicMaterial"]>; spin: number };
  const nodes: N[] = [];
  const links: { m: InstanceType<Three["MeshBasicMaterial"]>; fill: InstanceType<Three["Mesh"]>; fillM: InstanceType<Three["MeshBasicMaterial"]> }[] = [];
  const Y = HEIGHT * 0.62;

  const make = () => {
    const data = live.current.nodes;
    data.forEach((d, i) => {
      const group = new THREE.Group();
      const ringM = mat(new THREE.MeshBasicMaterial({ transparent: true }));
      const fillM = mat(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0 }));
      const gm = mat(new THREE.SpriteMaterial({ map: glowTex, transparent: true, depthWrite: false, opacity: 0 }));
      const glow = new THREE.Sprite(gm);
      glow.scale.set(R * 4.4, R * 4.4, 1);
      const cageM = mat(new THREE.LineBasicMaterial({ transparent: true, opacity: 0.9 }));
      const cage = new THREE.LineSegments(cageG, cageM);
      cage.position.z = 0.5;
      group.add(glow, new THREE.Mesh(disc, mat(new THREE.MeshBasicMaterial({ color: col.bg }))), new THREE.Mesh(disc, fillM), cage, new THREE.Mesh(ring, ringM));
      scene.add(group);
      nodes.push({ data: d, x: 0, y: Y, ringM, fillM, glow, group, shown: d.state === "start" ? "start" : "plan", born: 120 + i * 90, shake: 0, hover: 0, cage, cageM, spin: 0.5 });
      if (i > 0) {
        const m = mat(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0.9 }));
        const fm = mat(new THREE.MeshBasicMaterial({ transparent: true }));
        const base = new THREE.Mesh(bar, m);
        const fill = new THREE.Mesh(barL, fm);
        fill.scale.x = 0;
        scene.add(base, fill);
        links.push({ m, fill, fillM: fm });
        links[links.length - 1].fill.userData.base = base;
      }
    });
  };
  make();

  // --- packet and effects ---
  const packetGlow = new THREE.Sprite(mat(new THREE.SpriteMaterial({ map: glowTex, transparent: true, depthWrite: false })));
  packetGlow.scale.set(46, 46, 1);
  const packetCore = new THREE.Mesh(new THREE.CircleGeometry(5, 24), mat(new THREE.MeshBasicMaterial({ transparent: true })));
  owned.push(packetCore.geometry);
  scene.add(packetGlow, packetCore);

  const waves = Array.from({ length: 8 }, () => {
    const m = mat(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0, side: THREE.DoubleSide }));
    const mesh = new THREE.Mesh(wave, m);
    mesh.visible = false;
    scene.add(mesh);
    return { mesh, m, t: 0, life: 0, size: 0 };
  });
  let waveI = 0;
  const pulse = (x: number, y: number, color: InstanceType<Three["Color"]>, size = 70, life = 650) => {
    const w = waves[waveI++ % waves.length];
    w.mesh.position.set(x, y, 1);
    w.m.color.copy(color);
    w.t = 0;
    w.life = life;
    w.size = size;
    w.mesh.visible = true;
  };

  // drifting dust in the background, shifted a little by the pointer for depth
  const DUST = 120;
  const dustPos = new Float32Array(DUST * 3);
  const dustSpd = new Float32Array(DUST);
  for (let i = 0; i < DUST; i++) {
    dustPos[i * 3] = Math.random() * 1200;
    dustPos[i * 3 + 1] = Math.random() * HEIGHT;
    dustPos[i * 3 + 2] = -5;
    dustSpd[i] = 4 + Math.random() * 14;
  }
  const dustGeo = new THREE.BufferGeometry();
  dustGeo.setAttribute("position", new THREE.BufferAttribute(dustPos, 3));
  const dustM = mat(new THREE.PointsMaterial({ size: 2, transparent: true, opacity: 0.5, depthWrite: false, sizeAttenuation: false }));
  const dust = new THREE.Points(dustGeo, dustM);
  dust.frustumCulled = false;
  scene.add(dust);
  owned.push(dustGeo);

  // a comet tail behind the packet
  const TAIL = 28;
  const tailPos = new Float32Array(TAIL * 3);
  const tailCol = new Float32Array(TAIL * 3);
  const tailGeo = new THREE.BufferGeometry();
  tailGeo.setAttribute("position", new THREE.BufferAttribute(tailPos, 3));
  tailGeo.setAttribute("color", new THREE.BufferAttribute(tailCol, 3));
  const tailM = mat(new THREE.LineBasicMaterial({ vertexColors: true, transparent: true }));
  const tail = new THREE.Line(tailGeo, tailM);
  tail.frustumCulled = false;
  scene.add(tail);
  owned.push(tailGeo);
  const trail: [number, number][] = [];

  // lightning: a few jagged lines that are redrawn while they live
  const BOLT_PTS = 14;
  const bolts = Array.from({ length: 6 }, () => {
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(new Float32Array(BOLT_PTS * 3), 3));
    const m = mat(new THREE.LineBasicMaterial({ transparent: true }));
    const line = new THREE.Line(g, m);
    line.frustumCulled = false;
    line.visible = false;
    scene.add(line);
    owned.push(g);
    return { line, m, g, life: 0, x1: 0, y1: 0, x2: 0, y2: 0, redraw: 0 };
  });
  let boltI = 0;
  const drawBolt = (b: (typeof bolts)[number]) => {
    const a = b.g.attributes.position.array as Float32Array;
    const dx = b.x2 - b.x1, dy = b.y2 - b.y1;
    const len = Math.hypot(dx, dy) || 1;
    const nx = -dy / len, ny = dx / len;
    for (let i = 0; i < BOLT_PTS; i++) {
      const t = i / (BOLT_PTS - 1);
      const j = i === 0 || i === BOLT_PTS - 1 ? 0 : (Math.random() - 0.5) * Math.min(26, len * 0.35);
      a[i * 3] = b.x1 + dx * t + nx * j;
      a[i * 3 + 1] = b.y1 + dy * t + ny * j;
      a[i * 3 + 2] = 2.5;
    }
    b.g.attributes.position.needsUpdate = true;
  };
  const bolt = (x1: number, y1: number, x2: number, y2: number, color: InstanceType<Three["Color"]>, life = 220) => {
    const b = bolts[boltI++ % bolts.length];
    Object.assign(b, { x1, y1, x2, y2, life, redraw: 0 });
    b.m.color.copy(color);
    b.line.visible = true;
    drawBolt(b);
  };

  // a full-width wash of colour for a moment when a result lands
  const wash = new THREE.Mesh(new THREE.PlaneGeometry(1, 1), mat(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0, depthWrite: false })));
  wash.position.z = 4;
  scene.add(wash);
  owned.push(wash.geometry);

  const pos = new Float32Array(POOL * 3);
  const colr = new Float32Array(POOL * 3);
  const vel = new Float32Array(POOL * 2);
  const life = new Float32Array(POOL);
  const maxLife = new Float32Array(POOL);
  const geo = new THREE.BufferGeometry();
  geo.setAttribute("position", new THREE.BufferAttribute(pos, 3));
  geo.setAttribute("color", new THREE.BufferAttribute(colr, 3));
  const pointsM = mat(new THREE.PointsMaterial({ size: 4.5, vertexColors: true, transparent: true, depthWrite: false, sizeAttenuation: false }));
  const points = new THREE.Points(geo, pointsM);
  points.frustumCulled = false;
  scene.add(points);
  owned.push(geo);
  let pi = 0;
  const spark = (x: number, y: number, color: InstanceType<Three["Color"]> | InstanceType<Three["Color"]>[], n: number, speed: number, gravity = 0) => {
    for (let k = 0; k < n; k++) {
      const i = pi++ % POOL;
      const a = Math.random() * Math.PI * 2;
      const s = speed * (0.35 + Math.random() * 0.65);
      pos[i * 3] = x;
      pos[i * 3 + 1] = y;
      vel[i * 2] = Math.cos(a) * s;
      vel[i * 2 + 1] = Math.sin(a) * s + gravity;
      const c = Array.isArray(color) ? color[Math.floor(Math.random() * color.length)] : color;
      colr[i * 3] = c.r;
      colr[i * 3 + 1] = c.g;
      colr[i * 3 + 2] = c.b;
      maxLife[i] = life[i] = 450 + Math.random() * 600;
    }
  };

  const stateColor = (s: SceneNode["state"]) => (s === "ok" ? col.ok : s === "fail" ? col.fail : s === "start" ? col.text : col.mute);
  function restyle() {
    bloom.enabled = !light;
    bloom.strength = 0.75;
    dustM.color.copy(col.mute);
    pointsM.blending = light ? THREE.NormalBlending : THREE.AdditiveBlending;
    for (const l of links) l.m.color.copy(col.line);
    packetCore.material.color.copy(light ? col.accent : col.text);
    (packetGlow.material as InstanceType<Three["SpriteMaterial"]>).color.copy(col.accent);
    (packetGlow.material as InstanceType<Three["SpriteMaterial"]>).blending = light ? THREE.NormalBlending : THREE.AdditiveBlending;
    for (const n of nodes) {
      n.ringM.color.copy(stateColor(n.shown));
      n.fillM.color.copy(stateColor(n.shown));
      (n.glow.material as InstanceType<Three["SpriteMaterial"]>).color.copy(stateColor(n.shown));
      (n.glow.material as InstanceType<Three["SpriteMaterial"]>).blending = light ? THREE.NormalBlending : THREE.AdditiveBlending;
    }
  }

  function layout() {
    const w = el.clientWidth || 600;
    renderer.setSize(w, HEIGHT, false);
    composer.setSize(w, HEIGHT);
    bloom.resolution.set(w, HEIGHT);
    renderer.domElement.style.width = `${w}px`;
    renderer.domElement.style.height = `${HEIGHT}px`;
    camera.right = w;
    wash.scale.set(w, HEIGHT, 1);
    wash.position.set(w / 2, HEIGHT / 2, 4);
    for (let i = 0; i < DUST; i++) if (dustPos[i * 3] > w) dustPos[i * 3] = Math.random() * w;
    camera.updateProjectionMatrix();
    nodes.forEach((n, i) => {
      n.x = ((i + 0.5) / nodes.length) * w;
      n.group.position.set(n.x, n.y, 0);
    });
    links.forEach((l, k) => {
      const a = nodes[k], b = nodes[k + 1];
      const base = l.fill.userData.base as InstanceType<Three["Mesh"]>;
      const len = Math.max(b.x - a.x - R * 2, 1);
      base.position.set((a.x + b.x) / 2, Y, -1);
      base.scale.set(len, 0.75, 1);
      l.fill.position.set(a.x + R, Y, -0.5);
      l.fill.userData.len = len;
      l.fill.userData.x0 = a.x + R;
    });
  }
  const W_ = () => el.clientWidth || 600;
  const resize = new ResizeObserver(layout);
  resize.observe(el);
  layout();
  restyle();

  // --- the timeline ---
  const t0 = performance.now();
  let lastPhase = live.current.phase;
  let lastKey = live.current.playKey;
  let phaseT = t0;
  let doneSent = false;
  let packetX = nodes[0]?.x ?? 0;
  let last = t0;

  const apply = (n: N, s: SceneNode["state"], withFx: boolean) => {
    if (n.shown === s) return;
    n.shown = s;
    n.ringM.color.copy(stateColor(s));
    n.fillM.color.copy(stateColor(s));
    (n.glow.material as InstanceType<Three["SpriteMaterial"]>).color.copy(stateColor(s));
    if (!withFx) return;
    if (s === "fail") {
      n.shake = 1;
      n.spin = 9;
      pulse(n.x, n.y, col.fail, 66, 600);
      spark(n.x, n.y, col.fail, 70, 220);
      const idx = nodes.indexOf(n);
      const prev = nodes[Math.max(idx - 1, 0)];
      bolt(prev.x + R, prev.y, n.x - R, n.y, col.fail, 360);
      for (let k = 0; k < 3; k++) bolt(n.x, n.y, n.x + Math.cos(k * 2.1 + 1) * 62, n.y + Math.sin(k * 2.1 + 1) * 62, col.fail, 240);
      flash = 0.8;
      shakeCam = 1;
      wash.material.color.copy(col.fail);
    } else if (s === "ok") {
      n.spin = 7;
      pulse(n.x, n.y, col.ok, 84, 900);
      pulse(n.x, n.y, col.ok, 56, 600);
      spark(n.x, n.y, [col.ok, col.accent, col.wait, col.text], 190, 300, 60);
      flash = 1;
      wash.material.color.copy(col.ok);
    }
  };

  const final = () => {
    nodes.forEach((n) => apply(n, n.data.state, false));
    links.forEach((l, k) => {
      l.fill.scale.set(l.fill.userData.len as number, 1.5, 1);
      l.fillM.color.copy(stateColor(nodes[k + 1].data.state));
    });
  };

  let raf = 0;
  const loop = (now: number) => {
    raf = requestAnimationFrame(loop);
    step(now);
  };
  const step = (now: number) => {
    const dt = Math.max(0, Math.min(now - last, 50));
    last = now;
    const { phase, playKey, nodes: data } = live.current;
    if (phase !== lastPhase || playKey !== lastKey) {
      lastPhase = phase;
      lastKey = playKey;
      phaseT = now;
      doneSent = false;
      if (phase === "playing" || phase === "waiting") {
        nodes.forEach((n, i) => i > 0 && apply(n, "plan", false));
        links.forEach((l) => l.fill.scale.set(0, 1.5, 1));
      }
    }
    const el_ = Math.max(0, now - phaseT); // rAF timestamps can precede performance.now() by a hair
    const clock = Math.max(0, now - t0);

    // nodes: pop in, shake, hover, flicker when a fault is set
    nodes.forEach((n, i) => {
      const age = clock - n.born;
      const k = age <= 0 ? 0 : Math.min(age / 420, 1);
      const pop = k === 0 ? 0 : 1 + 2.2 * Math.pow(k - 1, 3) + 1.2 * Math.pow(k - 1, 2); // ease out back
      n.hover += ((n.hover > 0.5 ? 1 : 0) - n.hover) * 0.2;
      n.shake = Math.max(0, n.shake - dt / 450);
      const sx = Math.sin(clock * 0.09) * n.shake * 7;
      n.group.position.set(n.x + sx, n.y + Math.sin(clock / 900 + i) * 1.6, 0);
      n.group.scale.setScalar(Math.max(pop, 0.0001) * (1 + n.hover * 0.12));
      const faulted = !!data[i]?.fault && n.shown === "plan";
      const flick = faulted ? (Math.sin(clock * 0.03 + i * 3) > 0.15 ? 1 : 0.35) : 1;
      n.ringM.color.copy(faulted ? col.fail : stateColor(n.shown)).multiplyScalar(1);
      n.ringM.opacity = (n.shown === "plan" ? 0.8 : 1) * flick;
      n.spin += ((n.shown === "plan" ? 0.5 : 1.1) - n.spin) * Math.min(1, dt / 500);
      n.cage.rotation.x += (n.spin * dt) / 1000;
      n.cage.rotation.y += (n.spin * 1.3 * dt) / 1000;
      n.cageM.color.copy(faulted ? col.fail : stateColor(n.shown));
      n.cageM.opacity = (n.shown === "plan" ? 0.55 : 0.95) * flick;
      if (faulted && Math.random() < dt / 380) bolt(n.x, n.y, n.x + (Math.random() - 0.5) * 120, n.y + (Math.random() - 0.5) * 90, col.fail, 160);
      n.fillM.opacity = n.shown === "ok" || n.shown === "fail" ? 0.22 : n.shown === "start" ? 0.08 : 0;
      const gm = n.glow.material as InstanceType<Three["SpriteMaterial"]>;
      const glowTarget = n.shown === "ok" ? 0.5 : n.shown === "fail" ? 0.45 : faulted ? 0.3 * flick : n.shown === "start" ? 0.18 : 0;
      gm.opacity += (glowTarget * (light ? 0.7 : 1) - gm.opacity) * 0.12;
      if (faulted && Math.random() < dt / 90) spark(n.x + (Math.random() - 0.5) * R, n.y + (Math.random() - 0.5) * R, col.fail, 1, 60);
    });

    // packet
    let px = nodes[0]?.x ?? 0;
    let py = Y;
    let packetOn = true;
    if (phase === "idle" || phase === "done") {
      px = nodes[0]?.x ?? 0;
      py = Y + Math.sin(clock / 500) * 3;
      if (phase === "done") packetOn = false;
      if (phase === "idle" && Math.random() < dt / 160) spark(px, py, col.accent, 1, 40);
    } else if (phase === "waiting") {
      const a = nodes[0], b = nodes[1] ?? nodes[0];
      const u = (Math.sin(el_ / 380 - Math.PI / 2) + 1) / 2;
      px = a.x + (b.x - a.x) * u * 0.8;
      py = Y + Math.sin(el_ / 120) * 3;
      if (Math.random() < dt / 22) spark(px, py, col.accent, 1, 30);
      if (Math.floor(el_ / 900) !== Math.floor((el_ - dt) / 900)) pulse(a.x, a.y, col.accent, 60, 800);
    } else {
      // playing: hop i takes HOP_MS; the packet arrives at node i at i * HOP_MS
      const total = (nodes.length - 1) * HOP_MS;
      const t = reduced ? total + 1 : el_;
      const hop = Math.min(Math.floor(t / HOP_MS), nodes.length - 1);
      const u = Math.min(1, Math.max(0, (t - hop * HOP_MS) / HOP_MS));
      for (let i = 1; i < nodes.length; i++) {
        const arrive = i * HOP_MS;
        if (t >= arrive) apply(nodes[i], nodes[i].data.state, !reduced);
        const l = links[i - 1];
        const frac = t >= arrive ? 1 : t > arrive - HOP_MS ? (t - (arrive - HOP_MS)) / HOP_MS : 0;
        l.fill.scale.set(((l.fill.userData.len as number) || 1) * Math.min(1, frac * 1.0), 1.5, 1);
        l.fillM.color.copy(stateColor(nodes[i].shown === "plan" ? "start" : nodes[i].shown === "skip" ? "skip" : nodes[i].shown));
      }
      if (t >= total) {
        px = nodes[nodes.length - 1].x;
        packetOn = t < total + 250;
        if (!doneSent && t >= total + 250) {
          doneSent = true;
          live.current.onDone?.();
        }
      } else {
        const a = nodes[hop], b = nodes[Math.min(hop + 1, nodes.length - 1)];
        const e = u < 0.5 ? 2 * u * u : 1 - Math.pow(-2 * u + 2, 2) / 2;
        px = a.x + (b.x - a.x) * e;
        py = Y + Math.sin(u * Math.PI) * 26;
        if (Math.random() < 0.9) spark(px, py, col.accent, 2, 38);
      }
    }
    packetX = px;
    packetCore.position.set(px, py, 2);
    packetGlow.position.set(px, py, 1.5);
    packetCore.visible = packetGlow.visible = packetOn;
    (packetGlow.material as InstanceType<Three["SpriteMaterial"]>).opacity = (light ? 0.55 : 0.9) * (0.75 + 0.25 * Math.sin(clock / 160));

    // waves
    for (const w of waves) {
      if (!w.mesh.visible) continue;
      w.t += dt;
      const k = w.t / w.life;
      if (k >= 1) {
        w.mesh.visible = false;
        continue;
      }
      w.mesh.scale.setScalar(R + (w.size - R) * (1 - Math.pow(1 - k, 3)));
      w.m.opacity = 0.8 * (1 - k);
    }

    // comet tail
    if (packetOn) trail.unshift([px, py]);
    else if (trail.length) trail.pop();
    while (trail.length > TAIL) trail.pop();
    const tc = light ? col.accent : col.text;
    for (let i = 0; i < TAIL; i++) {
      const p = trail[Math.min(i, Math.max(trail.length - 1, 0))] ?? [px, py];
      tailPos[i * 3] = p[0];
      tailPos[i * 3 + 1] = p[1];
      tailPos[i * 3 + 2] = 1;
      const f = trail.length ? Math.pow(1 - i / TAIL, 2) * (i < trail.length ? 1 : 0) : 0;
      tailCol[i * 3] = col.accent.r * f + tc.r * f * 0.2;
      tailCol[i * 3 + 1] = col.accent.g * f + tc.g * f * 0.2;
      tailCol[i * 3 + 2] = col.accent.b * f + tc.b * f * 0.2;
    }
    tailGeo.attributes.position.needsUpdate = true;
    tailGeo.attributes.color.needsUpdate = true;
    tailM.blending = light ? THREE.NormalBlending : THREE.AdditiveBlending;

    // dust drifts, and shifts a little with the pointer
    mouse.sx += (mouse.x - mouse.sx) * 0.06;
    mouse.sy += (mouse.y - mouse.sy) * 0.06;
    for (let i = 0; i < DUST; i++) {
      dustPos[i * 3] -= (dustSpd[i] * dt) / 1000;
      if (dustPos[i * 3] < -10) dustPos[i * 3] = W_() + 10;
    }
    dust.position.set((0.5 - mouse.sx) * 18, (0.5 - mouse.sy) * 10, 0);
    dustGeo.attributes.position.needsUpdate = true;

    // lightning
    for (const b of bolts) {
      if (!b.line.visible) continue;
      b.life -= dt;
      if (b.life <= 0) {
        b.line.visible = false;
        continue;
      }
      b.redraw -= dt;
      if (b.redraw <= 0) {
        drawBolt(b);
        b.redraw = 45;
      }
      b.m.opacity = Math.min(1, b.life / 140);
    }

    // flash and shake
    flash = Math.max(0, flash - dt / 520);
    shakeCam = Math.max(0, shakeCam - dt / 380);
    wash.material.opacity = flash * (light ? 0.1 : 0.16);
    bloom.strength = 0.75 + flash * 1.1;
    camera.position.x = (Math.random() - 0.5) * shakeCam * 7;
    camera.position.y = (Math.random() - 0.5) * shakeCam * 5;

    // particles
    for (let i = 0; i < POOL; i++) {
      if (life[i] <= 0) {
        pos[i * 3 + 2] = 100; // off screen
        continue;
      }
      life[i] -= dt;
      const f = Math.max(life[i], 0) / maxLife[i];
      pos[i * 3] += (vel[i * 2] * dt) / 1000;
      pos[i * 3 + 1] += (vel[i * 2 + 1] * dt) / 1000;
      vel[i * 2] *= 0.985;
      vel[i * 2 + 1] = vel[i * 2 + 1] * 0.985 - dt * 0.03;
      pos[i * 3 + 2] = 3;
      if (f < 0.3) {
        colr[i * 3] *= 0.93;
        colr[i * 3 + 1] *= 0.93;
        colr[i * 3 + 2] *= 0.93;
      }
    }
    geo.attributes.position.needsUpdate = true;
    geo.attributes.color.needsUpdate = true;
    composer.render();
  };

  // pointer: hover and click on provider nodes
  const nodeAt = (e: PointerEvent) => {
    const r = renderer.domElement.getBoundingClientRect();
    const x = e.clientX - r.left, y = HEIGHT - (e.clientY - r.top);
    return nodes.findIndex((n, i) => i > 0 && n.data.provider && Math.hypot(n.x - x, n.y - y) < R + 10);
  };
  const onMove = (e: PointerEvent) => {
    const r0 = renderer.domElement.getBoundingClientRect();
    mouse.x = (e.clientX - r0.left) / Math.max(r0.width, 1);
    mouse.y = (e.clientY - r0.top) / Math.max(r0.height, 1);
    const i = live.current.onNode ? nodeAt(e) : -1;
    nodes.forEach((n, k) => (n.hover = k === i ? 1 : 0));
    renderer.domElement.style.cursor = i >= 0 ? "pointer" : "";
  };
  const onLeave = () => nodes.forEach((n) => (n.hover = 0));
  const onClick = (e: PointerEvent) => {
    const i = live.current.onNode ? nodeAt(e) : -1;
    if (i >= 0) {
      live.current.onNode?.(nodes[i].data.provider as string);
      spark(nodes[i].x, nodes[i].y, col.fail, 14, 90);
    }
  };
  renderer.domElement.addEventListener("pointermove", onMove);
  renderer.domElement.addEventListener("pointerleave", onLeave);
  renderer.domElement.addEventListener("pointerdown", onClick);

  if (reduced) {
    // one still frame of the final picture
    nodes.forEach((n) => (n.born = -1000));
    final();
    step(performance.now());
    if (live.current.phase === "playing") live.current.onDone?.();
  } else {
    if (live.current.phase === "done") final();
    raf = requestAnimationFrame(loop);
  }

  return () => {
    cancelAnimationFrame(raf);
    resize.disconnect();
    themeWatch.disconnect();
    renderer.domElement.removeEventListener("pointermove", onMove);
    renderer.domElement.removeEventListener("pointerleave", onLeave);
    renderer.domElement.removeEventListener("pointerdown", onClick);
    owned.forEach((o) => o.dispose());
    composer.dispose();
    renderer.dispose();
    renderer.domElement.remove();
    void packetX;
  };
}
