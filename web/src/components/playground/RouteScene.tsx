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
const POOL = 420;

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
    import("three")
      .then((THREE) => {
        if (dead) return;
        const off = build(THREE, el, live);
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

function build(THREE: Three, el: HTMLElement, live: Live): (() => void) | null {
  let renderer: InstanceType<Three["WebGLRenderer"]>;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
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

  // --- colours from the theme tokens ---
  const col = { ok: new THREE.Color(), fail: new THREE.Color(), mute: new THREE.Color(), line: new THREE.Color(), text: new THREE.Color(), bg: new THREE.Color(), accent: new THREE.Color() };
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
    light = col.bg.r + col.bg.g + col.bg.b > 1.5;
  };
  readColors();
  const themeWatch = new MutationObserver(() => {
    readColors();
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
  const owned: { dispose(): void }[] = [glowTex, disc, ring, wave, bar, barL];
  const mat = <T extends InstanceType<Three["Material"]>>(m: T) => (owned.push(m), m);

  // --- nodes ---
  type N = { data: SceneNode; x: number; y: number; ringM: InstanceType<Three["MeshBasicMaterial"]>; fillM: InstanceType<Three["MeshBasicMaterial"]>; glow: InstanceType<Three["Sprite"]>; group: InstanceType<Three["Group"]>; shown: SceneNode["state"]; born: number; shake: number; hover: number };
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
      group.add(glow, new THREE.Mesh(disc, mat(new THREE.MeshBasicMaterial({ color: col.bg }))), new THREE.Mesh(disc, fillM), new THREE.Mesh(ring, ringM));
      scene.add(group);
      nodes.push({ data: d, x: 0, y: Y, ringM, fillM, glow, group, shown: d.state === "start" ? "start" : "plan", born: 120 + i * 90, shake: 0, hover: 0 });
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
  const spark = (x: number, y: number, color: InstanceType<Three["Color"]>, n: number, speed: number, gravity = 0) => {
    for (let k = 0; k < n; k++) {
      const i = pi++ % POOL;
      const a = Math.random() * Math.PI * 2;
      const s = speed * (0.35 + Math.random() * 0.65);
      pos[i * 3] = x;
      pos[i * 3 + 1] = y;
      vel[i * 2] = Math.cos(a) * s;
      vel[i * 2 + 1] = Math.sin(a) * s + gravity;
      colr[i * 3] = color.r;
      colr[i * 3 + 1] = color.g;
      colr[i * 3 + 2] = color.b;
      maxLife[i] = life[i] = 450 + Math.random() * 600;
    }
  };

  const stateColor = (s: SceneNode["state"]) => (s === "ok" ? col.ok : s === "fail" ? col.fail : s === "start" ? col.text : col.mute);
  function restyle() {
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
    renderer.domElement.style.width = `${w}px`;
    renderer.domElement.style.height = `${HEIGHT}px`;
    camera.right = w;
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
      pulse(n.x, n.y, col.fail, 66, 600);
      spark(n.x, n.y, col.fail, 46, 190);
    } else if (s === "ok") {
      pulse(n.x, n.y, col.ok, 84, 900);
      pulse(n.x, n.y, col.ok, 56, 600);
      spark(n.x, n.y, col.ok, 80, 230, 40);
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
    const dt = Math.min(now - last, 50);
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
    const el_ = now - phaseT;
    const clock = now - t0;

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
    renderer.render(scene, camera);
  };

  // pointer: hover and click on provider nodes
  const nodeAt = (e: PointerEvent) => {
    const r = renderer.domElement.getBoundingClientRect();
    const x = e.clientX - r.left, y = HEIGHT - (e.clientY - r.top);
    return nodes.findIndex((n, i) => i > 0 && n.data.provider && Math.hypot(n.x - x, n.y - y) < R + 10);
  };
  const onMove = (e: PointerEvent) => {
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
    renderer.dispose();
    renderer.domElement.remove();
    void packetX;
  };
}
