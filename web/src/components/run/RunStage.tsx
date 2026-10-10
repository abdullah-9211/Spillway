"use client";

import { useEffect, useRef, useState } from "react";
import { GEO, type Layout, type LayoutNode, type RunGraph } from "@/lib/graph";
import { packetAt, reach, type Plan } from "@/lib/stage";

export const STAGE_H = 280;

export type Hover = { id: string; x: number; y: number } | null;

type Props = {
  layout: Layout;
  graph: RunGraph;
  plan: Plan;
  /** Replay position in ms, or null when showing the run as it stands now. */
  replayAt: number | null;
  selected: string | null;
  onSelect: (id: string) => void;
  onHover: (h: Hover) => void;
};

const PAD = 70;
const MODEL_Y = STAGE_H * 0.36;
const TOOL_Y = STAGE_H * 0.7;
const POOL = 700;

/**
 * The run as a scene: model calls are spinning crystals, tool calls are cubes, and a packet of light follows the run
 * from step to step. A step that finishes blooms green, one that stops cracks red and sparks, and where another worker
 * took over the stage flashes. It is decoration over words: every node is also a button in the graph below and a card in
 * the timeline. Without WebGL it says so and the graph below is the whole story; with reduced motion it draws one frame.
 */
export function RunStage(props: Props) {
  const host = useRef<HTMLDivElement>(null);
  const live = useRef(props);
  const [ok, setOk] = useState(true);
  const sig = props.layout.nodes.map((n) => n.id).join("|");

  useEffect(() => {
    live.current = props;
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
    // The scene is rebuilt when the set of nodes changes; their states and everything else are read from `live` each frame.
  }, [sig]);

  const { layout } = props;
  const first = layout.nodes[0];
  const lastNode = layout.nodes[layout.nodes.length - 1];
  const minX = first.x + GEO.nodeW / 2;
  const span = Math.max(lastNode.x + GEO.nodeW / 2 - minX, 1);
  return (
    <div className="stage" ref={host} style={{ height: STAGE_H }} role="presentation">
      {!ok && <p className="small stage__off">The 3D view needs WebGL. The graph below shows the same run.</p>}
      <ul className="stage__labels" aria-hidden="true">
        {layout.nodes.map((n) => {
          const frac = (n.x + GEO.nodeW / 2 - minX) / span;
          return (
            <li key={n.id} style={{ left: `calc(${PAD}px + (100% - ${2 * PAD}px) * ${frac})`, top: (n.kind === "tool" ? TOOL_Y : MODEL_Y) + 30 }} className={n.state}>
              <span>{n.label}</span>
              <span className="mute">{n.meta}</span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

type Live = { current: Props };
type Three = typeof import("three");
type Post = {
  EffectComposer: typeof import("three/examples/jsm/postprocessing/EffectComposer.js").EffectComposer;
  RenderPass: typeof import("three/examples/jsm/postprocessing/RenderPass.js").RenderPass;
  UnrealBloomPass: typeof import("three/examples/jsm/postprocessing/UnrealBloomPass.js").UnrealBloomPass;
  OutputPass: typeof import("three/examples/jsm/postprocessing/OutputPass.js").OutputPass;
};

const css = (el: HTMLElement, v: string, fb: string) => getComputedStyle(el).getPropertyValue(v).trim() || fb;

function build(THREE: Three, post: Post, el: HTMLElement, live: Live): (() => void) | null {
  let renderer: InstanceType<Three["WebGLRenderer"]>;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true });
  } catch {
    return null;
  }
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.domElement.className = "stage__canvas";
  renderer.domElement.setAttribute("aria-hidden", "true");
  el.prepend(renderer.domElement);
  const reduced = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;

  const scene = new THREE.Scene();
  const camera = new THREE.OrthographicCamera(0, 1, STAGE_H, 0, -50, 50);
  const composer = new post.EffectComposer(renderer);
  composer.addPass(new post.RenderPass(scene, camera));
  const bloom = new post.UnrealBloomPass(new THREE.Vector2(1, 1), 0.8, 0.6, 0.12);
  composer.addPass(bloom);
  composer.addPass(new post.OutputPass());

  // --- colours from the theme ---
  const C = { ok: new THREE.Color(), fail: new THREE.Color(), run: new THREE.Color(), mute: new THREE.Color(), text: new THREE.Color(), bg: new THREE.Color(), accent: new THREE.Color(), wait: new THREE.Color(), line: new THREE.Color() };
  let light = false;
  const readColors = () => {
    C.ok.set(css(el, "--ok", "#27a644"));
    C.fail.set(css(el, "--fail", "#c64545"));
    C.run.set(css(el, "--run", "#5db8a6"));
    C.mute.set(css(el, "--subtle", "#8a8f98"));
    C.text.set(css(el, "--text", "#f7f8f8"));
    C.bg.set(css(el, "--bg", "#010102"));
    C.accent.set(css(el, "--accent", "#5e6ad2"));
    C.wait.set(css(el, "--wait", "#e8a55a"));
    C.line.set(css(el, "--line2", "#34343a"));
    light = C.bg.r + C.bg.g + C.bg.b > 1.5;
    renderer.setClearColor(C.bg, 1);
    bloom.enabled = !light;
  };
  readColors();
  const themeWatch = new MutationObserver(readColors);
  themeWatch.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

  const owned: { dispose(): void }[] = [];
  const track = <T extends { dispose(): void }>(o: T): T => (owned.push(o), o);
  const glowTex = track(
    (() => {
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
    })(),
  );
  const blend = () => (light ? THREE.NormalBlending : THREE.AdditiveBlending);

  // --- scene objects, rebuilt for this set of nodes ---
  let W = Math.max(el.clientWidth, 320);
  const { layout, graph } = live.current;
  const first = layout.nodes[0];
  const last = layout.nodes[layout.nodes.length - 1];
  const minX = first.x + GEO.nodeW / 2;
  const span = Math.max(last.x + GEO.nodeW / 2 - minX, 1);
  const sx = (x: number) => PAD + ((x - minX) / span) * Math.max(W - 2 * PAD, 1);

  type N = {
    n: LayoutNode; // replaced each frame with the latest state of the same node
    group: InstanceType<Three["Group"]>;
    cage: InstanceType<Three["LineSegments"]>;
    cageM: InstanceType<Three["LineBasicMaterial"]>;
    glow: InstanceType<Three["Sprite"]>;
    ring: InstanceType<Three["Mesh"]>;
    ringM: InstanceType<Three["MeshBasicMaterial"]>;
    x: number;
    y: number;
    born: number;
    shake: number;
    hover: number;
    spin: number;
    lastReach: string;
  };
  const nodes: N[] = [];
  const icoEdges = track(new THREE.EdgesGeometry(new THREE.IcosahedronGeometry(15, 0)));
  const boxEdges = track(new THREE.EdgesGeometry(new THREE.BoxGeometry(24, 24, 24)));
  const octEdges = track(new THREE.EdgesGeometry(new THREE.OctahedronGeometry(14, 0)));
  const ringGeo = track(new THREE.RingGeometry(21, 22.5, 48));

  layout.nodes.forEach((n, i) => {
    const group = new THREE.Group();
    const cageM = track(new THREE.LineBasicMaterial({ transparent: true }));
    const cage = new THREE.LineSegments(n.kind === "goal" ? octEdges : n.kind === "tool" ? boxEdges : icoEdges, cageM);
    const gm = track(new THREE.SpriteMaterial({ map: glowTex, transparent: true, depthWrite: false, opacity: 0 }));
    const glow = new THREE.Sprite(gm);
    glow.scale.set(96, 96, 1);
    const ringM = track(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0.8, side: THREE.DoubleSide }));
    const ring = new THREE.Mesh(ringGeo, ringM);
    group.add(glow, ring, cage);
    scene.add(group);
    nodes.push({ n, group, cage, cageM, glow, ring, ringM, x: 0, y: n.kind === "tool" ? TOOL_Y : MODEL_Y, born: 150 + i * 70, shake: 0, hover: 0, spin: 0.6, lastReach: "" });
  });
  const byId = new Map(nodes.map((o) => [o.n.id, o]));

  // the path the packet follows: a smooth line between consecutive nodes
  const SEG = 28;
  const pathPts: { x: number; y: number }[][] = [];
  const linkLines: InstanceType<Three["Line"]>[] = [];
  const linkMats: InstanceType<Three["LineBasicMaterial"]>[] = [];
  for (let i = 1; i < nodes.length; i++) {
    const m = track(new THREE.LineBasicMaterial({ transparent: true, opacity: 0.8 }));
    const g = track(new THREE.BufferGeometry());
    g.setAttribute("position", new THREE.BufferAttribute(new Float32Array((SEG + 1) * 3), 3));
    const l = new THREE.Line(g, m);
    l.frustumCulled = false;
    scene.add(l);
    linkLines.push(l);
    linkMats.push(m);
    pathPts.push([]);
  }
  const curve = (a: N, b: N, u: number) => {
    const e = u * u * (3 - 2 * u); // smoothstep: leaves and arrives gently
    return { x: a.x + (b.x - a.x) * u, y: a.y + (b.y - a.y) * e };
  };

  // bands and cuts
  const bandMeshes = layout.bands.map((b) => {
    const m = track(new THREE.MeshBasicMaterial({ transparent: true, opacity: b.tint === "base" ? 0.05 : 0.07, depthWrite: false }));
    const mesh = new THREE.Mesh(track(new THREE.PlaneGeometry(1, 1)), m);
    mesh.position.z = -10;
    scene.add(mesh);
    return { b, mesh, m };
  });
  const cutLines = layout.cuts.map((c) => {
    const g = track(new THREE.BufferGeometry());
    g.setAttribute("position", new THREE.BufferAttribute(new Float32Array([0, 0, 0, 0, STAGE_H, 0]), 3));
    const m = track(new THREE.LineDashedMaterial({ dashSize: 6, gapSize: 6, transparent: true, opacity: 0.7 }));
    const l = new THREE.Line(g, m);
    l.computeLineDistances();
    scene.add(l);
    return { c, l, m };
  });

  // packet
  const packetGlow = new THREE.Sprite(track(new THREE.SpriteMaterial({ map: glowTex, transparent: true, depthWrite: false })));
  packetGlow.scale.set(54, 54, 1);
  const packetCore = new THREE.Mesh(track(new THREE.CircleGeometry(5, 24)), track(new THREE.MeshBasicMaterial({ transparent: true })));
  scene.add(packetGlow, packetCore);
  const TAIL = 30;
  const tailPos = new Float32Array(TAIL * 3);
  const tailCol = new Float32Array(TAIL * 3);
  const tailGeo = track(new THREE.BufferGeometry());
  tailGeo.setAttribute("position", new THREE.BufferAttribute(tailPos, 3));
  tailGeo.setAttribute("color", new THREE.BufferAttribute(tailCol, 3));
  const tailMat = track(new THREE.LineBasicMaterial({ vertexColors: true, transparent: true }));
  const tail = new THREE.Line(tailGeo, tailMat);
  tail.frustumCulled = false;
  scene.add(tail);
  const trail: [number, number][] = [];

  // particles and shockwaves
  const pos = new Float32Array(POOL * 3);
  const col = new Float32Array(POOL * 3);
  const vel = new Float32Array(POOL * 2);
  const life = new Float32Array(POOL);
  const maxLife = new Float32Array(POOL);
  const pGeo = track(new THREE.BufferGeometry());
  pGeo.setAttribute("position", new THREE.BufferAttribute(pos, 3));
  pGeo.setAttribute("color", new THREE.BufferAttribute(col, 3));
  const pMat = track(new THREE.PointsMaterial({ size: 4.5, vertexColors: true, transparent: true, depthWrite: false, sizeAttenuation: false }));
  const points = new THREE.Points(pGeo, pMat);
  points.frustumCulled = false;
  scene.add(points);
  let pi = 0;
  const spark = (x: number, y: number, color: InstanceType<Three["Color"]> | InstanceType<Three["Color"]>[], count: number, speed: number, gravity = 0) => {
    for (let k = 0; k < count; k++) {
      const i = pi++ % POOL;
      const a = Math.random() * Math.PI * 2;
      const s = speed * (0.3 + Math.random() * 0.7);
      pos[i * 3] = x;
      pos[i * 3 + 1] = y;
      pos[i * 3 + 2] = 3;
      vel[i * 2] = Math.cos(a) * s;
      vel[i * 2 + 1] = Math.sin(a) * s + gravity;
      const c = Array.isArray(color) ? color[Math.floor(Math.random() * color.length)] : color;
      col[i * 3] = c.r;
      col[i * 3 + 1] = c.g;
      col[i * 3 + 2] = c.b;
      maxLife[i] = life[i] = 450 + Math.random() * 650;
    }
  };
  const waveGeo = track(new THREE.RingGeometry(0.92, 1, 64));
  const waves = Array.from({ length: 8 }, () => {
    const m = track(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0, side: THREE.DoubleSide }));
    const mesh = new THREE.Mesh(waveGeo, m);
    mesh.visible = false;
    scene.add(mesh);
    return { mesh, m, t: 0, life: 0, size: 0 };
  });
  let wi = 0;
  const pulse = (x: number, y: number, color: InstanceType<Three["Color"]>, size = 80, ms = 800) => {
    const w = waves[wi++ % waves.length];
    w.mesh.position.set(x, y, 1);
    w.m.color.copy(color);
    Object.assign(w, { t: 0, life: ms, size });
    w.mesh.visible = true;
  };
  // lightning along a cut
  const BOLT = 16;
  const bolts = Array.from({ length: 4 }, () => {
    const g = track(new THREE.BufferGeometry());
    g.setAttribute("position", new THREE.BufferAttribute(new Float32Array(BOLT * 3), 3));
    const m = track(new THREE.LineBasicMaterial({ transparent: true }));
    const l = new THREE.Line(g, m);
    l.frustumCulled = false;
    l.visible = false;
    scene.add(l);
    return { g, m, l, life: 0, x: 0, redraw: 0 };
  });
  let bi = 0;
  const drawBolt = (b: (typeof bolts)[number]) => {
    const a = b.g.attributes.position.array as Float32Array;
    for (let i = 0; i < BOLT; i++) {
      const t = i / (BOLT - 1);
      a[i * 3] = b.x + (i === 0 || i === BOLT - 1 ? 0 : (Math.random() - 0.5) * 22);
      a[i * 3 + 1] = STAGE_H * t;
      a[i * 3 + 2] = 2;
    }
    b.g.attributes.position.needsUpdate = true;
  };
  const strike = (x: number, ms = 600) => {
    const b = bolts[bi++ % bolts.length];
    Object.assign(b, { x, life: ms, redraw: 0 });
    b.m.color.copy(C.fail);
    b.l.visible = true;
    drawBolt(b);
  };

  // layout to the real width
  const place = () => {
    nodes.forEach((o, i) => {
      o.x = sx(o.n.x + GEO.nodeW / 2);
      o.y = o.n.kind === "tool" ? TOOL_Y : MODEL_Y;
      o.group.position.set(o.x, o.y, 0);
      if (i > 0) {
        const a = nodes[i - 1];
        const attr = linkLines[i - 1].geometry.attributes.position;
        const arr = attr.array as Float32Array;
        for (let k = 0; k <= SEG; k++) {
          const p = curve(a, o, k / SEG);
          arr[k * 3] = p.x;
          arr[k * 3 + 1] = p.y;
          arr[k * 3 + 2] = -1;
        }
        attr.needsUpdate = true;
      }
    });
    bandMeshes.forEach(({ b, mesh }) => {
      const x0 = Math.max(0, sx(b.x + GEO.nodeW / 2) - 0);
      const x1 = Math.min(W, b.x + b.w >= last.x + GEO.nodeW + 40 ? W : sx(b.x + b.w + GEO.nodeW / 2));
      mesh.position.x = (x0 + x1) / 2;
      mesh.position.y = STAGE_H / 2;
      mesh.scale.set(Math.max(x1 - x0, 1), STAGE_H, 1);
    });
    cutLines.forEach(({ c, l }) => {
      l.position.x = sx(c.x + GEO.nodeW / 2);
    });
  };
  const resize = new ResizeObserver(() => {
    W = Math.max(el.clientWidth, 320);
    renderer.setSize(W, STAGE_H, false);
    renderer.domElement.style.width = `${W}px`;
    renderer.domElement.style.height = `${STAGE_H}px`;
    composer.setSize(W, STAGE_H);
    bloom.resolution.set(W, STAGE_H);
    camera.right = W;
    camera.updateProjectionMatrix();
    place();
  });
  resize.observe(el);

  // --- the loop ---
  const tone = (n: LayoutNode, r: string, replaying: boolean) => {
    if (replaying && r === "ahead") return C.mute;
    if (n.kind === "goal") return C.accent;
    if (n.state === "stopped" || n.state === "failed") return C.fail;
    if (n.state === "running") return C.run;
    return n.reissued ? C.run : C.ok;
  };
  const t0 = performance.now();
  let last_ = t0;
  let raf = 0;
  let packetIdx = Math.max(nodes.length - 1, 0);
  let flash = 0;
  let shakeCam = 0;
  const prevStates = new Map<string, string>();
  let prevStatus = graph.run.status;
  const mouse = { x: 0.5, y: 0.5 };

  const step = (now: number) => {
    const dt = Math.max(0, Math.min(now - last_, 50));
    last_ = now;
    const clock = Math.max(0, now - t0);
    const { replayAt, selected, graph: g, layout: lay, plan } = live.current;
    bandMeshes.forEach(({ b, m }) => m.color.copy(b.tint === "base" ? C.mute : C.run));
    const latest = new Map(lay.nodes.map((x) => [x.id, x]));
    const replaying = replayAt !== null;
    const runLive = ["queued", "running", "waiting_tool", "waiting_human", "sleeping"].includes(g.run.status);

    // nodes
    nodes.forEach((o, i) => {
      o.n = latest.get(o.n.id) ?? o.n;
      const st = plan.steps[i];
      const r = replaying && st ? reach(st, replayAt as number) : "done";
      const age = clock - o.born;
      const k = age <= 0 ? 0 : Math.min(age / 450, 1);
      const popBack = k === 0 ? 0 : 1 + 2.2 * Math.pow(k - 1, 3) + 1.2 * Math.pow(k - 1, 2);
      const base = replaying && r === "ahead" ? 0.55 : 1;
      o.hover += ((o.hover > 0.5 ? 1 : 0) - o.hover) * 0.2;
      o.shake = Math.max(0, o.shake - dt / 450);
      const color = tone(o.n, r, replaying);
      o.cageM.color.copy(color);
      o.ringM.color.copy(color);
      (o.glow.material as InstanceType<Three["SpriteMaterial"]>).color.copy(color);
      o.cageM.opacity = replaying && r === "ahead" ? 0.25 : 0.95;
      o.ringM.opacity = (replaying && r === "ahead" ? 0.15 : o.n.state === "running" || r === "active" ? 0.9 : 0.55) * (o.n.id === selected ? 1 : 0.85);
      const glowT = replaying && r === "ahead" ? 0 : o.n.id === selected ? 0.6 : o.n.state === "running" || r === "active" ? 0.5 : o.n.state === "stopped" || o.n.state === "failed" ? 0.35 : 0.22;
      const gm = o.glow.material as InstanceType<Three["SpriteMaterial"]>;
      gm.blending = blend();
      gm.opacity += (glowT * (light ? 0.7 : 1) - gm.opacity) * 0.12;
      const sel = o.n.id === selected ? 1.18 : 1;
      const pulseK = o.n.state === "running" || r === "active" ? 1 + 0.08 * Math.sin(clock / 220) : 1;
      o.group.scale.setScalar(Math.max(popBack, 0.0001) * base * sel * pulseK * (1 + o.hover * 0.12));
      const jitter = o.n.state === "stopped" ? (Math.random() - 0.5) * 2.2 : Math.sin(clock * 0.09) * o.shake * 7;
      o.group.position.set(o.x + jitter, o.y + Math.sin(clock / 900 + i) * 1.6, 0);
      const want = o.n.state === "running" || r === "active" ? 1.8 : o.n.state === "stopped" ? 0.2 : 0.6;
      o.spin += (want - o.spin) * Math.min(1, dt / 400);
      o.cage.rotation.x += (o.spin * dt) / 1000;
      o.cage.rotation.y += (o.spin * 1.3 * dt) / 1000;
      o.ring.rotation.z += (dt / 1000) * (o.n.reissued ? -0.8 : 0.3);
      if (o.n.state === "stopped" && Math.random() < dt / 160) spark(o.x, o.y, C.fail, 1, 70);
      if (o.n.state === "running" && Math.random() < dt / 90) {
        const a = clock / 300 + i;
        spark(o.x + Math.cos(a) * 24, o.y + Math.sin(a) * 24, C.run, 1, 20);
      }
      // moments: a step reached in a replay, or a step whose state just changed live
      const key = `${r}/${o.n.state}`;
      if (o.lastReach && o.lastReach !== key) {
        if (r === "active") {
          pulse(o.x, o.y, color, 60, 600);
          spark(o.x, o.y, color, 22, 150);
        }
        if (r === "done" && replaying && o.n.state === "finished") spark(o.x, o.y, [C.ok, C.text], 16, 170, 20);
        if (o.n.state === "stopped" && r !== "ahead") {
          pulse(o.x, o.y, C.fail, 90, 700);
          spark(o.x, o.y, C.fail, 40, 220);
          o.shake = 1;
          shakeCam = 0.8;
        }
      }
      o.lastReach = key;
    });

    // recoveries in a replay: the cut flashes when the replay reaches the step after it
    if (replaying) {
      plan.steps.forEach((s, i) => {
        if (!s.recoveryBefore) return;
        const was = (live as unknown as { _rec?: Set<number> })._rec ?? ((live as unknown as { _rec: Set<number> })._rec = new Set());
        if ((replayAt as number) >= s.start - 900 && !was.has(i)) {
          was.add(i);
          const c = cutLines[Math.min(cutLines.length - 1, Math.max(0, was.size - 1))];
          if (c) {
            const x = sx(c.c.x + GEO.nodeW / 2);
            strike(x, 900);
            pulse(x, STAGE_H / 2, C.fail, 140, 900);
            flash = 0.9;
            shakeCam = 1;
          }
        }
        if ((replayAt as number) < s.start - 1500) (live as unknown as { _rec?: Set<number> })._rec?.delete(i);
      });
    }

    // cuts hum with lightning
    cutLines.forEach(({ c, m }) => {
      m.color.copy(C.fail);
      if (Math.random() < dt / 500) strike(sx(c.x + GEO.nodeW / 2), 220);
    });

    // links
    linkMats.forEach((m, i) => {
      const a = nodes[i];
      const b = nodes[i + 1];
      const redo = b.n.reissued && a.n.node?.step_no === b.n.node?.step_no;
      const st = plan.steps[i + 1];
      const ahead = replaying && st && reach(st, replayAt as number) === "ahead";
      m.color.copy(redo ? C.run : b.n.state === "running" ? C.run : C.mute);
      m.opacity = ahead ? 0.15 : redo ? 0.9 : 0.55;
    });

    // packet
    let target = { x: nodes[0].x, y: nodes[0].y };
    if (replaying) {
      const pk = packetAt(plan, replayAt as number);
      const b = nodes[pk.index];
      const a = nodes[Math.max(pk.index - 1, 0)];
      target = pk.index === 0 ? { x: b.x, y: b.y } : curve(a, b, pk.u);
      packetIdx = pk.index;
    } else {
      const runningIdx = g.nodes.findIndex((n) => n.state === "running");
      const idx = runLive ? (runningIdx >= 0 ? runningIdx + 1 : nodes.length - 1) : nodes.length - 1;
      const dest = nodes[Math.min(idx, nodes.length - 1)];
      const prev = nodes[Math.max(Math.min(idx, nodes.length - 1) - 1, 0)];
      // ride back and forth along the last link while a step runs, so a live run looks alive
      const u = runLive && runningIdx >= 0 ? (Math.sin(clock / 520 - Math.PI / 2) + 1) / 2 : 1;
      target = dest === prev ? { x: dest.x, y: dest.y } : curve(prev, dest, u);
      packetIdx = Math.min(idx, nodes.length - 1);
    }
    const dx = target.x;
    const dy = target.y;
    packetCore.position.set(dx, dy, 2);
    packetGlow.position.set(dx, dy, 1.5);
    (packetCore.material as InstanceType<Three["MeshBasicMaterial"]>).color.copy(light ? C.accent : C.text);
    const gm = packetGlow.material as InstanceType<Three["SpriteMaterial"]>;
    gm.color.copy(replaying ? C.accent : runLive ? C.run : C.ok);
    gm.blending = blend();
    gm.opacity = (light ? 0.55 : 0.9) * (0.75 + 0.25 * Math.sin(clock / 150)) * (!replaying && !runLive ? 0.6 : 1);
    trail.unshift([dx, dy]);
    while (trail.length > TAIL) trail.pop();
    for (let i = 0; i < TAIL; i++) {
      const p = trail[Math.min(i, trail.length - 1)] ?? [dx, dy];
      tailPos[i * 3] = p[0];
      tailPos[i * 3 + 1] = p[1];
      tailPos[i * 3 + 2] = 1;
      const f = Math.pow(1 - i / TAIL, 2) * (i < trail.length ? 1 : 0);
      const c = replaying ? C.accent : runLive ? C.run : C.ok;
      tailCol[i * 3] = c.r * f;
      tailCol[i * 3 + 1] = c.g * f;
      tailCol[i * 3 + 2] = c.b * f;
    }
    tailGeo.attributes.position.needsUpdate = true;
    tailGeo.attributes.color.needsUpdate = true;
    tailMat.blending = blend();
    if (runLive && !replaying && Math.random() < dt / 30) spark(dx, dy, C.run, 1, 30);

    // changes in the live data: new steps pop in, a run that ends celebrates or mourns
    g.nodes.forEach((n) => {
      const id = `${n.step_no}:${n.epoch}`;
      const o = byId.get(id);
      const was = prevStates.get(id);
      if (o && was && was !== n.state) {
        if (n.state === "finished") {
          pulse(o.x, o.y, C.ok, 70, 700);
          spark(o.x, o.y, [C.ok, C.text], 26, 190, 25);
        } else if (n.state === "failed" || n.state === "stopped") {
          pulse(o.x, o.y, C.fail, 80, 700);
          spark(o.x, o.y, C.fail, 36, 200);
          shakeCam = 0.7;
        }
      }
      prevStates.set(id, n.state);
    });
    if (g.run.status !== prevStatus) {
      if (g.run.status === "succeeded") {
        for (let k = 0; k < 6; k++) spark(W * (0.15 + 0.14 * k), STAGE_H * 0.9, [C.ok, C.accent, C.wait, C.text], 40, 260, 120);
        flash = 1;
      } else if (g.run.status === "failed" || g.run.status === "cancelled") {
        flash = 0.8;
        shakeCam = 1;
      }
      prevStatus = g.run.status;
    }

    // ambience
    flash = Math.max(0, flash - dt / 600);
    shakeCam = Math.max(0, shakeCam - dt / 400);
    bloom.strength = 0.8 + flash * 1.2;
    camera.position.x = (Math.random() - 0.5) * shakeCam * 6 + (mouse.x - 0.5) * 6;
    camera.position.y = (Math.random() - 0.5) * shakeCam * 4 + (0.5 - mouse.y) * 4;

    waves.forEach((w) => {
      if (!w.mesh.visible) return;
      w.t += dt;
      const k = w.t / w.life;
      if (k >= 1) {
        w.mesh.visible = false;
        return;
      }
      w.mesh.scale.setScalar(20 + (w.size - 20) * (1 - Math.pow(1 - k, 3)));
      w.m.opacity = 0.8 * (1 - k);
    });
    bolts.forEach((b) => {
      if (!b.l.visible) return;
      b.life -= dt;
      if (b.life <= 0) {
        b.l.visible = false;
        return;
      }
      b.redraw -= dt;
      if (b.redraw <= 0) {
        drawBolt(b);
        b.redraw = 50;
      }
      b.m.opacity = Math.min(1, b.life / 150);
    });
    pMat.blending = blend();
    for (let i = 0; i < POOL; i++) {
      if (life[i] <= 0) {
        pos[i * 3 + 2] = 100;
        continue;
      }
      life[i] -= dt;
      pos[i * 3] += (vel[i * 2] * dt) / 1000;
      pos[i * 3 + 1] += (vel[i * 2 + 1] * dt) / 1000;
      vel[i * 2] *= 0.985;
      vel[i * 2 + 1] = vel[i * 2 + 1] * 0.985 - dt * 0.03;
      if (life[i] / maxLife[i] < 0.3) {
        col[i * 3] *= 0.93;
        col[i * 3 + 1] *= 0.93;
        col[i * 3 + 2] *= 0.93;
      }
    }
    pGeo.attributes.position.needsUpdate = true;
    pGeo.attributes.color.needsUpdate = true;
    void packetIdx;
    composer.render();
  };
  const loop = (now: number) => {
    raf = requestAnimationFrame(loop);
    step(now);
  };

  // pointer: hover and click
  const pick = (e: PointerEvent) => {
    const r = renderer.domElement.getBoundingClientRect();
    const x = e.clientX - r.left;
    const y = STAGE_H - (e.clientY - r.top);
    let best = -1;
    let bd = 34;
    nodes.forEach((o, i) => {
      if (o.n.kind === "goal") return;
      const d = Math.hypot(o.x - x, o.y - y);
      if (d < bd) {
        bd = d;
        best = i;
      }
    });
    return best;
  };
  const onMove = (e: PointerEvent) => {
    const r = renderer.domElement.getBoundingClientRect();
    mouse.x = (e.clientX - r.left) / Math.max(r.width, 1);
    mouse.y = (e.clientY - r.top) / Math.max(r.height, 1);
    const i = pick(e);
    nodes.forEach((o, k) => (o.hover = k === i ? 1 : 0));
    renderer.domElement.style.cursor = i >= 0 ? "pointer" : "";
    const o = i >= 0 ? nodes[i] : null;
    onHoverRef(o ? { id: o.n.id, x: o.x, y: STAGE_H - o.y } : null);
  };
  const onHoverRef = (h: Hover) => (live.current as Props).onHover(h);
  const onLeave = () => {
    nodes.forEach((o) => (o.hover = 0));
    onHoverRef(null);
  };
  const onDown = (e: PointerEvent) => {
    const i = pick(e);
    if (i >= 0) (live.current as Props).onSelect(nodes[i].n.id);
  };
  renderer.domElement.addEventListener("pointermove", onMove);
  renderer.domElement.addEventListener("pointerleave", onLeave);
  renderer.domElement.addEventListener("pointerdown", onDown);

  // first frame state, so a re-render does not replay the intro for nodes already settled
  graph.nodes.forEach((n) => prevStates.set(`${n.step_no}:${n.epoch}`, n.state));
  if (reduced) {
    nodes.forEach((o) => (o.born = -1000));
    step(performance.now() + 600);
  } else {
    raf = requestAnimationFrame(loop);
  }

  return () => {
    cancelAnimationFrame(raf);
    resize.disconnect();
    themeWatch.disconnect();
    renderer.domElement.removeEventListener("pointermove", onMove);
    renderer.domElement.removeEventListener("pointerleave", onLeave);
    renderer.domElement.removeEventListener("pointerdown", onDown);
    owned.forEach((o) => o.dispose());
    composer.dispose();
    renderer.dispose();
    renderer.domElement.remove();
  };
}
