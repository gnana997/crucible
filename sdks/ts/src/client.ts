// The crucible client: a thin typed fetch wrapper over the daemon's REST
// API (the JSON contract is docs/openapi.json; the exec frame stream is
// docs/wire.md). Server-side runtimes only (Node ≥ 20, Bun, Deno) — a
// daemon bearer token grants control of the host's microVMs and must never
// ship to a browser.
import { FrameDecoder, FrameType } from "./frames.ts";
import { errorFrom } from "./errors.ts";
import type {
  CreateSandboxRequest,
  PortMapping,
  ErrorResponse,
  ExecRequest,
  ExecResult,
  FilesPutResult,
  ImageResponse,
  LogsResponse,
  Page,
  SandboxResponse,
  ServiceSpec,
  ServiceStatus,
  SnapshotResponse,
  Whoami,
} from "./types.ts";

export interface CrucibleOptions {
  /** e.g. "http://127.0.0.1:7878" (the daemon default). */
  baseUrl?: string;
  /** Daemon API key; omit against a keyless loopback daemon. */
  token?: string;
  /** Override fetch (tests, custom agents). */
  fetch?: typeof fetch;
}

export interface ExecHandlers {
  onStdout?: (chunk: Uint8Array) => void;
  onStderr?: (chunk: Uint8Array) => void;
}

export class Crucible {
  readonly #baseUrl: string;
  readonly #token: string;
  readonly #fetch: typeof fetch;

  constructor(opts: CrucibleOptions = {}) {
    this.#baseUrl = (opts.baseUrl ?? "http://127.0.0.1:7878").replace(/\/+$/, "");
    this.#token = opts.token ?? "";
    this.#fetch = opts.fetch ?? fetch;
  }

  // ---- meta -----------------------------------------------------------

  async health(): Promise<void> {
    await this.#req("GET", "/healthz");
  }

  async whoami(): Promise<Whoami> {
    return this.#json("GET", "/whoami");
  }

  async listProfiles(): Promise<string[]> {
    const out = await this.#json<{ profiles: string[] }>("GET", "/profiles");
    return out.profiles ?? [];
  }

  // ---- sandboxes ------------------------------------------------------

  async createSandbox(req: CreateSandboxRequest = {}): Promise<SandboxResponse> {
    return this.#json("POST", "/sandboxes", req);
  }

  async listSandboxes(): Promise<Page<SandboxResponse>> {
    const out = await this.#json<{ sandboxes: SandboxResponse[] }>("GET", "/sandboxes");
    return { items: out.sandboxes ?? [] };
  }

  async getSandbox(id: string): Promise<SandboxResponse> {
    return this.#json("GET", `/sandboxes/${encodeURIComponent(id)}`);
  }

  async deleteSandbox(id: string): Promise<void> {
    await this.#req("DELETE", `/sandboxes/${encodeURIComponent(id)}`);
  }

  /**
   * exec runs one command and streams its output: stdout/stderr chunks go
   * to the handlers as they arrive; the returned promise resolves with the
   * terminal ExecResult (exit code, usage). Interactive exec (live stdin)
   * is not implemented yet — it needs the WebSocket transport from
   * docs/wire.md and is a welcome contribution.
   */
  async exec(id: string, req: ExecRequest, handlers: ExecHandlers = {}): Promise<ExecResult> {
    const resp = await this.#req("POST", `/sandboxes/${encodeURIComponent(id)}/exec`, req);
    if (!resp.body) throw new Error("exec: response has no body stream");

    const dec = new FrameDecoder();
    let result: ExecResult | undefined;
    const reader = resp.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      for (const f of dec.push(value)) {
        switch (f.type) {
          case FrameType.stdout:
            handlers.onStdout?.(f.payload);
            break;
          case FrameType.stderr:
            handlers.onStderr?.(f.payload);
            break;
          case FrameType.exit:
            result = JSON.parse(new TextDecoder().decode(f.payload)) as ExecResult;
            break;
        }
      }
    }
    dec.end();
    if (!result) throw new Error("exec: stream ended without an exit frame");
    return result;
  }

  // ---- files ----------------------------------------------------------

  /** putFiles streams a tar archive to be extracted beneath dest in the guest. */
  async putFiles(id: string, dest: string, tar: BodyInit): Promise<FilesPutResult> {
    const path = `/sandboxes/${encodeURIComponent(id)}/files?path=${encodeURIComponent(dest)}`;
    return this.#json("POST", path, undefined, { body: tar, contentType: "application/x-tar" });
  }

  /** readFile returns one guest file's bytes, capped at maxBytes when > 0. */
  async readFile(id: string, path: string, maxBytes = 0): Promise<Uint8Array> {
    let p = `/sandboxes/${encodeURIComponent(id)}/files?path=${encodeURIComponent(path)}`;
    if (maxBytes > 0) p += `&max_bytes=${maxBytes}`;
    const resp = await this.#req("GET", p);
    return new Uint8Array(await resp.arrayBuffer());
  }

  // ---- logs -----------------------------------------------------------

  /** logs reads durable sandbox logs; since < 0 tails, source is "", "service" or "exec". */
  async logs(id: string, opts: { since?: number; source?: string } = {}): Promise<LogsResponse> {
    const q = new URLSearchParams();
    if (opts.since !== undefined && opts.since >= 0) q.set("since", String(opts.since));
    if (opts.source && opts.source !== "all") q.set("source", opts.source);
    const qs = q.size > 0 ? `?${q}` : "";
    return this.#json("GET", `/sandboxes/${encodeURIComponent(id)}/logs${qs}`);
  }

  // ---- snapshots & fork -------------------------------------------------

  async snapshot(sandboxID: string): Promise<SnapshotResponse> {
    return this.#json("POST", `/sandboxes/${encodeURIComponent(sandboxID)}/snapshot`);
  }

  async listSnapshots(): Promise<Page<SnapshotResponse>> {
    const out = await this.#json<{ snapshots: SnapshotResponse[] }>("GET", "/snapshots");
    return { items: out.snapshots ?? [] };
  }

  async getSnapshot(id: string): Promise<SnapshotResponse> {
    return this.#json("GET", `/snapshots/${encodeURIComponent(id)}`);
  }

  async deleteSnapshot(id: string): Promise<void> {
    await this.#req("DELETE", `/snapshots/${encodeURIComponent(id)}`);
  }

  /** fork creates count copies; publish (host→guest ports, count must be 1) needs daemon >= v0.3.4. */
  async fork(snapshotID: string, count = 1, publish?: PortMapping[]): Promise<SandboxResponse[]> {
    const path = `/snapshots/${encodeURIComponent(snapshotID)}/fork`;
    const out = publish?.length
      ? await this.#json<{ sandboxes: SandboxResponse[] }>("POST", path, { count, publish })
      : await this.#json<{ sandboxes: SandboxResponse[] }>("POST", `${path}?count=${count}`);
    return out.sandboxes ?? [];
  }

  // ---- images -----------------------------------------------------------

  async pullImage(ref: string): Promise<ImageResponse> {
    return this.#json("POST", "/images", { ref });
  }

  async listImages(): Promise<Page<ImageResponse>> {
    const out = await this.#json<{ images: ImageResponse[] }>("GET", "/images");
    return { items: out.images ?? [] };
  }

  async getImage(ref: string): Promise<ImageResponse> {
    return this.#json("GET", `/images/${encodeURIComponent(ref)}`);
  }

  async deleteImage(ref: string): Promise<void> {
    await this.#req("DELETE", `/images/${encodeURIComponent(ref)}`);
  }

  // ---- supervised service -------------------------------------------------

  async configureService(id: string, spec: ServiceSpec): Promise<ServiceStatus> {
    return this.#json("PUT", `/sandboxes/${encodeURIComponent(id)}/service`, spec);
  }

  async startService(id: string): Promise<ServiceStatus> {
    return this.#json("POST", `/sandboxes/${encodeURIComponent(id)}/service/start`);
  }

  async stopService(id: string, graceSec = 0): Promise<ServiceStatus> {
    const body = graceSec > 0 ? { grace_s: graceSec } : undefined;
    return this.#json("POST", `/sandboxes/${encodeURIComponent(id)}/service/stop`, body);
  }

  async restartService(id: string): Promise<ServiceStatus> {
    return this.#json("POST", `/sandboxes/${encodeURIComponent(id)}/service/restart`);
  }

  async serviceStatus(id: string): Promise<ServiceStatus> {
    return this.#json("GET", `/sandboxes/${encodeURIComponent(id)}/service`);
  }

  // ---- plumbing -----------------------------------------------------------

  async #json<T>(
    method: string,
    path: string,
    jsonBody?: unknown,
    raw?: { body: BodyInit; contentType: string },
  ): Promise<T> {
    const resp = await this.#req(method, path, jsonBody, raw);
    return (await resp.json()) as T;
  }

  async #req(
    method: string,
    path: string,
    jsonBody?: unknown,
    raw?: { body: BodyInit; contentType: string },
  ): Promise<Response> {
    const headers: Record<string, string> = {};
    if (this.#token) headers["Authorization"] = `Bearer ${this.#token}`;
    let body: BodyInit | undefined;
    if (raw) {
      headers["Content-Type"] = raw.contentType;
      body = raw.body;
    } else if (jsonBody !== undefined) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(jsonBody);
    }
    const resp = await this.#fetch(this.#baseUrl + path, {
      method,
      headers,
      body,
      // Streaming request bodies (putFiles with a ReadableStream) need
      // half-duplex mode on fetch implementations that support it.
      ...(raw && typeof raw.body === "object" && raw.body instanceof ReadableStream
        ? { duplex: "half" as const }
        : {}),
    });
    if (!resp.ok) {
      let message = resp.statusText;
      try {
        const e = (await resp.json()) as ErrorResponse;
        if (e.error) message = e.error;
      } catch {
        // non-JSON error body; keep statusText
      }
      throw errorFrom(resp.status, message);
    }
    return resp;
  }
}
