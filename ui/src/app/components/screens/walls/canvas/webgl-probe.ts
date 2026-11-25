/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Loads the walls view's 3D stack in its own chunk and draws one small frame off screen, so an end-to-end check can
// show that the stack runs with WebGL 2 under the console's Content-Security-Policy. Only the e2e build reaches this
// module, through webgl-probe-e2e-seam.ts, and nothing it draws is attached to the page.
import { createRoot, extend } from "@react-three/fiber";
import CameraControls from "camera-controls";
import { createElement } from "react";
import {
  Box3,
  BoxGeometry,
  CanvasTexture,
  Matrix4,
  Mesh,
  MeshBasicMaterial,
  PerspectiveCamera,
  Quaternion,
  Raycaster,
  Scene,
  ShaderMaterial,
  Sphere,
  Spherical,
  Vector2,
  Vector3,
  Vector4,
  WebGLRenderer,
} from "three";
import { EffectComposer } from "three/addons/postprocessing/EffectComposer.js";
import { OutputPass } from "three/addons/postprocessing/OutputPass.js";
import { RenderPass } from "three/addons/postprocessing/RenderPass.js";
import { UnrealBloomPass } from "three/addons/postprocessing/UnrealBloomPass.js";

export interface WebGLProbeResult {
  webgl2: boolean;
  drawn: boolean;
  steps: string[];
}

const SIZE = 64;

function offscreenCanvas(): HTMLCanvasElement {
  const canvas = document.createElement("canvas");
  canvas.width = SIZE;
  canvas.height = SIZE;
  return canvas;
}

export async function runWebGLProbe(): Promise<WebGLProbeResult> {
  const steps: string[] = [];
  const canvas = offscreenCanvas();
  const renderer = new WebGLRenderer({ canvas, antialias: false, preserveDrawingBuffer: true });
  try {
    renderer.setPixelRatio(1);
    renderer.setSize(SIZE, SIZE, false);
    const gl = renderer.getContext();
    const webgl2 = gl instanceof WebGL2RenderingContext;

    // A bundled GLSL shader sampling a texture painted on a canvas: no image URL and no eval.
    const paint = document.createElement("canvas");
    paint.width = 4;
    paint.height = 4;
    const ctx = paint.getContext("2d");
    if (ctx) {
      ctx.fillStyle = "white";
      ctx.fillRect(0, 0, 4, 4);
    }
    const material = new ShaderMaterial({
      uniforms: { uTex: { value: new CanvasTexture(paint) } },
      vertexShader:
        "varying vec2 vUv;\n" +
        "void main() { vUv = uv; gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0); }",
      fragmentShader: "uniform sampler2D uTex;\nvarying vec2 vUv;\nvoid main() { gl_FragColor = texture2D(uTex, vUv); }",
    });
    const scene = new Scene();
    scene.add(new Mesh(new BoxGeometry(2, 2, 2), material));
    const camera = new PerspectiveCamera(50, 1, 0.1, 100);
    camera.position.set(0, 0, 4);
    renderer.render(scene, camera);
    steps.push("shader");

    // The camera rig the walls view uses.
    CameraControls.install({ THREE: { Vector2, Vector3, Vector4, Quaternion, Matrix4, Spherical, Box3, Sphere, Raycaster } });
    const controls = new CameraControls(camera, canvas);
    await controls.setLookAt(0, 0, 4, 0, 0, 0, false);
    controls.update(1 / 60);
    controls.dispose();
    steps.push("controls");

    // three's own post-processing chain: bloom, then the output pass to the canvas.
    const composer = new EffectComposer(renderer);
    composer.addPass(new RenderPass(scene, camera));
    composer.addPass(new UnrealBloomPass(new Vector2(SIZE, SIZE), 0.6, 0.2, 0.8));
    composer.addPass(new OutputPass());
    composer.render();
    const pixel = new Uint8Array(4);
    gl.readPixels(SIZE / 2, SIZE / 2, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, pixel);
    composer.dispose();
    material.dispose();
    steps.push("bloom");

    // React Three Fiber: one mesh in its own canvas, the way the walls view mounts its scenes.
    extend({ Mesh, BoxGeometry, MeshBasicMaterial });
    const root = createRoot(offscreenCanvas());
    await root.configure({ frameloop: "never", dpr: 1, size: { width: SIZE, height: SIZE, top: 0, left: 0 } });
    root.render(
      createElement("mesh", null, createElement("boxGeometry"), createElement("meshBasicMaterial", { color: "white" })),
    );
    root.unmount();
    steps.push("fiber");

    return { webgl2, drawn: pixel[0] + pixel[1] + pixel[2] > 0, steps };
  } finally {
    renderer.dispose();
    renderer.forceContextLoss();
  }
}
