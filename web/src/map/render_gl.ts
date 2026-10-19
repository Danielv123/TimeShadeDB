// WebGL2 terrain renderer (PLAN.md §4.7). z0 tiles are R8UI (pal8) or R16UI (pal16)
// palette-index textures; the fragment shader looks colours up in a 256x256 RGBA palette
// texture (65,536 entries). Coarse tiles are R16UI RGB565, expanded in the shader with the
// same bit replication as FORMAT.md 3. Integer textures, so sampling is always nearest.

import type { DecodedDelta } from "../api/types";
import type { Viewport } from "./layer";
import {
  blockOrigin,
  FORMAT_PAL16,
  FORMAT_PAL8,
  FORMAT_RGB565,
  snapRect,
  TILE,
  textureBytes,
  type DrawCommand,
  type TexturePixels,
  type TileRenderer,
  type TileTexture,
} from "./renderer.ts";

const VERT = `#version 300 es
in vec2 a_pos;
uniform vec4 u_dst;
uniform vec4 u_src;
out vec2 v_uv;
void main() {
  v_uv = mix(u_src.xy, u_src.zw, a_pos);
  vec2 p = u_dst.xy + a_pos * u_dst.zw;
  gl_Position = vec4(p, 0.0, 1.0);
}`;

const FRAG = `#version 300 es
precision highp float;
precision highp int;
precision highp usampler2D;
uniform usampler2D u_tile;
uniform sampler2D u_pal;
uniform int u_mode;
in vec2 v_uv;
out vec4 o_color;
void main() {
  ivec2 p = clamp(ivec2(floor(v_uv)), ivec2(0), ivec2(${TILE - 1}));
  uint i = texelFetch(u_tile, p, 0).r;
  if (u_mode == 1) {
    if (i == 0u) { o_color = vec4(0.0, 0.0, 0.0, 1.0); return; }
    uint r = (i >> 11u) & 31u;
    uint g = (i >> 5u) & 63u;
    uint b = i & 31u;
    uvec3 c = uvec3((r << 3u) | (r >> 2u), (g << 2u) | (g >> 4u), (b << 3u) | (b >> 2u));
    o_color = vec4(vec3(c) / 255.0, 1.0);
  } else {
    o_color = texelFetch(u_pal, ivec2(int(i & 255u), int(i >> 8u)), 0);
  }
}`;

interface GLTexture extends TileTexture {
  tex: WebGLTexture | null;
  wide: boolean;
}

export class GLRenderer implements TileRenderer {
  readonly kind = "webgl2" as const;
  onLost?: () => void;
  private readonly canvas: HTMLCanvasElement;
  private gl: WebGL2RenderingContext;
  private prog!: WebGLProgram;
  private vao!: WebGLVertexArrayObject;
  private palTex!: WebGLTexture;
  private uDst!: WebGLUniformLocation;
  private uSrc!: WebGLUniformLocation;
  private uMode!: WebGLUniformLocation;
  private palette = new Uint16Array(0);
  private readonly palRGBA = new Uint8Array(256 * 256 * 4);
  private lost = false;
  private textures = new Set<GLTexture>();

  /** Throws when WebGL2 is not available. */
  constructor(canvas: HTMLCanvasElement) {
    this.canvas = canvas;
    const gl = canvas.getContext("webgl2", {
      alpha: false,
      antialias: false,
      depth: false,
      stencil: false,
      premultipliedAlpha: false,
      preserveDrawingBuffer: false,
      powerPreference: "high-performance",
    });
    if (!gl) {
      throw new Error("WebGL2 is not available");
    }
    this.gl = gl;
    this.init();
    canvas.addEventListener("webglcontextlost", this.handleLost, false);
    canvas.addEventListener("webglcontextrestored", this.handleRestored, false);
  }

  get paletteLength(): number {
    return this.palette.length;
  }

  /** The WebGL2 context, for layers that draw after terrain (MapLayer.drawGL). */
  get context(): WebGL2RenderingContext {
    return this.gl;
  }

  private handleLost = (e: Event): void => {
    e.preventDefault();
    this.lost = true;
    for (const t of this.textures) {
      t.tex = null;
    }
    this.textures.clear();
    this.onLost?.();
  };

  private handleRestored = (): void => {
    this.lost = false;
    this.init();
    const pal = this.palette;
    this.palette = new Uint16Array(0);
    this.setPalette(pal);
  };

  private init(): void {
    const gl = this.gl;
    const sh = (type: number, src: string): WebGLShader => {
      const s = gl.createShader(type)!;
      gl.shaderSource(s, src);
      gl.compileShader(s);
      if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) {
        throw new Error(`shader: ${gl.getShaderInfoLog(s)}`);
      }
      return s;
    };
    const prog = gl.createProgram()!;
    gl.attachShader(prog, sh(gl.VERTEX_SHADER, VERT));
    gl.attachShader(prog, sh(gl.FRAGMENT_SHADER, FRAG));
    gl.bindAttribLocation(prog, 0, "a_pos");
    gl.linkProgram(prog);
    if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) {
      throw new Error(`program: ${gl.getProgramInfoLog(prog)}`);
    }
    this.prog = prog;
    this.uDst = gl.getUniformLocation(prog, "u_dst")!;
    this.uSrc = gl.getUniformLocation(prog, "u_src")!;
    this.uMode = gl.getUniformLocation(prog, "u_mode")!;
    gl.useProgram(prog);
    gl.uniform1i(gl.getUniformLocation(prog, "u_tile"), 0);
    gl.uniform1i(gl.getUniformLocation(prog, "u_pal"), 1);

    this.vao = gl.createVertexArray()!;
    gl.bindVertexArray(this.vao);
    const buf = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, buf);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([0, 0, 1, 0, 0, 1, 1, 1]), gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);

    this.palTex = gl.createTexture()!;
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, this.palTex);
    gl.texStorage2D(gl.TEXTURE_2D, 1, gl.RGBA8, 256, 256);
    this.texParams();
    gl.activeTexture(gl.TEXTURE0);
    gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1);
    gl.disable(gl.BLEND);
    gl.disable(gl.DEPTH_TEST);
  }

  private texParams(): void {
    const gl = this.gl;
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  }

  setPalette(colors: Uint16Array): void {
    const old = this.palette.length;
    const n = Math.min(colors.length, 65536);
    // Palettes are append-only: only entries from the old length on are new, unless the
    // palette was reset (old length 0).
    const from = old <= n ? old : 0;
    const rgba = this.palRGBA;
    for (let i = from; i < n; i++) {
      const c = colors[i];
      const r5 = (c >> 11) & 31;
      const g6 = (c >> 5) & 63;
      const b5 = c & 31;
      const o = i * 4;
      rgba[o] = (r5 << 3) | (r5 >> 2);
      rgba[o + 1] = (g6 << 2) | (g6 >> 4);
      rgba[o + 2] = (b5 << 3) | (b5 >> 2);
      rgba[o + 3] = 255;
    }
    if (n > 0) {
      rgba[0] = rgba[1] = rgba[2] = 0;
      rgba[3] = 255;
    }
    this.palette = colors.slice(0, n);
    if (this.lost) {
      return;
    }
    const gl = this.gl;
    const row0 = from >> 8;
    const rows = Math.ceil(n / 256) - row0;
    if (rows > 0) {
      gl.activeTexture(gl.TEXTURE1);
      gl.bindTexture(gl.TEXTURE_2D, this.palTex);
      gl.texSubImage2D(gl.TEXTURE_2D, 0, 0, row0, 256, rows, gl.RGBA, gl.UNSIGNED_BYTE, rgba.subarray(row0 * 1024, (row0 + rows) * 1024));
      gl.activeTexture(gl.TEXTURE0);
    }
  }

  resetPalette(): void {
    this.palette = new Uint16Array(0);
    this.palRGBA.fill(0);
  }

  createTexture(p: TexturePixels): TileTexture {
    const wide = p.format !== FORMAT_PAL8;
    const t: GLTexture = { tex: null, wide, format: p.format, bytes: textureBytes(p.format, false) };
    if (this.lost) {
      return t;
    }
    const gl = this.gl;
    gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1);
    const tex = gl.createTexture()!;
    gl.bindTexture(gl.TEXTURE_2D, tex);
    gl.texStorage2D(gl.TEXTURE_2D, 1, wide ? gl.R16UI : gl.R8UI, TILE, TILE);
    this.texParams();
    gl.texSubImage2D(gl.TEXTURE_2D, 0, 0, 0, TILE, TILE, gl.RED_INTEGER, wide ? gl.UNSIGNED_SHORT : gl.UNSIGNED_BYTE, p.pixels);
    t.tex = tex;
    this.textures.add(t);
    return t;
  }

  applyDelta(handle: TileTexture, delta: DecodedDelta): boolean {
    const t = handle as GLTexture;
    if (this.lost || !t.tex || t.format === FORMAT_RGB565 || delta.header.format === FORMAT_RGB565) {
      return false;
    }
    const deltaWide = delta.header.format === FORMAT_PAL16;
    if (deltaWide && !t.wide) {
      return false;
    }
    const gl = this.gl;
    gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1);
    gl.bindTexture(gl.TEXTURE_2D, t.tex);
    for (const e of delta.entries) {
      const [x, y] = blockOrigin(e.block);
      let px: Uint8Array | Uint16Array = e.pixels;
      if (t.wide && !deltaWide) {
        px = Uint16Array.from(e.pixels);
      }
      gl.texSubImage2D(gl.TEXTURE_2D, 0, x, y, 32, 32, gl.RED_INTEGER, t.wide ? gl.UNSIGNED_SHORT : gl.UNSIGNED_BYTE, px);
    }
    return true;
  }

  deleteTexture(handle: TileTexture): void {
    const t = handle as GLTexture;
    if (t.tex && !this.lost) {
      this.gl.deleteTexture(t.tex);
    }
    t.tex = null;
    this.textures.delete(t);
  }

  render(vp: Viewport, draws: readonly DrawCommand[]): void {
    const w = Math.max(1, Math.round(vp.width * vp.dpr));
    const h = Math.max(1, Math.round(vp.height * vp.dpr));
    if (this.canvas.width !== w || this.canvas.height !== h) {
      this.canvas.width = w;
      this.canvas.height = h;
    }
    if (this.lost) {
      return;
    }
    const gl = this.gl;
    gl.viewport(0, 0, w, h);
    // Layers drawing with GL after terrain may have changed state.
    gl.disable(gl.BLEND);
    gl.disable(gl.SCISSOR_TEST);
    gl.clearColor(0, 0, 0, 1);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.useProgram(this.prog);
    gl.bindVertexArray(this.vao);
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, this.palTex);
    gl.activeTexture(gl.TEXTURE0);
    let mode = -1;
    for (const d of draws) {
      const t = d.tex as GLTexture;
      if (!t.tex) {
        continue;
      }
      const [x, y, dw, dh] = snapRect(d.dx, d.dy, d.dw, d.dh, vp.dpr);
      if (dw <= 0 || dh <= 0 || x >= w || y >= h || x + dw <= 0 || y + dh <= 0) {
        continue;
      }
      // Clip space: x right, y up.
      gl.uniform4f(this.uDst, (x / w) * 2 - 1, 1 - (y / h) * 2, (dw / w) * 2, -(dh / h) * 2);
      gl.uniform4f(this.uSrc, d.sx, d.sy, d.sx + d.sw, d.sy + d.sh);
      const m = t.format === FORMAT_RGB565 ? 1 : 0;
      if (m !== mode) {
        gl.uniform1i(this.uMode, m);
        mode = m;
      }
      gl.bindTexture(gl.TEXTURE_2D, t.tex);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    }
  }

  dispose(): void {
    for (const t of [...this.textures]) {
      this.deleteTexture(t);
    }
    this.canvas.removeEventListener("webglcontextlost", this.handleLost);
    this.canvas.removeEventListener("webglcontextrestored", this.handleRestored);
    this.gl.getExtension("WEBGL_lose_context")?.loseContext();
  }
}

/** Whether pixels of this format fit the renderer's texture types. */
export function glSupportsFormat(format: number): boolean {
  return format === FORMAT_PAL8 || format === FORMAT_PAL16 || format === FORMAT_RGB565;
}
